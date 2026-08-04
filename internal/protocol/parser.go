// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jwadow/kiro-gateway-go/internal/recovery"
)

type Event struct {
	Type         string
	Content      string
	Thinking     string
	ToolCall     *ToolCall
	Usage        any
	ContextUsage float64
}
type ToolCall struct {
	ID               string       `json:"id"`
	Type             string       `json:"type"`
	Function         ToolFunction `json:"function"`
	Truncated        bool         `json:"-"`
	TruncationReason string       `json:"-"`
}
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

const MaxBufferBytes = 8 << 20

type Parser struct {
	buffer      string
	lastContent string
	current     *ToolCall
	calls       []ToolCall
	err         error
}

func NewParser() *Parser { return &Parser{} }

var patterns = []struct{ prefix, kind string }{{`{"content":`, "content"}, {`{"name":`, "tool_start"}, {`{"input":`, "tool_input"}, {`{"stop":`, "tool_stop"}, {`{"followupPrompt":`, "followup"}, {`{"usage":`, "usage"}, {`{"contextUsagePercentage":`, "context_usage"}}

func (p *Parser) Feed(chunk []byte) []Event {
	if p.err != nil {
		return nil
	}
	p.buffer += strings.ToValidUTF8(string(chunk), "")
	if len(p.buffer) > MaxBufferBytes {
		p.err = fmt.Errorf("Kiro event buffer exceeds %d bytes", MaxBufferBytes)
		p.buffer = ""
		return nil
	}
	var events []Event
	for {
		pos := -1
		kind := ""
		for _, pat := range patterns {
			if i := strings.Index(p.buffer, pat.prefix); i >= 0 && (pos < 0 || i < pos) {
				pos = i
				kind = pat.kind
			}
		}
		if pos < 0 {
			break
		}
		end := MatchingBrace(p.buffer, pos)
		if end < 0 {
			break
		}
		raw := p.buffer[pos : end+1]
		p.buffer = p.buffer[end+1:]
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) != nil {
			continue
		}
		if event, ok := p.process(data, kind); ok {
			events = append(events, event)
		}
	}
	return events
}
func MatchingBrace(text string, start int) int {
	if start < 0 || start >= len(text) || text[start] != '{' {
		return -1
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if !inString {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}
func (p *Parser) process(data map[string]any, kind string) (Event, bool) {
	switch kind {
	case "content":
		if _, ok := data["followupPrompt"]; ok {
			return Event{}, false
		}
		content, _ := data["content"].(string)
		if content == p.lastContent {
			return Event{}, false
		}
		p.lastContent = content
		return Event{Type: "content", Content: content}, true
	case "usage":
		return Event{Type: "usage", Usage: data["usage"]}, true
	case "context_usage":
		v, _ := data["contextUsagePercentage"].(float64)
		return Event{Type: "context_usage", ContextUsage: v}, true
	case "tool_start":
		if p.current != nil {
			p.finalize()
		}
		id, _ := data["toolUseId"].(string)
		name, _ := data["name"].(string)
		if id == "" {
			id = newToolID()
		}
		p.current = &ToolCall{ID: id, Type: "function", Function: ToolFunction{Name: name, Arguments: inputString(data["input"])}}
		if stop, _ := data["stop"].(bool); stop {
			p.finalize()
		}
	case "tool_input":
		if p.current != nil {
			p.current.Function.Arguments += inputString(data["input"])
		}
	case "tool_stop":
		if stop, _ := data["stop"].(bool); stop && p.current != nil {
			p.finalize()
		}
	}
	return Event{}, false
}
func inputString(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case map[string]any:
		if len(value) == 0 {
			return ""
		}
		b, _ := json.Marshal(value)
		return string(b)
	default:
		return fmt.Sprint(value)
	}
}
func (p *Parser) finalize() {
	if p.current == nil {
		return
	}
	args := strings.TrimSpace(p.current.Function.Arguments)
	if args == "" {
		p.current.Function.Arguments = "{}"
	} else {
		var value any
		if json.Unmarshal([]byte(args), &value) != nil {
			p.current.Truncated, p.current.TruncationReason = diagnoseTruncation(args)
			if p.current.Truncated {
				partial := args
				if len(partial) > 4096 {
					partial = partial[:4096]
				}
				recovery.SaveToolTruncation(p.current.ID, p.current.Function.Name, map[string]any{"reason": p.current.TruncationReason, "partial_arguments": partial, "size_bytes": len(args)})
			}
			p.current.Function.Arguments = "{}"
		} else {
			b, _ := json.Marshal(value)
			p.current.Function.Arguments = string(b)
		}
	}
	p.calls = append(p.calls, *p.current)
	p.current = nil
}
func (p *Parser) Err() error { return p.err }

func (p *Parser) ToolCalls() []ToolCall {
	if p.current != nil {
		p.finalize()
	}
	return Deduplicate(p.calls)
}
func Deduplicate(calls []ToolCall) []ToolCall {
	byID := map[string]ToolCall{}
	var noID []ToolCall
	var order []string
	for _, c := range calls {
		if c.ID == "" {
			noID = append(noID, c)
			continue
		}
		old, ok := byID[c.ID]
		if !ok {
			byID[c.ID] = c
			order = append(order, c.ID)
		} else if c.Function.Arguments != "{}" && (old.Function.Arguments == "{}" || len(c.Function.Arguments) > len(old.Function.Arguments)) {
			byID[c.ID] = c
		}
	}
	all := make([]ToolCall, 0, len(calls))
	for _, id := range order {
		all = append(all, byID[id])
	}
	all = append(all, noID...)
	seen := map[string]struct{}{}
	out := all[:0]
	for _, c := range all {
		key := c.Function.Name + "-" + c.Function.Arguments
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}
func diagnoseTruncation(s string) (bool, string) {
	trim := strings.TrimSpace(s)
	if trim == "" {
		return false, "empty string"
	}
	if trim[0] == '{' && !strings.HasSuffix(trim, "}") {
		return true, "missing closing brace(s)"
	}
	if trim[0] == '[' && !strings.HasSuffix(trim, "]") {
		return true, "missing closing bracket(s)"
	}
	if strings.Count(trim, "{") != strings.Count(trim, "}") {
		return true, "unbalanced braces"
	}
	if strings.Count(trim, "[") != strings.Count(trim, "]") {
		return true, "unbalanced brackets"
	}
	escaped := false
	quotes := 0
	for _, r := range trim {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			quotes++
		}
	}
	if quotes%2 != 0 {
		return true, "unclosed string literal"
	}
	return false, "malformed JSON"
}
func ParseBracketToolCalls(text string) []ToolCall {
	var out []ToolCall
	lower := strings.ToLower(text)
	for offset := 0; ; {
		i := strings.Index(lower[offset:], "[called ")
		if i < 0 {
			break
		}
		start := offset + i + len("[Called ")
		rest := text[start:]
		marker := strings.Index(strings.ToLower(rest), " with args:")
		if marker < 0 {
			break
		}
		name := strings.TrimSpace(rest[:marker])
		jsonStart := start + marker + len(" with args:")
		relative := strings.Index(text[jsonStart:], "{")
		if relative < 0 {
			offset = start
			continue
		}
		jsonStart += relative
		end := MatchingBrace(text, jsonStart)
		if end < 0 {
			offset = jsonStart + 1
			continue
		}
		var value any
		if json.Unmarshal([]byte(text[jsonStart:end+1]), &value) == nil {
			b, _ := json.Marshal(value)
			out = append(out, ToolCall{ID: newToolID(), Type: "function", Function: ToolFunction{Name: name, Arguments: string(b)}})
		}
		offset = end + 1
	}
	return out
}
func newToolID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "call_00000000"
	}
	return "call_" + hex.EncodeToString(b[:])
}
