// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/mcp"
	"github.com/jwadow/kiro-gateway-go/internal/model"
	"github.com/jwadow/kiro-gateway-go/internal/protocol"
	"github.com/jwadow/kiro-gateway-go/internal/recovery"
	"github.com/jwadow/kiro-gateway-go/internal/tokenizer"
)

type Result struct {
	Content      string
	Thinking     string
	ToolCalls    []protocol.ToolCall
	Usage        any
	ContextUsage float64
	Truncated    bool
}
type FirstTokenTimeoutError struct{ Timeout time.Duration }

func (e *FirstTokenTimeoutError) Error() string {
	return fmt.Sprintf("no response within %s", e.Timeout)
}

type IdleTimeoutError struct{ Timeout time.Duration }

func (e *IdleTimeoutError) Error() string {
	return fmt.Sprintf("upstream stream idle for %s", e.Timeout)
}

// ProbeFirstEvent buffers through the first complete upstream protocol event.
// Callers replay the returned prefix with io.MultiReader before the remaining
// body, so no downstream response is committed before the timeout succeeds.
func ProbeFirstEvent(ctx context.Context, body io.Reader, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		return nil, nil
	}
	type readResult struct {
		data []byte
		err  error
	}
	results := make(chan readResult, 1)
	go func() {
		parser := protocol.NewParser()
		var prefix []byte
		buffer := make([]byte, 32<<10)
		for {
			n, err := body.Read(buffer)
			if n > 0 {
				chunk := append([]byte(nil), buffer[:n]...)
				prefix = append(prefix, chunk...)
				if len(parser.Feed(chunk)) > 0 {
					results <- readResult{data: prefix}
					return
				}
			}
			if err != nil {
				results <- readResult{data: prefix, err: err}
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, &FirstTokenTimeoutError{Timeout: timeout}
	case result := <-results:
		if result.err != nil {
			return result.data, result.err
		}
		return result.data, nil
	}
}

func Parse(ctx context.Context, body io.Reader, timeout time.Duration, handler func(protocol.Event) error) ([]protocol.ToolCall, error) {
	return ParseWithOptions(ctx, body, timeout, ParseOptions{}, handler)
}

type ParseOptions struct {
	ReasoningMode     string
	InitialBufferSize int
	IdleTimeout       time.Duration
}

func ParseWithOptions(ctx context.Context, body io.Reader, timeout time.Duration, opts ParseOptions, handler func(protocol.Event) error) ([]protocol.ToolCall, error) {
	parser := protocol.NewParser()
	thinking := recovery.NewThinkingParser(recovery.WithHandlingMode(opts.ReasoningMode), recovery.WithInitialBufferSize(opts.InitialBufferSize))
	emit := func(event protocol.Event) error {
		if event.Type != "content" {
			return handler(event)
		}
		parsed := thinking.Feed(event.Content)
		if parsed.ThinkingContent != "" {
			if text, ok := thinking.ProcessForOutput(parsed.ThinkingContent, parsed.IsFirstThinkingChunk, parsed.IsLastThinkingChunk); ok {
				typeName := "thinking"
				if thinking.HandlingMode == recovery.HandlingPass || thinking.HandlingMode == recovery.HandlingStripTags {
					typeName = "content"
				}
				out := protocol.Event{Type: typeName, Content: text, Thinking: text}
				if typeName == "content" {
					out.Thinking = ""
				}
				if err := handler(out); err != nil {
					return err
				}
			}
		}
		if parsed.RegularContent != "" {
			return handler(protocol.Event{Type: "content", Content: parsed.RegularContent})
		}
		return nil
	}
	type readResult struct {
		chunk []byte
		err   error
	}
	reads := make(chan readResult, 1)
	go func() {
		reader := bufio.NewReaderSize(body, 32<<10)
		for {
			chunk := make([]byte, 32<<10)
			n, err := reader.Read(chunk)
			result := readResult{err: err}
			if n > 0 {
				result.chunk = chunk[:n]
			}
			select {
			case reads <- result:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(timeout)
	if timeout <= 0 {
		if !timer.Stop() {
			<-timer.C
		}
	}
	defer timer.Stop()
	timerC := (<-chan time.Time)(nil)
	if timeout > 0 {
		timerC = timer.C
	}
	resetTimer := func(delay time.Duration) {
		if delay <= 0 {
			timerC = nil
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
		timerC = timer.C
	}
	first := true
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timerC:
			if first {
				return nil, &FirstTokenTimeoutError{timeout}
			}
			return nil, &IdleTimeoutError{opts.IdleTimeout}
		case result := <-reads:
			if len(result.chunk) > 0 {
				if !first {
					resetTimer(opts.IdleTimeout)
				}
				for _, event := range parser.Feed(result.chunk) {
					if first {
						first = false
						resetTimer(opts.IdleTimeout)
					}
					if err := emit(event); err != nil {
						return nil, err
					}
				}
				if err := parser.Err(); err != nil {
					return nil, err
				}
			}
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					final := thinking.Finalize()
					if final.ThinkingContent != "" {
						if text, ok := thinking.ProcessForOutput(final.ThinkingContent, final.IsFirstThinkingChunk, final.IsLastThinkingChunk); ok {
							typeName := "thinking"
							if thinking.HandlingMode == recovery.HandlingPass || thinking.HandlingMode == recovery.HandlingStripTags {
								typeName = "content"
							}
							out := protocol.Event{Type: typeName, Content: text, Thinking: text}
							if typeName == "content" {
								out.Thinking = ""
							}
							if handlerErr := handler(out); handlerErr != nil {
								return nil, handlerErr
							}
						}
					}
					if final.RegularContent != "" {
						if handlerErr := handler(protocol.Event{Type: "content", Content: final.RegularContent}); handlerErr != nil {
							return nil, handlerErr
						}
					}
					return parser.ToolCalls(), nil
				}
				return nil, result.err
			}
		}
	}
}
func Collect(ctx context.Context, body io.Reader, timeout time.Duration) (Result, error) {
	return CollectWithOptions(ctx, body, timeout, ParseOptions{})
}
func CollectWithOptions(ctx context.Context, body io.Reader, timeout time.Duration, opts ParseOptions) (Result, error) {
	var result Result
	calls, err := ParseWithOptions(ctx, body, timeout, opts, func(event protocol.Event) error {
		switch event.Type {
		case "content":
			result.Content += event.Content
		case "thinking":
			result.Thinking += event.Thinking
		case "usage":
			result.Usage = event.Usage
		case "context_usage":
			result.ContextUsage = event.ContextUsage
		}
		return nil
	})
	result.ToolCalls = protocol.Deduplicate(append(calls, protocol.ParseBracketToolCalls(result.Content+result.Thinking)...))
	result.Truncated = result.Content != "" && result.Usage == nil && result.ContextUsage == 0 && len(result.ToolCalls) == 0
	if result.Truncated {
		recovery.SaveContentTruncation(result.Content)
	}
	return result, err
}

type WebSearchFunc func(context.Context, string) (mcp.Response, error)

type OpenAIOptions struct {
	Model             string
	PromptTokens      int
	ModelCache        *model.Cache
	ReasoningMode     string
	InitialBufferSize int
	IdleTimeout       time.Duration
	WebSearch         WebSearchFunc
}

func OpenAI(ctx context.Context, w http.ResponseWriter, body io.Reader, timeout time.Duration, opts OpenAIOptions) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()
	first := true
	completionText := ""
	var usage any
	var contextUsage float64
	calls, err := ParseWithOptions(ctx, body, timeout, ParseOptions{ReasoningMode: opts.ReasoningMode, InitialBufferSize: opts.InitialBufferSize, IdleTimeout: opts.IdleTimeout}, func(event protocol.Event) error {
		switch event.Type {
		case "content", "thinking":
			text := event.Content
			field := "content"
			if event.Type == "thinking" {
				text = event.Thinking
				if opts.ReasoningMode == "as_reasoning_content" {
					field = "reasoning_content"
				}
			}
			completionText += text
			delta := map[string]any{field: text}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			return writeOpenAIChunk(w, flusher, id, created, opts.Model, delta, nil)
		case "usage":
			usage = event.Usage
		case "context_usage":
			contextUsage = event.ContextUsage
		}
		return nil
	})
	if err != nil {
		if writeErr := writeOpenAIStreamError(w, flusher, err); writeErr != nil {
			return writeErr
		}
		return err
	}
	calls, searches, err := interceptWebSearch(ctx, calls, opts.WebSearch)
	if err != nil {
		if writeErr := writeOpenAIStreamError(w, flusher, err); writeErr != nil {
			return writeErr
		}
		return nil
	}
	for _, search := range searches {
		completionText += search.Summary
		for _, chunk := range textChunks(search.Summary, 100) {
			delta := map[string]any{"content": chunk}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			if err := writeOpenAIChunk(w, flusher, id, created, opts.Model, delta, nil); err != nil {
				return err
			}
		}
	}
	for index, call := range calls {
		delta := map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": call.ID, "type": "function", "function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments}}}}
		if first {
			delta["role"] = "assistant"
			first = false
		}
		if err := writeOpenAIChunk(w, flusher, id, created, opts.Model, delta, nil); err != nil {
			return err
		}
	}
	truncated := completionText != "" && usage == nil && contextUsage == 0 && len(calls) == 0
	if truncated {
		recovery.SaveContentTruncation(completionText)
	}
	finish := "stop"
	if truncated {
		finish = "length"
	} else if len(calls) > 0 {
		finish = "tool_calls"
	}
	completionTokens := tokenizer.CountTokens(completionText)
	promptTokens, totalTokens := tokenizer.UsageFromContext(contextUsage, completionTokens, opts.PromptTokens, opts.ModelCache, opts.Model)
	usagePayload := map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": totalTokens}
	if err := writeOpenAIChunkWithUsage(w, flusher, id, created, opts.Model, map[string]any{}, finish, usagePayload); err != nil {
		return err
	}
	_, err = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	_ = completionText
	_ = usage
	return err
}
func writeOpenAIStreamError(w io.Writer, f http.Flusher, err error) error {
	value := map[string]any{"error": map[string]any{"message": err.Error(), "type": "api_error", "code": "upstream_error"}}
	data, _ := json.Marshal(value)
	if _, writeErr := fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data); writeErr != nil {
		return writeErr
	}
	f.Flush()
	return nil
}

