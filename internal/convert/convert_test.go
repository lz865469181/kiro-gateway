package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestExtractTextAndImages(t *testing.T) {
	blocks := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "tool_reference", "text": "ignored"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,png-data"}},
		" world",
	}
	if got := ExtractTextContent(blocks); got != "hello world" {
		t.Fatalf("text = %q", got)
	}
	images := ExtractImagesFromContent(blocks)
	if len(images) != 1 || images[0]["media_type"] != "image/png" || images[0]["data"] != "png-data" {
		t.Fatalf("images = %#v", images)
	}
	anthropic := []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/webp", "data": "webp-data"}}}
	images = ExtractImagesFromContent(anthropic)
	if len(images) != 1 || images[0]["media_type"] != "image/webp" {
		t.Fatalf("anthropic images = %#v", images)
	}
	kiro := ConvertImagesToKiroFormat([]map[string]any{{"data": "data:image/jpeg;base64,jpeg-data"}})
	if kiro[0]["format"] != "jpeg" || kiro[0]["source"].(map[string]any)["bytes"] != "jpeg-data" {
		t.Fatalf("kiro image = %#v", kiro)
	}
}

func TestSanitizeJSONSchemaRecursive(t *testing.T) {
	schema := map[string]any{
		"type": "object", "required": []string{}, "additionalProperties": false,
		"properties": map[string]any{"nested": map[string]any{"type": "object", "required": []any{}, "additionalProperties": true}},
		"anyOf":      []any{map[string]any{"type": "object", "additionalProperties": false}, "literal"},
	}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["required"]; ok {
		t.Fatal("empty required retained")
	}
	if _, ok := got["additionalProperties"]; ok {
		t.Fatal("additionalProperties retained")
	}
	nested := got["properties"].(map[string]any)["nested"].(map[string]any)
	if _, ok := nested["required"]; ok {
		t.Fatal("nested required retained")
	}
	if _, ok := got["anyOf"].([]any)[0].(map[string]any)["additionalProperties"]; ok {
		t.Fatal("list schema not sanitized")
	}
}

func TestMessageRepairAndMerging(t *testing.T) {
	orphan := UnifiedMessage{Role: "user", Content: "before", ToolResults: []map[string]any{{"tool_use_id": "x", "content": "result"}}, Images: []map[string]any{{"data": "image"}}}
	fixed, changed := EnsureAssistantBeforeToolResults([]UnifiedMessage{orphan})
	if !changed || len(fixed[0].ToolResults) != 0 || !strings.Contains(fixed[0].Content.(string), "[Tool Result (x)]") || len(fixed[0].Images) != 1 {
		t.Fatalf("orphan repair = %#v", fixed)
	}
	valid := []UnifiedMessage{{Role: "assistant", ToolCalls: []map[string]any{{"id": "x"}}}, orphan}
	fixed, changed = EnsureAssistantBeforeToolResults(valid)
	if changed || len(fixed[1].ToolResults) != 1 {
		t.Fatal("valid result modified")
	}
	merged := MergeAdjacentMessages([]UnifiedMessage{{Role: "assistant", Content: "a", ToolCalls: []map[string]any{{"id": "1"}}}, {Role: "assistant", Content: "b", ToolCalls: []map[string]any{{"id": "2"}}}})
	if len(merged) != 1 || merged[0].Content != "a\nb" || len(merged[0].ToolCalls) != 2 {
		t.Fatalf("merge = %#v", merged)
	}
	roles := EnsureAlternatingRoles(NormalizeMessageRoles([]UnifiedMessage{{Role: "developer", Content: "a"}, {Role: "developer", Content: "b"}, {Role: "user", Content: "c"}}))
	want := []string{"user", "assistant", "user", "assistant", "user"}
	for i := range want {
		if roles[i].Role != want[i] {
			t.Fatalf("roles = %#v", roles)
		}
	}
}

func TestStripToolContentPreservesContext(t *testing.T) {
	messages := []UnifiedMessage{{Role: "assistant", Content: "working", ToolCalls: []map[string]any{{"id": "c1", "function": map[string]any{"name": "bash", "arguments": "{\"command\":\"ls\"}"}}}}, {Role: "user", ToolResults: []map[string]any{{"tool_use_id": "c1", "content": "output"}}, Images: []map[string]any{{"data": "img"}}}}
	got, changed := StripAllToolContent(messages)
	if !changed || len(got[0].ToolCalls) != 0 || !strings.Contains(got[0].Content.(string), "[Tool: bash (c1)]") || !strings.Contains(got[1].Content.(string), "output") || len(got[1].Images) != 1 {
		t.Fatalf("stripped = %#v", got)
	}
}

