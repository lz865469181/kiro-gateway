package tokenizer

import (
	"testing"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/convert"
	"github.com/jwadow/kiro-gateway-go/internal/model"
)

func TestCountTokensFallbackAndCorrection(t *testing.T) {
	if ClaudeCorrectionFactor != 1.15 || CountTokens("") != 0 {
		t.Fatal("constants or empty count changed")
	}
	if got := CountTokens("Test", false); got != 2 {
		t.Fatalf("uncorrected fallback %d", got)
	}
	text := "This is a test text for token counting"
	without, with := CountTokens(text, false), CountTokens(text)
	if with <= without || float64(with)/float64(without) < 1.1 || float64(with)/float64(without) > 1.2 {
		t.Fatalf("correction: %d -> %d", without, with)
	}
	if CountTokens("Привет, мир! 你好世界 🌍") <= 0 {
		t.Fatal("unicode not counted")
	}
}

func TestCountMessageTokensContentAndTools(t *testing.T) {
	if CountMessageTokens(nil) != 0 || CountMessageTokens([]map[string]any{}) != 0 {
		t.Fatal("empty messages not zero")
	}
	messages := []map[string]any{
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_123", "name": "weather", "input": map[string]any{"city": "Tokyo"}}}},
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_123", "is_error": false, "content": []map[string]any{{"type": "text", "text": "sunny"}, {"type": "image"}}}}},
		{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"function": map[string]any{"name": "next", "arguments": "{}"}}}},
		{"role": "tool", "content": "done", "tool_call_id": "call_1"},
	}
	without := CountMessageTokens(messages, false)
	if without <= 100 {
		t.Fatalf("message blocks undercounted: %d", without)
	}
	if with := CountMessageTokens(messages); with <= without {
		t.Fatalf("correction not applied: %d <= %d", with, without)
	}
	unknown := CountMessageTokens([]map[string]any{{"role": "user", "content": []any{map[string]any{"type": "custom", "payload": "value"}, 42}}}, false)
	if unknown <= 7 {
		t.Fatalf("unknown fallback undercounted: %d", unknown)
	}
}

func TestCountToolsTokensFormats(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}
	openAI := []map[string]any{{"type": "function", "function": map[string]any{"name": "search", "description": "Search files", "parameters": schema}}}
	anthropic := []map[string]any{{"name": "search", "description": "Search files", "input_schema": schema}}
	a, b := CountToolsTokens(openAI, false), CountToolsTokens(anthropic, false)
	if a <= 4 || b <= 4 || a != b {
		t.Fatalf("tool formats: openai=%d anthropic=%d", a, b)
	}
	if CountToolsTokens(nil) != 0 || CountToolsTokens([]any{}) != 0 {
		t.Fatal("empty tools not zero")
	}
}

func TestCountSystemTokensAndEstimate(t *testing.T) {
	blocks := []any{
		map[string]any{"type": "text", "text": "You are helpful."},
		map[string]any{"type": "text", "text": "Be concise.", "cache_control": map[string]any{"type": "ephemeral"}},
		42,
	}
	withoutCache := CountSystemTokens([]any{map[string]any{"text": "Hello"}}, false)
	withCache := CountSystemTokens([]any{map[string]any{"text": "Hello", "cache_control": map[string]any{"type": "ephemeral"}}}, false)
	if CountSystemTokens(nil) != 0 || CountSystemTokens("") != 0 || CountSystemTokens(blocks, false) <= 0 || withCache <= withoutCache {
		t.Fatal("system counting failed")
	}
	messages := []map[string]any{{"role": "user", "content": "Hello"}}
	tools := []map[string]any{{"name": "read", "description": "Read file", "input_schema": map[string]any{"type": "object"}}}
	estimate := EstimateRequestTokens(messages, tools, blocks, false)
	if estimate.MessagesTokens <= 0 || estimate.ToolsTokens <= 4 || estimate.SystemTokens <= 0 || estimate.TotalTokens != estimate.MessagesTokens+estimate.ToolsTokens+estimate.SystemTokens {
		t.Fatalf("estimate %#v", estimate)
	}
}

func TestImageFixedCost(t *testing.T) {
	base := CountMessageTokens([]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "look"}}}}, false)
	image := CountMessageTokens([]map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "look"}, {"type": "image_url"}}}}, false)
	if image-base != 100 {
		t.Fatalf("image cost %d", image-base)
	}
}

func TestUsageFromContext(t *testing.T) {
	cache := model.NewCache(time.Hour, 200000)
	cache.Update([]model.Info{{ModelID: "small", TokenLimits: map[string]any{"maxInputTokens": 1000}}})

	prompt, total := UsageFromContext(25, 40, 12, cache, "small")
	if prompt != 210 || total != 250 {
		t.Fatalf("context usage prompt=%d total=%d", prompt, total)
	}
	prompt, total = UsageFromContext(1, 20, 12, cache, "small")
	if prompt != 0 || total != 10 {
		t.Fatalf("completion larger than context prompt=%d total=%d", prompt, total)
	}
	for _, percentage := range []float64{0, -1} {
		prompt, total = UsageFromContext(percentage, 40, 12, cache, "small")
		if prompt != 12 || total != 52 {
			t.Fatalf("fallback at %v prompt=%d total=%d", percentage, prompt, total)
		}
	}
}

func TestTypedRequestStructs(t *testing.T) {
	description := "Read a workspace file"
	messages := []convert.ChatMessage{{Role: "user", Content: "Read main.go"}}
	tools := []convert.OpenAITool{{
		Type: "function",
		Function: &convert.ToolFunction{
			Name: "read_file", Description: &description,
			Parameters: map[string]any{"type": "object"},
		},
	}}
	if CountMessageTokens(messages, false) <= 0 || CountToolsTokens(tools, false) <= 4 {
		t.Fatal("typed request structs were not counted")
	}
}
