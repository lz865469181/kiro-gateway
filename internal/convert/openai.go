package convert

import "strings"

func ConvertOpenAIMessagesToUnified(messages []ChatMessage) (string, []UnifiedMessage) {
	systems := []string{}
	nonSystem := []ChatMessage{}
	for _, msg := range messages {
		if msg.Role == "system" {
			systems = append(systems, ExtractTextContent(msg.Content))
		} else {
			nonSystem = append(nonSystem, msg)
		}
	}
	out := []UnifiedMessage{}
	pendingResults := []map[string]any{}
	pendingImages := []map[string]any{}
	flush := func() {
		if len(pendingResults) > 0 {
			out = append(out, UnifiedMessage{Role: "user", Content: "", ToolResults: append([]map[string]any(nil), pendingResults...), Images: append([]map[string]any(nil), pendingImages...)})
			pendingResults = nil
			pendingImages = nil
		}
	}
	for _, msg := range nonSystem {
		if msg.Role == "tool" {
			text := ExtractTextContent(msg.Content)
			if text == "" {
				text = "(empty result)"
			}
			pendingResults = append(pendingResults, map[string]any{"type": "tool_result", "tool_use_id": msg.ToolCallID, "content": text})
			pendingImages = append(pendingImages, ExtractImagesFromContent(msg.Content)...)
			continue
		}
		flush()
		u := UnifiedMessage{Role: msg.Role, Content: ExtractTextContent(msg.Content)}
		if msg.Role == "assistant" {
			for _, tc := range msg.ToolCalls {
				fn, _ := asMap(tc["function"])
				u.ToolCalls = append(u.ToolCalls, map[string]any{"id": stringValue(tc["id"]), "type": "function", "function": map[string]any{"name": stringValue(fn["name"]), "arguments": valueOr(fn["arguments"], "{}")}})
			}
		}
		if msg.Role == "user" {
			if items, ok := asSlice(msg.Content); ok {
				for _, item := range items {
					m, _ := asMap(item)
					if m["type"] == "tool_result" {
						text := ExtractTextContent(m["content"])
						if text == "" {
							text = "(empty result)"
						}
						u.ToolResults = append(u.ToolResults, map[string]any{"type": "tool_result", "tool_use_id": stringValue(m["tool_use_id"]), "content": text})
					}
				}
			}
			u.Images = ExtractImagesFromContent(msg.Content)
		}
		out = append(out, u)
	}
	flush()
	return strings.TrimSpace(strings.Join(systems, "\n")), out
}

func valueOr(v any, fallback any) any {
	if v == nil || v == "" {
		return fallback
	}
	return v
}

func ConvertOpenAIToolsToUnified(tools []OpenAITool) []UnifiedTool {
	out := []UnifiedTool{}
	for _, tool := range tools {
		typ := tool.Type
		if typ == "" {
			typ = "function"
		}
		if typ != "function" {
			continue
		}
		if tool.Function != nil {
			out = append(out, UnifiedTool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters})
		} else if tool.Name != "" {
			out = append(out, UnifiedTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
		}
	}
	return out
}

func ReasoningEffortToBudget(maxTokens int, effort string) int {
	p := map[string]float64{"none": 0, "minimal": .10, "low": .20, "medium": .50, "high": .80, "xhigh": .95}
	return int(float64(maxTokens) * p[effort])
}

func ExtractThinkingConfigFromOpenAI(request ChatCompletionRequest) ThinkingConfig {
	if request.ReasoningEffort == "" {
		return ThinkingConfig{Enabled: true}
	}
	if request.ReasoningEffort == "none" {
		return ThinkingConfig{Enabled: false}
	}
	max := 4096
	if request.MaxTokens != nil {
		max = *request.MaxTokens
	} else if request.MaxCompletionTokens != nil {
		max = *request.MaxCompletionTokens
	}
	budget := ReasoningEffortToBudget(max, request.ReasoningEffort)
	return ThinkingConfig{Enabled: true, BudgetTokens: &budget}
}

func BuildOpenAIKiroPayloadWithOptions(request ChatCompletionRequest, conversationID, profileARN string, opts Options) (map[string]any, error) {
	system, messages := ConvertOpenAIMessagesToUnified(request.Messages)
	result, err := BuildKiroPayload(messages, system, ResolveModel(request.Model, opts.HiddenModels), ConvertOpenAIToolsToUnified(request.Tools), conversationID, profileARN, ExtractThinkingConfigFromOpenAI(request), opts)
	return result.Payload, err
}

func BuildOpenAIKiroPayload(request ChatCompletionRequest, conversationID, profileARN string) (map[string]any, error) {
	return BuildOpenAIKiroPayloadWithOptions(request, conversationID, profileARN, DefaultOptions())
}
func OpenAIToKiro(request ChatCompletionRequest, conversationID, profileARN string) (map[string]any, error) {
	return BuildOpenAIKiroPayload(request, conversationID, profileARN)
}