func TestOpenAIAdapter(t *testing.T) {
	messages := []ChatMessage{{Role: "system", Content: "one"}, {Role: "system", Content: "two"}, {Role: "assistant", ToolCalls: []map[string]any{{"id": "c", "function": map[string]any{"name": "shot", "arguments": "{}"}}}}, {Role: "tool", ToolCallID: "c", Content: []any{map[string]any{"type": "text", "text": "done"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,img"}}}}, {Role: "user", Content: "next"}}
	system, got := ConvertOpenAIMessagesToUnified(messages)
	if system != "one\ntwo" || len(got) != 3 || len(got[0].ToolCalls) != 1 || len(got[1].ToolResults) != 1 || len(got[1].Images) != 1 {
		t.Fatalf("system=%q messages=%#v", system, got)
	}
	tools := ConvertOpenAIToolsToUnified([]OpenAITool{{Function: &ToolFunction{Name: "nested"}}, {Name: "flat", InputSchema: map[string]any{}}, {Type: "other", Name: "skip"}})
	if len(tools) != 2 || tools[0].Name != "nested" || tools[1].Name != "flat" {
		t.Fatalf("tools=%#v", tools)
	}
	for effort, want := range map[string]int{"none": 0, "minimal": 409, "low": 819, "medium": 2048, "high": 3276, "xhigh": 3891} {
		if got := ReasoningEffortToBudget(4096, effort); got != want {
			t.Errorf("%s=%d want %d", effort, got, want)
		}
	}
}

func TestAnthropicAdapter(t *testing.T) {
	content := []any{map[string]any{"type": "text", "text": "using"}, map[string]any{"type": "tool_use", "id": "c", "name": "browser", "input": map[string]any{"url": "x"}}}
	resultContent := []any{map[string]any{"type": "tool_result", "tool_use_id": "c", "content": []any{map[string]any{"type": "text", "text": "captured"}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "img"}}}}}
	got := ConvertAnthropicMessages([]AnthropicMessage{{Role: "assistant", Content: content}, {Role: "user", Content: resultContent}})
	if got[0].Content != "using" || len(got[0].ToolCalls) != 1 || len(got[1].ToolResults) != 1 || got[1].ToolResults[0]["content"] != "captured" || len(got[1].Images) != 1 {
		t.Fatalf("messages=%#v", got)
	}
	system := ExtractSystemPrompt([]any{map[string]any{"type": "text", "text": "one", "cache_control": map[string]any{}}, map[string]any{"type": "text", "text": "two"}})
	if system != "one\ntwo" {
		t.Fatalf("system=%q", system)
	}
	cfg := ExtractThinkingConfigFromAnthropic(AnthropicMessagesRequest{Thinking: map[string]any{"type": "enabled", "budget_tokens": float64(8000)}})
	if cfg.BudgetTokens == nil || *cfg.BudgetTokens != 8000 {
		t.Fatalf("cfg=%#v", cfg)
	}
}

func quietOptions() Options {
	o := DefaultOptions()
	o.FakeReasoningEnabled = false
	o.TruncationRecovery = false
	return o
}

func TestExactKiroPayload(t *testing.T) {
	desc := ""
	opts := quietOptions()
	req := ChatCompletionRequest{Model: "claude-sonnet-4-5-20250101", Messages: []ChatMessage{{Role: "system", Content: "system"}, {Role: "user", Content: "hello"}, {Role: "assistant", Content: "calling", ToolCalls: []map[string]any{{"id": "c1", "function": map[string]any{"name": "tool", "arguments": "{\"x\":1}"}}}}, {Role: "tool", ToolCallID: "c1", Content: "ok"}}, Tools: []OpenAITool{{Function: &ToolFunction{Name: "tool", Description: &desc, Parameters: map[string]any{"type": "object", "required": []any{}, "additionalProperties": false}}}}}
	payload, err := BuildOpenAIKiroPayloadWithOptions(req, "conv", "arn", opts)
	if err != nil {
		t.Fatal(err)
	}
	state := payload["conversationState"].(map[string]any)
	if state["chatTriggerType"] != "MANUAL" || state["conversationId"] != "conv" || payload["profileArn"] != "arn" {
		t.Fatalf("payload=%#v", payload)
	}
	history := state["history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history=%#v", history)
	}
	first := history[0].(map[string]any)["userInputMessage"].(map[string]any)
	if first["content"] != "system\n\nhello" || first["modelId"] != "claude-sonnet-4.5" || first["origin"] != "AI_EDITOR" {
		t.Fatalf("first=%#v", first)
	}
	assistant := history[1].(map[string]any)["assistantResponseMessage"].(map[string]any)
	uses := assistant["toolUses"].([]any)
	if uses[0].(map[string]any)["input"].(map[string]any)["x"] != float64(1) {
		t.Fatalf("assistant=%#v", assistant)
	}
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	ctx := current["userInputMessageContext"].(map[string]any)
	if len(ctx["toolResults"].([]any)) != 1 || len(ctx["tools"].([]any)) != 1 {
		t.Fatalf("current=%#v", current)
	}
	spec := ctx["tools"].([]any)[0].(map[string]any)["toolSpecification"].(map[string]any)
	if spec["description"] != "Tool: tool" {
		t.Fatalf("spec=%#v", spec)
	}
	schema := spec["inputSchema"].(map[string]any)["json"].(map[string]any)
	if _, ok := schema["required"]; ok {
		t.Fatal("required retained")
	}
}

func TestThinkingLongToolsAndValidation(t *testing.T) {
	opts := DefaultOptions()
	opts.TruncationRecovery = false
	opts.ToolDescriptionMaxLength = 4
	opts.FakeReasoningMaxTokens = 4000
	opts.FakeReasoningBudgetCap = 5000
	long := "lengthy"
	budget := 8000
	result, err := BuildKiroPayload([]UnifiedMessage{{Role: "user", Content: "question"}}, "system", "m", []UnifiedTool{{Name: "read", Description: &long}}, "c", "", ThinkingConfig{Enabled: true, BudgetTokens: &budget}, opts)
	if err != nil {
		t.Fatal(err)
	}
	content := result.Payload["conversationState"].(map[string]any)["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)["content"].(string)
	for _, part := range []string{"## Tool: read", "<thinking_mode>enabled</thinking_mode>", "<max_thinking_length>5000</max_thinking_length>", "question"} {
		if !strings.Contains(content, part) {
			t.Errorf("missing %q", part)
		}
	}
	if !strings.Contains(result.ToolDocumentation, "lengthy") {
		t.Fatal("missing documentation")
	}
	_, err = BuildKiroPayload([]UnifiedMessage{{Role: "user", Content: "x"}}, "", "m", []UnifiedTool{{Name: strings.Repeat("x", 65)}}, "c", "", ThinkingConfig{}, opts)
	if err == nil || !strings.Contains(err.Error(), "64 characters") {
		t.Fatalf("err=%v", err)
	}
}

func makeTrimPayload(pairs, size int) map[string]any {
	h := []any{}
	for i := 0; i < pairs; i++ {
		h = append(h, map[string]any{"userInputMessage": map[string]any{"content": strings.Repeat("x", size)}}, map[string]any{"assistantResponseMessage": map[string]any{"content": strings.Repeat("y", size)}})
	}
	return map[string]any{"conversationState": map[string]any{"chatTriggerType": "MANUAL", "conversationId": "c", "currentMessage": map[string]any{"userInputMessage": map[string]any{"content": "now", "modelId": "m"}}, "history": h}}
}

func TestPayloadSizeAndTrim(t *testing.T) {
	payload := makeTrimPayload(10, 500)
	original := CheckPayloadSize(payload)
	stats := TrimPayloadToLimit(payload, original/2)
	if !stats.Trimmed || stats.FinalBytes > original/2 || stats.FinalEntries < 2 || stats.FinalEntries >= stats.OriginalEntries {
		t.Fatalf("stats=%#v", stats)
	}
	h := payload["conversationState"].(map[string]any)["history"].([]any)
	if _, ok := h[0].(map[string]any)["userInputMessage"]; !ok {
		t.Fatal("history not user aligned")
	}
	b, _ := json.Marshal(map[string]any{"key": "value"})
	if CheckPayloadSize(map[string]any{"key": "value"}) != len(b) {
		t.Fatal("size mismatch")
	}
}

func TestTrimRepairsOrphanedResults(t *testing.T) {
	h := []any{map[string]any{"userInputMessage": map[string]any{"content": "u"}}, map[string]any{"assistantResponseMessage": map[string]any{"content": "a", "toolUses": []any{map[string]any{"toolUseId": "valid"}}}}, map[string]any{"userInputMessage": map[string]any{"content": "u2", "userInputMessageContext": map[string]any{"toolResults": []any{map[string]any{"toolUseId": "valid", "content": []any{map[string]any{"text": "keep"}}}, map[string]any{"toolUseId": "orphan", "content": []any{map[string]any{"text": "preserve"}}}}}}}, map[string]any{"assistantResponseMessage": map[string]any{"content": "a2", "toolUses": []any{}}}}
	payload := map[string]any{"conversationState": map[string]any{"history": h}}
	TrimPayloadToLimit(payload, CheckPayloadSize(payload)+100)
	user := h[2].(map[string]any)["userInputMessage"].(map[string]any)
	ctx := user["userInputMessageContext"].(map[string]any)
	if len(ctx["toolResults"].([]any)) != 1 || !strings.Contains(user["content"].(string), "[trimmed tool result] preserve") {
		t.Fatalf("user=%#v", user)
	}
	a := h[3].(map[string]any)["assistantResponseMessage"].(map[string]any)
	if _, ok := a["toolUses"]; ok {
		t.Fatal("empty toolUses retained")
	}
}