func writeOpenAIChunk(w io.Writer, f http.Flusher, id string, created int64, model string, delta map[string]any, finish any) error {
	return writeOpenAIChunkWithUsage(w, f, id, created, model, delta, finish, nil)
}

func writeOpenAIChunkWithUsage(w io.Writer, f http.Flusher, id string, created int64, model string, delta map[string]any, finish any, usage map[string]any) error {
	value := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usage != nil {
		value["usage"] = usage
	}
	data, _ := json.Marshal(value)
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	f.Flush()
	return nil
}
func OpenAIResponse(result Result, modelID string, promptTokens int, caches ...*model.Cache) map[string]any {
	content := result.Content
	message := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if result.Thinking != "" {
		message["reasoning_content"] = result.Thinking
	}
	if len(result.ToolCalls) > 0 {
		message["tool_calls"] = result.ToolCalls
	}
	if result.Truncated {
		finish = "length"
	} else if len(result.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	completion := tokenizer.CountTokens(content + result.Thinking)
	var cache *model.Cache
	if len(caches) > 0 {
		cache = caches[0]
	}
	promptTokens, totalTokens := tokenizer.UsageFromContext(result.ContextUsage, completion, promptTokens, cache, modelID)
	usage := map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completion, "total_tokens": totalTokens}
	if result.Usage != nil {
		usage["credits_used"] = result.Usage
	}
	return map[string]any{"id": "chatcmpl-" + randomID(), "object": "chat.completion", "created": time.Now().Unix(), "model": modelID, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage}
}

