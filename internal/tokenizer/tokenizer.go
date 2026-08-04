// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tokenizer provides fast, conservative request token estimates. Claude's
// tokenizer is not public, so estimates use the Python gateway's four-character
// fallback and its empirical Claude correction factor.
package tokenizer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/jwadow/kiro-gateway-go/internal/model"
)

const ClaudeCorrectionFactor = 1.15

func correctedOption(options []bool) bool {
	return len(options) == 0 || options[0]
}

// CountTokens estimates text tokens and applies Claude's correction by default.
func CountTokens(text string, applyClaudeCorrection ...bool) int {
	return CountTokensFromRunes(len([]rune(text)), applyClaudeCorrection...)
}

// CountTokensFromRunes estimates tokens without retaining the original text.
func CountTokensFromRunes(runes int, applyClaudeCorrection ...bool) int {
	if runes <= 0 {
		return 0
	}
	base := runes/4 + 1
	if correctedOption(applyClaudeCorrection) {
		return int(float64(base) * ClaudeCorrectionFactor)
	}
	return base
}

func sequence(value any) ([]any, bool) {
	if value == nil {
		return nil, false
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	result := make([]any, rv.Len())
	for i := range result {
		result[i] = rv.Index(i).Interface()
	}
	return result, true
}

func object(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	if result, ok := value.(map[string]any); ok {
		return result, true
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
		result := make(map[string]any, rv.Len())
		iterator := rv.MapRange()
		for iterator.Next() {
			result[iterator.Key().String()] = iterator.Value().Interface()
		}
		return result, true
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}

	// JSON round-tripping honors field tags and omitempty while letting this
	// package count request structs without importing their owning package.
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var result map[string]any
	if json.Unmarshal(encoded, &result) != nil {
		return nil, false
	}
	return result, true
}

func pythonString(value any) string {
	switch value := value.(type) {
	case nil:
		return "None"
	case string:
		return value
	case bool:
		if value {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprint(value)
	}
}

func jsonString(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return pythonString(value)
	}
	return string(bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}))
}

func uncorrected(text string) int { return CountTokens(text, false) }

// CountMessageTokens estimates OpenAI and Anthropic message structures.
func CountMessageTokens(messages any, applyClaudeCorrection ...bool) int {
	items, ok := sequence(messages)
	if !ok || len(items) == 0 {
		return 0
	}
	total := 0
	for _, rawMessage := range items {
		message, ok := object(rawMessage)
		if !ok {
			continue
		}
		total += 4
		if role, ok := message["role"].(string); ok {
			total += uncorrected(role)
		}
		if content := message["content"]; content != nil {
			switch content := content.(type) {
			case string:
				if content != "" {
					total += uncorrected(content)
				}
			default:
				if blocks, ok := sequence(content); ok {
					for _, block := range blocks {
						total += countContentBlock(block)
					}
				}
			}
		}
		if calls, ok := sequence(message["tool_calls"]); ok {
			for _, rawCall := range calls {
				call, ok := object(rawCall)
				if !ok {
					continue
				}
				total += 4
				function, _ := object(call["function"])
				total += uncorrected(stringField(function, "name"))
				total += uncorrected(stringField(function, "arguments"))
			}
		}
		if id := stringField(message, "tool_call_id"); id != "" {
			total += uncorrected(id)
		}
	}
	total += 3
	return applyCorrection(total, correctedOption(applyClaudeCorrection))
}

func countContentBlock(raw any) int {
	block, ok := object(raw)
	if !ok {
		return uncorrected(pythonString(raw))
	}
	switch stringField(block, "type") {
	case "text":
		return uncorrected(stringField(block, "text"))
	case "image_url", "image":
		return 100
	case "tool_use":
		return uncorrected(stringField(block, "id")) +
			uncorrected(stringField(block, "name")) + uncorrected(jsonString(defaultObject(block["input"])))
	case "tool_result":
		total := uncorrected(stringField(block, "tool_use_id"))
		if value, exists := block["is_error"]; exists && value != nil {
			total += uncorrected(pythonString(value))
		}
		content := block["content"]
		if text, ok := content.(string); ok {
			return total + uncorrected(text)
		}
		if results, ok := sequence(content); ok {
			for _, rawResult := range results {
				result, ok := object(rawResult)
				if !ok {
					total += uncorrected(pythonString(rawResult))
					continue
				}
				switch stringField(result, "type") {
				case "text":
					total += uncorrected(stringField(result, "text"))
				case "image_url", "image":
					total += 100
				}
			}
			return total
		}
		if content != nil {
			total += uncorrected(pythonString(content))
		}
		return total
	default:
		return uncorrected(jsonString(block))
	}
}

