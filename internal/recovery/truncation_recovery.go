// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package recovery

const truncationToolNotice = "[API Limitation] Your tool call was truncated by the upstream API due to output size limits.\n\n" +
	"If the tool result below shows an error or unexpected behavior, this is likely a CONSEQUENCE of the truncation, " +
	"not the root cause. The tool call itself was cut off before it could be fully transmitted.\n\n" +
	"Repeating the exact same operation will be truncated again. Consider adapting your approach."

const truncationUserNotice = "[System Notice] Your previous response was truncated by the API due to " +
	"output size limitations. This is not an error on your part. " +
	"If you need to continue, please adapt your approach rather than repeating the same output."

func ShouldInjectRecovery(enabled bool) bool { return enabled }

// GenerateTruncationToolResult returns the exact synthetic notice and unified
// dictionary shape used by the Python gateway. The diagnostic arguments are
// intentionally not included in the model-facing message.
func GenerateTruncationToolResult(toolName, toolUseID string, truncationInfo map[string]any) map[string]any {
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolUseID,
		"content":     truncationToolNotice,
		"is_error":    true,
	}
}

func GenerateTruncationUserMessage() string { return truncationUserNotice }