const maxStreamingRecoveryBytes = 1 << 20

func appendBounded(dst, value string) string {
	if len(dst) >= maxStreamingRecoveryBytes {
		return dst
	}
	remaining := maxStreamingRecoveryBytes - len(dst)
	if len(value) > remaining {
		value = value[:remaining]
	}
	return dst + value
}

type AnthropicOptions struct {
	Model             string
	InputTokens       int
	ModelCache        *model.Cache
	ReasoningMode     string
	InitialBufferSize int
	IdleTimeout       time.Duration
	WebSearch         WebSearchFunc
}

func Anthropic(ctx context.Context, w http.ResponseWriter, body io.Reader, timeout time.Duration, opts AnthropicOptions) error {
	f, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	id := "msg_" + randomID()
	startUsage := map[string]any{"input_tokens": opts.InputTokens, "output_tokens": 0}
	start := map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": opts.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": startUsage}}
	writeAnthropicEvent(w, f, "message_start", start)

	retainedOutput := ""
	outputRunes := 0
	var usage any
	var contextUsage float64
	index := 0
	activeKind := ""
	closeActive := func() {
		if activeKind != "" {
			writeAnthropicEvent(w, f, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			index++
			activeKind = ""
		}
	}
	emitText := func(kind, text string) {
		if activeKind != kind {
			closeActive()
			field := "text"
			if kind == "thinking" {
				field = "thinking"
			}
			block := map[string]any{"type": kind, field: ""}
			if kind == "thinking" {
				block["signature"] = ""
			}
			writeAnthropicEvent(w, f, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
			activeKind = kind
		}
		deltaType, field := "text_delta", "text"
		if kind == "thinking" {
			deltaType, field = "thinking_delta", "thinking"
		}
		writeAnthropicEvent(w, f, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": deltaType, field: text}})
	}
	calls, err := ParseWithOptions(ctx, body, timeout, ParseOptions{ReasoningMode: opts.ReasoningMode, InitialBufferSize: opts.InitialBufferSize, IdleTimeout: opts.IdleTimeout}, func(event protocol.Event) error {
		switch event.Type {
		case "usage":
			usage = event.Usage
		case "context_usage":
			contextUsage = event.ContextUsage
		case "content":
			outputRunes += len([]rune(event.Content))
			retainedOutput = appendBounded(retainedOutput, event.Content)
			emitText("text", event.Content)
		case "thinking":
			outputRunes += len([]rune(event.Thinking))
			retainedOutput = appendBounded(retainedOutput, event.Thinking)
			emitText("thinking", event.Thinking)
		}
		return nil
	})
	closeActive()
	if err != nil {
		writeAnthropicEvent(w, f, "error", anthropicStreamError(err))
		return err
	}
	calls, searches, _ := interceptWebSearch(ctx, calls, opts.WebSearch)
	for _, search := range searches {
		writeAnthropicSearchBlocks(w, f, &index, search)
		outputRunes += len([]rune(search.Summary))
		retainedOutput = appendBounded(retainedOutput, search.Summary)
	}
	for _, call := range calls {
		writeAnthropicEvent(w, f, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": map[string]any{}}})
		writeAnthropicEvent(w, f, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments}})
		writeAnthropicEvent(w, f, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	truncated := outputRunes > 0 && usage == nil && contextUsage == 0 && len(calls) == 0
	if truncated && outputRunes <= len([]rune(retainedOutput)) {
		recovery.SaveContentTruncation(retainedOutput)
	}
	stop := "end_turn"
	if truncated {
		stop = "max_tokens"
	} else if len(calls) > 0 {
		stop = "tool_use"
	}
	outputTokens := tokenizer.CountTokensFromRunes(outputRunes)
	inputTokens, _ := tokenizer.UsageFromContext(contextUsage, outputTokens, opts.InputTokens, opts.ModelCache, opts.Model)
	deltaUsage := map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}
	copyAnthropicCacheUsage(deltaUsage, usage)
	writeAnthropicEvent(w, f, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": deltaUsage})
	writeAnthropicEvent(w, f, "message_stop", map[string]any{"type": "message_stop"})
	return nil
}

type SearchResult struct {
	Query    string
	Response mcp.Response
	Summary  string
}

func InterceptWebSearch(ctx context.Context, result Result, search WebSearchFunc) (Result, []SearchResult, error) {
	calls, searches, err := interceptWebSearch(ctx, result.ToolCalls, search)
	if err != nil {
		return result, nil, err
	}
	result.ToolCalls = calls
	for _, item := range searches {
		result.Content += item.Summary
	}
	return result, searches, nil
}

func interceptWebSearch(ctx context.Context, calls []protocol.ToolCall, search WebSearchFunc) ([]protocol.ToolCall, []SearchResult, error) {
	if search == nil {
		return calls, nil, nil
	}
	regular := make([]protocol.ToolCall, 0, len(calls))
	var searches []SearchResult
	for _, call := range calls {
		if call.Function.Name != "web_search" {
			regular = append(regular, call)
			continue
		}
		var input struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil || strings.TrimSpace(input.Query) == "" {
			regular = append(regular, call)
			continue
		}
		response, err := search(ctx, input.Query)
		if err != nil {
			regular = append(regular, call)
			continue
		}
		searches = append(searches, SearchResult{Query: input.Query, Response: response, Summary: mcp.Summary(input.Query, response.Results)})
	}
	return regular, searches, nil
}

func writeAnthropicSearchBlocks(w io.Writer, f http.Flusher, index *int, search SearchResult) {
	writeAnthropicEvent(w, f, "content_block_start", map[string]any{"type": "content_block_start", "index": *index, "content_block": map[string]any{"id": search.Response.ToolUseID, "type": "server_tool_use", "name": "web_search", "input": map[string]any{}}})
	input, _ := json.Marshal(map[string]any{"query": search.Query})
	writeAnthropicEvent(w, f, "content_block_delta", map[string]any{"type": "content_block_delta", "index": *index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	writeAnthropicEvent(w, f, "content_block_stop", map[string]any{"type": "content_block_stop", "index": *index})
	(*index)++
	writeAnthropicEvent(w, f, "content_block_start", map[string]any{"type": "content_block_start", "index": *index, "content_block": map[string]any{"type": "web_search_tool_result", "tool_use_id": search.Response.ToolUseID, "content": mcp.ResultContent(search.Response.Results)}})
	writeAnthropicEvent(w, f, "content_block_stop", map[string]any{"type": "content_block_stop", "index": *index})
	(*index)++
	writeAnthropicEvent(w, f, "content_block_start", map[string]any{"type": "content_block_start", "index": *index, "content_block": map[string]any{"type": "text", "text": ""}})
	for _, chunk := range textChunks(search.Summary, 100) {
		writeAnthropicEvent(w, f, "content_block_delta", map[string]any{"type": "content_block_delta", "index": *index, "delta": map[string]any{"type": "text_delta", "text": chunk}})
	}
	writeAnthropicEvent(w, f, "content_block_stop", map[string]any{"type": "content_block_stop", "index": *index})
	(*index)++
}

func textChunks(text string, size int) []string {
	if text == "" || size <= 0 {
		return nil
	}
	runes := []rune(text)
	chunks := make([]string, 0, (len(runes)+size-1)/size)
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

func AnthropicWebSearch(w http.ResponseWriter, model string, inputTokens int, search SearchResult) error {
	f, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	start := map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_" + randomID(), "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0}}}
	writeAnthropicEvent(w, f, "message_start", start)
	index := 0
	writeAnthropicSearchBlocks(w, f, &index, search)
	writeAnthropicEvent(w, f, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": estimateTokens(search.Summary)}})
	writeAnthropicEvent(w, f, "message_stop", map[string]any{"type": "message_stop"})
	return nil
}

func AnthropicResponse(result Result, modelID string, inputTokens int, caches ...*model.Cache) map[string]any {
	return AnthropicResponseWithSearches(result, nil, modelID, inputTokens, caches...)
}

func AnthropicResponseWithSearches(result Result, searches []SearchResult, modelID string, inputTokens int, caches ...*model.Cache) map[string]any {
	content := []any{}
	if result.Thinking != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": result.Thinking, "signature": ""})
	}
	for _, search := range searches {
		content = append(content,
			map[string]any{"type": "server_tool_use", "id": search.Response.ToolUseID, "name": "web_search", "input": map[string]any{"query": search.Query}},
			map[string]any{"type": "web_search_tool_result", "tool_use_id": search.Response.ToolUseID, "content": mcp.ResultContent(search.Response.Results)},
		)
	}
	if result.Content != "" || len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": result.Content})
	}
	for _, call := range result.ToolCalls {
		var input any = map[string]any{}
		_ = json.Unmarshal([]byte(call.Function.Arguments), &input)
		content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
	}
	stop := "end_turn"
	if result.Truncated {
		stop = "max_tokens"
	} else if len(result.ToolCalls) > 0 {
		stop = "tool_use"
	}
	outputTokens := tokenizer.CountTokens(result.Content + result.Thinking)
	var cache *model.Cache
	if len(caches) > 0 {
		cache = caches[0]
	}
	inputTokens, _ = tokenizer.UsageFromContext(result.ContextUsage, outputTokens, inputTokens, cache, modelID)
	usage := map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}
	copyAnthropicCacheUsage(usage, result.Usage)
	return map[string]any{"id": "msg_" + randomID(), "type": "message", "role": "assistant", "content": content, "model": modelID, "stop_reason": stop, "stop_sequence": nil, "usage": usage}
}

func copyAnthropicCacheUsage(dst map[string]any, upstream any) {
	values, ok := upstream.(map[string]any)
	if !ok {
		return
	}
	for _, keys := range [][2]string{{"cacheReadInputTokens", "cache_read_input_tokens"}, {"cache_read_input_tokens", "cache_read_input_tokens"}, {"cacheCreationInputTokens", "cache_creation_input_tokens"}, {"cache_creation_input_tokens", "cache_creation_input_tokens"}} {
		if value, exists := values[keys[0]]; exists {
			switch value.(type) {
			case int, int32, int64, float32, float64, json.Number:
				dst[keys[1]] = value
			}
		}
	}
}

func anthropicStreamError(err error) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}}
}

func writeAnthropicEvent(w io.Writer, f http.Flusher, name string, value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	f.Flush()
}
func estimateTokens(text string) int {
	runes := len([]rune(text))
	if runes == 0 {
		return 0
	}
	return (runes + 3) / 4
}
func randomID() string { return strings.ReplaceAll(fmt.Sprintf("%d", time.Now().UnixNano()), "-", "") }
