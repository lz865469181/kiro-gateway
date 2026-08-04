package stream

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/mcp"
	"github.com/jwadow/kiro-gateway-go/internal/model"
	"github.com/jwadow/kiro-gateway-go/internal/recovery"
)

type startAwareWriter struct {
	httptest.ResponseRecorder
	started chan struct{}
	once    sync.Once
}

func (w *startAwareWriter) Flush() {
	if strings.Contains(w.Body.String(), "message_start") {
		w.once.Do(func() { close(w.started) })
	}
}

type waitForStartReader struct{ started <-chan struct{} }

func (r waitForStartReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
		return copy(p, []byte(`{"content":"incremental"}`)), io.EOF
	case <-time.After(time.Second):
		return 0, errors.New("upstream read began before message_start was flushed")
	}
}

func TestAnthropicStartsBeforeReadingUpstream(t *testing.T) {
	w := &startAwareWriter{ResponseRecorder: *httptest.NewRecorder(), started: make(chan struct{})}
	if err := Anthropic(context.Background(), w, waitForStartReader{started: w.started}, time.Second, AnthropicOptions{Model: "test"}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if strings.Index(body, "message_start") > strings.Index(body, "incremental") {
		t.Fatalf("event order invalid: %s", body)
	}
}

func TestCollect(t *testing.T) {
	raw := `noise{"content":"Hello "}{"content":"world"}{"usage":1.5}{"name":"read","toolUseId":"call_1"}{"input":"{\"path\":\"x\"}"}{"stop":true}`
	result, err := Collect(context.Background(), strings.NewReader(raw), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "Hello world" || len(result.ToolCalls) != 1 {
		t.Fatalf("result=%+v", result)
	}
}
func TestOpenAIStream(t *testing.T) {
	raw := `{"content":"Hello"}`
	w := httptest.NewRecorder()
	if err := OpenAI(context.Background(), w, strings.NewReader(raw), time.Second, OpenAIOptions{Model: "test"}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("body=%s", body)
	}
}
func TestAnthropicStream(t *testing.T) {
	raw := `{"content":"Hello"}`
	w := httptest.NewRecorder()
	if err := Anthropic(context.Background(), w, strings.NewReader(raw), time.Second, AnthropicOptions{Model: "test"}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", `"stop_reason":"max_tokens"`} {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %s in %s", event, body)
		}
	}
}
func TestContextUsageTokenAccounting(t *testing.T) {
	cache := model.NewCache(time.Hour, 200000)
	cache.Update([]model.Info{{ModelID: "test", TokenLimits: map[string]any{"maxInputTokens": 1000}}})
	raw := `{"content":"12345678"}{"contextUsagePercentage":25}`

	openAI := httptest.NewRecorder()
	if err := OpenAI(context.Background(), openAI, strings.NewReader(raw), time.Second, OpenAIOptions{Model: "test", PromptTokens: 7, ModelCache: cache}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"prompt_tokens":247`, `"completion_tokens":3`, `"total_tokens":250`} {
		if !strings.Contains(openAI.Body.String(), want) {
			t.Fatalf("OpenAI stream missing %s in %s", want, openAI.Body.String())
		}
	}

	anthropic := httptest.NewRecorder()
	if err := Anthropic(context.Background(), anthropic, strings.NewReader(raw), time.Second, AnthropicOptions{Model: "test", InputTokens: 7, ModelCache: cache}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"input_tokens":247`, `"output_tokens":3`} {
		if !strings.Contains(anthropic.Body.String(), want) {
			t.Fatalf("Anthropic stream missing %s in %s", want, anthropic.Body.String())
		}
	}

	result, err := Collect(context.Background(), strings.NewReader(raw), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	openAIResponse := OpenAIResponse(result, "test", 7, cache)
	usage := openAIResponse["usage"].(map[string]any)
	if usage["prompt_tokens"] != 247 || usage["completion_tokens"] != 3 || usage["total_tokens"] != 250 {
		t.Fatalf("OpenAI response usage=%#v", usage)
	}
	anthropicResponse := AnthropicResponse(result, "test", 7, cache)
	anthropicUsage := anthropicResponse["usage"].(map[string]any)
	if anthropicUsage["input_tokens"] != 247 || anthropicUsage["output_tokens"] != 3 {
		t.Fatalf("Anthropic response usage=%#v", anthropicUsage)
	}
}

func TestThinkingModesAndAnthropicChunks(t *testing.T) {
	raw := `{"content":"<thinking>plan</thinking>answer"}{"usage":1}`
	result, err := CollectWithOptions(context.Background(), strings.NewReader(raw), time.Second, ParseOptions{ReasoningMode: recovery.HandlingAsReasoningContent, InitialBufferSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.Thinking != "plan" || result.Content != "answer" {
		t.Fatalf("result=%+v", result)
	}

	w := httptest.NewRecorder()
	if err := Anthropic(context.Background(), w, strings.NewReader(raw), time.Second, AnthropicOptions{Model: "test", ReasoningMode: recovery.HandlingAsReasoningContent, InitialBufferSize: 20}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"thinking_delta"`) || !strings.Contains(body, `"type":"text_delta"`) || strings.Count(body, "content_block_start") < 4 {
		t.Fatalf("thinking/text blocks missing: %s", body)
	}

	removed, err := CollectWithOptions(context.Background(), strings.NewReader(raw), time.Second, ParseOptions{ReasoningMode: recovery.HandlingRemove, InitialBufferSize: 20})
	if err != nil || removed.Thinking != "" || removed.Content != "answer" {
		t.Fatalf("removed=%+v err=%v", removed, err)
	}
	passed, err := CollectWithOptions(context.Background(), strings.NewReader(raw), time.Second, ParseOptions{ReasoningMode: recovery.HandlingPass, InitialBufferSize: 20})
	if err != nil || passed.Content != "<thinking>plan</thinking>answer" {
		t.Fatalf("passed=%+v err=%v", passed, err)
	}
}

func TestTruncationSavedFromIncompleteContent(t *testing.T) {
	content := "incomplete response"
	result, err := Collect(context.Background(), strings.NewReader(`{"content":"`+content+`"}`), time.Second)
	if err != nil || !result.Truncated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, ok := recovery.GetContentTruncation(content); !ok {
		t.Fatal("content truncation was not stored")
	}
	openAI := OpenAIResponse(result, "test", 1)
	choice := openAI["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("OpenAI finish_reason=%v", choice["finish_reason"])
	}
	anthropic := AnthropicResponse(result, "test", 1)
	if anthropic["stop_reason"] != "max_tokens" {
		t.Fatalf("Anthropic stop_reason=%v", anthropic["stop_reason"])
	}

	openAIStream := httptest.NewRecorder()
	if err := OpenAI(context.Background(), openAIStream, strings.NewReader(`{"content":"`+content+`"}`), time.Second, OpenAIOptions{Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(openAIStream.Body.String(), `"finish_reason":"length"`) {
		t.Fatalf("OpenAI stream=%s", openAIStream.Body.String())
	}
}

func TestOpenAIInterceptsWebSearchAndPreservesRegularTools(t *testing.T) {
	raw := `{"name":"web_search","toolUseId":"call_search"}{"input":"{\"query\":\"Go news\"}"}{"stop":true}{"name":"read","toolUseId":"call_read"}{"input":"{\"path\":\"x\"}"}{"stop":true}{"usage":1}`
	w := httptest.NewRecorder()
	search := func(_ context.Context, query string) (mcp.Response, error) {
		if query != "Go news" {
			t.Fatalf("query=%q", query)
		}
		return mcp.Response{ToolUseID: "srvtoolu_test", Results: map[string]any{"results": []any{map[string]any{"title": "Go", "url": "https://go.dev", "snippet": "News"}}}}, nil
	}
	if err := OpenAI(context.Background(), w, strings.NewReader(raw), time.Second, OpenAIOptions{Model: "test", PromptTokens: 12, WebSearch: search}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if !strings.Contains(body, `Search results for \"Go news\"`) || strings.Contains(body, `"name":"web_search"`) || !strings.Contains(body, `"name":"read"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) || !strings.Contains(body, `"prompt_tokens":12`) {
		t.Fatalf("body=%s", body)
	}
}

func TestAnthropicWebSearchProtocolBlocks(t *testing.T) {
	w := httptest.NewRecorder()
	result := SearchResult{Query: "Go", Summary: "summary", Response: mcp.Response{ToolUseID: "srvtoolu_test", Results: map[string]any{"results": []any{map[string]any{"title": "Go", "url": "https://go.dev", "snippet": "News"}}}}}
	if err := AnthropicWebSearch(w, "test", 7, result); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	for _, want := range []string{`"type":"server_tool_use"`, `"type":"input_json_delta"`, `"type":"web_search_tool_result"`, `"type":"web_search_result"`, `"tool_use_id":"srvtoolu_test"`, `"input_tokens":7`, `"output_tokens":2`, "message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
}

func TestAnthropicToolUseStreamsInputJSON(t *testing.T) {
	raw := `{"name":"read","toolUseId":"call_1"}{"input":"{\"path\":\"x\"}"}{"stop":true}{"usage":1}`
	w := httptest.NewRecorder()
	if err := Anthropic(context.Background(), w, strings.NewReader(raw), time.Second, AnthropicOptions{Model: "test"}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	for _, want := range []string{`"id":"call_1","input":{},"name":"read","type":"tool_use"`, `"type":"input_json_delta"`, `"partial_json":"{\"path\":\"x\"}"`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	start := strings.Index(body, `"type":"tool_use"`)
	delta := strings.Index(body, `"type":"input_json_delta"`)
	stop := strings.Index(body[delta:], "event: content_block_stop")
	if start < 0 || delta < start || stop < 0 {
		t.Fatalf("tool event order invalid: %s", body)
	}
}

func TestAnthropicCacheUsageForwarded(t *testing.T) {
	for _, raw := range []string{
		`{"content":"ok"}{"usage":{"cacheReadInputTokens":12,"cacheCreationInputTokens":34}}`,
		`{"content":"ok"}{"usage":{"cache_read_input_tokens":12,"cache_creation_input_tokens":34}}`,
	} {
		w := httptest.NewRecorder()
		if err := Anthropic(context.Background(), w, strings.NewReader(raw), time.Second, AnthropicOptions{Model: "test"}); err != nil {
			t.Fatal(err)
		}
		body := w.Body.String()
		if strings.Count(body, `"cache_read_input_tokens":12`) != 1 || strings.Count(body, `"cache_creation_input_tokens":34`) != 1 {
			t.Fatalf("cache usage missing from message_delta: %s", body)
		}
		result, err := Collect(context.Background(), strings.NewReader(raw), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		usage := AnthropicResponse(result, "test", 1)["usage"].(map[string]any)
		if usage["cache_read_input_tokens"] != float64(12) || usage["cache_creation_input_tokens"] != float64(34) {
			t.Fatalf("usage=%#v", usage)
		}
	}
}

type errorAfterChunkReader struct {
	chunk []byte
	read  bool
}

func (r *errorAfterChunkReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errors.New("upstream read failed")
	}
	r.read = true
	return copy(p, r.chunk), nil
}

func TestMidStreamErrorsUseProtocolPayloads(t *testing.T) {
	openAI := httptest.NewRecorder()
	if err := OpenAI(context.Background(), openAI, &errorAfterChunkReader{chunk: []byte(`{"content":"first"}`)}, time.Second, OpenAIOptions{Model: "test"}); err == nil || err.Error() != "upstream read failed" {
		t.Fatalf("err=%v", err)
	}
	openAIBody := openAI.Body.String()
	if !strings.Contains(openAIBody, `"content":"first"`) || !strings.Contains(openAIBody, `"error":{"code":"upstream_error","message":"upstream read failed","type":"api_error"}`) || !strings.HasSuffix(openAIBody, "data: [DONE]\n\n") {
		t.Fatalf("OpenAI body=%s", openAIBody)
	}

	anthropic := httptest.NewRecorder()
	if err := Anthropic(context.Background(), anthropic, &errorAfterChunkReader{chunk: []byte(`{"content":"first"}`)}, time.Second, AnthropicOptions{Model: "test"}); err == nil || err.Error() != "upstream read failed" {
		t.Fatalf("err=%v", err)
	}
	anthropicBody := anthropic.Body.String()
	if !strings.Contains(anthropicBody, `"text":"first"`) || !strings.Contains(anthropicBody, "event: error") || !strings.Contains(anthropicBody, `"message":"upstream read failed"`) || strings.Contains(anthropicBody, "event: message_stop") {
		t.Fatalf("Anthropic body=%s", anthropicBody)
	}
}

func TestInterceptWebSearchFailurePreservesOriginalToolCall(t *testing.T) {
	result, err := Collect(context.Background(), strings.NewReader(`{"name":"web_search","toolUseId":"call"}{"input":"{\"query\":\"Go\"}"}{"stop":true}`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, searches, err := InterceptWebSearch(context.Background(), result, func(context.Context, string) (mcp.Response, error) { return mcp.Response{}, errors.New("unavailable") })
	if err != nil || len(searches) != 0 || len(got.ToolCalls) != 1 || got.ToolCalls[0].Function.Name != "web_search" {
		t.Fatalf("result=%+v searches=%+v err=%v", got, searches, err)
	}
}

func TestFirstTokenTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	_, err := Collect(context.Background(), reader, 5*time.Millisecond)
	var timeout *FirstTokenTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err=%v", err)
	}
}

func TestIdleTimeoutAfterFirstEvent(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		_, _ = writer.Write([]byte(`{"content":"first"}`))
		defer writer.Close()
		time.Sleep(time.Second)
	}()
	result, err := CollectWithOptions(context.Background(), reader, time.Second, ParseOptions{IdleTimeout: 10 * time.Millisecond})
	var timeout *IdleTimeoutError
	if !errors.As(err, &timeout) || result.Content != "first" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
