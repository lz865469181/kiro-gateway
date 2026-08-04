package convert

import (
	"fmt"
	"strings"
)

func ConvertAnthropicContentToText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	items, ok := asSlice(content)
	if !ok {
		if content == nil {
			return ""
		}
		return fmt.Sprint(content)
	}
	var b strings.Builder
	for _, item := range items {
		m, _ := asMap(item)
		if m["type"] == "text" {
			b.WriteString(stringValue(m["text"]))
		}
	}
	return b.String()
}

func ExtractSystemPrompt(system any) string {
	if system == nil {
		return ""
	}
	if s, ok := system.(string); ok {
		return s
	}
	items, ok := asSlice(system)
	if !ok {
		return fmt.Sprint(system)
	}
	parts := []string{}
	for _, item := range items {
		m, _ := asMap(item)
		if m["type"] == "text" {
			parts = append(parts, stringValue(m["text"]))
		}
	}
	return strings.Join(parts, "\n")
}

func ExtractToolResultsFromAnthropicContent(content any) []map[string]any {
	items, ok := asSlice(content)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, item := range items {
		m, _ := asMap(item)
		id := stringValue(m["tool_use_id"])
		if m["type"] != "tool_result" || id == "" {
			continue
		}
		value := m["content"]
		text := ""
		if _, ok := asSlice(value); ok {
			text = ExtractTextContent(value)
		} else {
			text = stringValue(value)
		}
		if text == "" {
			text = "(empty result)"
		}
		out = append(out, map[string]any{"type": "tool_result", "tool_use_id": id, "content": text})
	}
	return out
}

func ExtractImagesFromToolResults(content any) []map[string]any {
	items, ok := asSlice(content)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, item := range items {
		m, _ := asMap(item)
		if m["type"] == "tool_result" {
			out = append(out, ExtractImagesFromContent(m["content"])...)
		}
	}
	return out
}

func ExtractToolUsesFromAnthropicContent(content any) []map[string]any {
	items, ok := asSlice(content)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, item := range items {
		m, _ := asMap(item)
		id, name := stringValue(m["id"]), stringValue(m["name"])
		if m["type"] != "tool_use" || id == "" || name == "" {
			continue
		}
		input := m["input"]
		if input == nil {
			input = map[string]any{}
		}
		out = append(out, map[string]any{
			"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": input},
		})
	}
	return out
}

func ConvertAnthropicMessages(messages []AnthropicMessage) []UnifiedMessage {
	out := make([]UnifiedMessage, 0, len(messages))
	for _, msg := range messages {
		u := UnifiedMessage{Role: msg.Role, Content: ConvertAnthropicContentToText(msg.Content)}
		if msg.Role == "assistant" {
			u.ToolCalls = ExtractToolUsesFromAnthropicContent(msg.Content)
		} else if msg.Role == "user" {
			u.ToolResults = ExtractToolResultsFromAnthropicContent(msg.Content)
			u.Images = append(ExtractImagesFromContent(msg.Content), ExtractImagesFromToolResults(msg.Content)...)
		}
		out = append(out, u)
	}
	return out
}

func ConvertAnthropicTools(tools []AnthropicTool) []UnifiedTool {
	out := make([]UnifiedTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, UnifiedTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	}
	return out
}

func ExtractThinkingConfigFromAnthropic(request AnthropicMessagesRequest) ThinkingConfig {
	if len(request.Thinking) == 0 {
		return ThinkingConfig{Enabled: true}
	}
	typ := stringValue(request.Thinking["type"])
	if typ == "disabled" {
		return ThinkingConfig{Enabled: false}
	}
	if typ == "enabled" {
		if n, ok := numberToInt(request.Thinking["budget_tokens"]); ok && n != 0 {
			return ThinkingConfig{Enabled: true, BudgetTokens: &n}
		}
	}
	return ThinkingConfig{Enabled: true}
}

func numberToInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func AnthropicToKiroWithOptions(request AnthropicMessagesRequest, conversationID, profileARN string, opts Options) (map[string]any, error) {
	result, err := BuildKiroPayload(ConvertAnthropicMessages(request.Messages), ExtractSystemPrompt(request.System), ResolveModel(request.Model, opts.HiddenModels), ConvertAnthropicTools(request.Tools), conversationID, profileARN, ExtractThinkingConfigFromAnthropic(request), opts)
	return result.Payload, err
}

func AnthropicToKiro(request AnthropicMessagesRequest, conversationID, profileARN string) (map[string]any, error) {
	return AnthropicToKiroWithOptions(request, conversationID, profileARN, DefaultOptions())
}