func defaultObject(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func stringField(object map[string]any, key string) string {
	if value, ok := object[key].(string); ok {
		return value
	}
	return ""
}

func applyCorrection(tokens int, enabled bool) int {
	if enabled {
		return int(float64(tokens) * ClaudeCorrectionFactor)
	}
	return tokens
}

// CountToolsTokens estimates both OpenAI function wrappers and flat Anthropic tools.
func CountToolsTokens(tools any, applyClaudeCorrection ...bool) int {
	items, ok := sequence(tools)
	if !ok || len(items) == 0 {
		return 0
	}
	total := 0
	for _, rawTool := range items {
		tool, ok := object(rawTool)
		if !ok {
			continue
		}
		total += 4
		payload := tool
		if stringField(tool, "type") == "function" {
			if function, ok := object(tool["function"]); ok {
				payload = function
			}
		}
		total += uncorrected(stringField(payload, "name"))
		total += uncorrected(stringField(payload, "description"))
		parameters, exists := payload["input_schema"]
		if !exists || parameters == nil {
			parameters, exists = payload["parameters"]
		}
		if exists && parameters != nil {
			total += uncorrected(jsonString(parameters))
		}
	}
	return applyCorrection(total, correctedOption(applyClaudeCorrection))
}

// CountSystemTokens estimates a string or Anthropic system block list.
func CountSystemTokens(systemPrompt any, applyClaudeCorrection ...bool) int {
	if systemPrompt == nil {
		return 0
	}
	total := 0
	switch prompt := systemPrompt.(type) {
	case string:
		if prompt == "" {
			return 0
		}
		total = uncorrected(prompt)
	default:
		if blocks, ok := sequence(prompt); ok {
			if len(blocks) == 0 {
				return 0
			}
			for _, rawBlock := range blocks {
				if block, ok := object(rawBlock); ok {
					total += uncorrected(stringField(block, "text"))
					if cache, exists := block["cache_control"]; exists && cache != nil {
						total += uncorrected(jsonString(cache))
					}
				} else {
					total += uncorrected(pythonString(rawBlock))
				}
			}
		} else {
			total = uncorrected(pythonString(prompt))
		}
	}
	return applyCorrection(total, correctedOption(applyClaudeCorrection))
}

type RequestTokenEstimate struct {
	MessagesTokens int `json:"messages_tokens"`
	ToolsTokens    int `json:"tools_tokens"`
	SystemTokens   int `json:"system_tokens"`
	TotalTokens    int `json:"total_tokens"`
}

func EstimateRequestTokens(messages, tools, systemPrompt any, applyClaudeCorrection ...bool) RequestTokenEstimate {
	correct := correctedOption(applyClaudeCorrection)
	result := RequestTokenEstimate{
		MessagesTokens: CountMessageTokens(messages, correct),
		ToolsTokens:    CountToolsTokens(tools, correct),
		SystemTokens:   CountSystemTokens(systemPrompt, correct),
	}
	result.TotalTokens = result.MessagesTokens + result.ToolsTokens + result.SystemTokens
	return result
}

// UsageFromContext converts Kiro's total context usage percentage into API token
// counts. A missing or zero percentage leaves the locally estimated prompt count
// intact, matching the Python gateway's fallback behavior.
func UsageFromContext(contextUsagePercentage float64, completionTokens, fallbackPromptTokens int, cache *model.Cache, modelID string) (promptTokens, totalTokens int) {
	if contextUsagePercentage > 0 && cache != nil {
		totalTokens = int(contextUsagePercentage / 100 * float64(cache.MaxInputTokens(modelID)))
		promptTokens = totalTokens - completionTokens
		if promptTokens < 0 {
			promptTokens = 0
		}
		return promptTokens, totalTokens
	}
	return fallbackPromptTokens, fallbackPromptTokens + completionTokens
}
