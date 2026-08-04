package convert

import "strings"

func ToolCallsToText(calls []map[string]any) string {
	parts := make([]string, 0, len(calls))
	for _, tc := range calls {
		fn, _ := asMap(tc["function"])
		name := stringValue(fn["name"])
		if name == "" {
			name = "unknown"
		}
		args := stringValue(fn["arguments"])
		if args == "" {
			args = "{}"
		}
		id := stringValue(tc["id"])
		if id != "" {
			parts = append(parts, "[Tool: "+name+" ("+id+")]\n"+args)
		} else {
			parts = append(parts, "[Tool: "+name+"]\n"+args)
		}
	}
	return strings.Join(parts, "\n\n")
}

func ToolResultsToText(results []map[string]any) string {
	parts := make([]string, 0, len(results))
	for _, tr := range results {
		text := ExtractTextContent(tr["content"])
		if text == "" {
			text = "(empty result)"
		}
		id := stringValue(tr["tool_use_id"])
		if id != "" {
			parts = append(parts, "[Tool Result ("+id+")]\n"+text)
		} else {
			parts = append(parts, "[Tool Result]\n"+text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func StripAllToolContent(messages []UnifiedMessage) ([]UnifiedMessage, bool) {
	out := make([]UnifiedMessage, 0, len(messages))
	changed := false
	for _, msg := range messages {
		if len(msg.ToolCalls) == 0 && len(msg.ToolResults) == 0 {
			out = append(out, msg)
			continue
		}
		changed = true
		parts := []string{}
		if text := ExtractTextContent(msg.Content); text != "" {
			parts = append(parts, text)
		}
		if text := ToolCallsToText(msg.ToolCalls); text != "" {
			parts = append(parts, text)
		}
		if text := ToolResultsToText(msg.ToolResults); text != "" {
			parts = append(parts, text)
		}
		content := strings.Join(parts, "\n\n")
		if content == "" {
			content = "(empty placeholder)"
		}
		out = append(out, UnifiedMessage{Role: msg.Role, Content: content, Images: msg.Images})
	}
	return out, changed
}

func EnsureAssistantBeforeToolResults(messages []UnifiedMessage) ([]UnifiedMessage, bool) {
	out := make([]UnifiedMessage, 0, len(messages))
	changed := false
	for _, msg := range messages {
		if len(msg.ToolResults) > 0 && (len(out) == 0 || out[len(out)-1].Role != "assistant" || len(out[len(out)-1].ToolCalls) == 0) {
			text := ToolResultsToText(msg.ToolResults)
			original := ExtractTextContent(msg.Content)
			if original != "" && text != "" {
				original += "\n\n" + text
			} else if text != "" {
				original = text
			}
			msg.Content = original
			msg.ToolResults = nil
			changed = true
		}
		out = append(out, msg)
	}
	return out, changed
}

func MergeAdjacentMessages(messages []UnifiedMessage) []UnifiedMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]UnifiedMessage, 0, len(messages))
	for _, msg := range messages {
		if len(out) == 0 || out[len(out)-1].Role != msg.Role {
			out = append(out, msg)
			continue
		}
		last := &out[len(out)-1]
		lastItems, lastList := asSlice(last.Content)
		items, list := asSlice(msg.Content)
		switch {
		case lastList && list:
			last.Content = append(lastItems, items...)
		case lastList:
			last.Content = append(lastItems, map[string]any{"type": "text", "text": ExtractTextContent(msg.Content)})
		case list:
			last.Content = append([]any{map[string]any{"type": "text", "text": ExtractTextContent(last.Content)}}, items...)
		default:
			last.Content = ExtractTextContent(last.Content) + "\n" + ExtractTextContent(msg.Content)
		}
		if msg.Role == "assistant" {
			last.ToolCalls = append(last.ToolCalls, msg.ToolCalls...)
		}
		if msg.Role == "user" {
			last.ToolResults = append(last.ToolResults, msg.ToolResults...)
		}
		// Python does not merge images here; preserve that behavior.
	}
	return out
}

func EnsureFirstMessageIsUser(messages []UnifiedMessage) []UnifiedMessage {
	if len(messages) > 0 && messages[0].Role != "user" {
		return append([]UnifiedMessage{{Role: "user", Content: "(empty placeholder)"}}, messages...)
	}
	return messages
}

func NormalizeMessageRoles(messages []UnifiedMessage) []UnifiedMessage {
	for i := range messages {
		if messages[i].Role != "user" && messages[i].Role != "assistant" {
			messages[i].Role = "user"
		}
	}
	return messages
}

func EnsureAlternatingRoles(messages []UnifiedMessage) []UnifiedMessage {
	if len(messages) < 2 {
		return messages
	}
	out := []UnifiedMessage{messages[0]}
	for _, msg := range messages[1:] {
		if msg.Role == "user" && out[len(out)-1].Role == "user" {
			out = append(out, UnifiedMessage{Role: "assistant", Content: "(empty placeholder)"})
		}
		out = append(out, msg)
	}
	return out
}

func extractToolResultsFromContent(content any) []map[string]any {
	items, ok := asSlice(content)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, item := range items {
		m, ok := asMap(item)
		if !ok || m["type"] != "tool_result" {
			continue
		}
		text := ExtractTextContent(m["content"])
		if text == "" {
			text = "(empty result)"
		}
		out = append(out, map[string]any{"tool_use_id": stringValue(m["tool_use_id"]), "content": text})
	}
	return out
}

func convertToolResults(results []map[string]any) []any {
	out := make([]any, 0, len(results))
	for _, tr := range results {
		text := ExtractTextContent(tr["content"])
		if text == "" {
			text = "(empty result)"
		}
		out = append(out, map[string]any{"content": []any{map[string]any{"text": text}}, "status": "success", "toolUseId": stringValue(tr["tool_use_id"])})
	}
	return out
}

func extractToolUses(content any, calls []map[string]any) []any {
	out := []any{}
	for _, tc := range calls {
		fn, _ := asMap(tc["function"])
		out = append(out, map[string]any{"name": stringValue(fn["name"]), "input": decodeArguments(fn["arguments"]), "toolUseId": stringValue(tc["id"])})
	}
	if items, ok := asSlice(content); ok {
		for _, item := range items {
			if m, ok := asMap(item); ok && m["type"] == "tool_use" {
				out = append(out, map[string]any{"name": stringValue(m["name"]), "input": m["input"], "toolUseId": stringValue(m["id"])})
			}
		}
	}
	return out
}

func BuildKiroHistory(messages []UnifiedMessage, modelID string) []any {
	out := []any{}
	for _, msg := range messages {
		content := ExtractTextContent(msg.Content)
		if content == "" {
			content = "(empty placeholder)"
		}
		if msg.Role == "user" {
			u := map[string]any{"content": content, "modelId": modelID, "origin": "AI_EDITOR"}
			images := msg.Images
			if len(images) == 0 {
				images = ExtractImagesFromContent(msg.Content)
			}
			if k := ConvertImagesToKiroFormat(images); len(k) > 0 {
				u["images"] = mapsToAny(k)
			}
			results := msg.ToolResults
			if len(results) == 0 {
				results = extractToolResultsFromContent(msg.Content)
			}
			if len(results) > 0 {
				u["userInputMessageContext"] = map[string]any{"toolResults": convertToolResults(results)}
			}
			out = append(out, map[string]any{"userInputMessage": u})
		} else if msg.Role == "assistant" {
			a := map[string]any{"content": content}
			if uses := extractToolUses(msg.Content, msg.ToolCalls); len(uses) > 0 {
				a["toolUses"] = uses
			}
			out = append(out, map[string]any{"assistantResponseMessage": a})
		}
	}
	return out
}

func mapsToAny(in []map[string]any) []any {
	out := make([]any, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}
