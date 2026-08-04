package gatewayerr

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEnhanceKiroError(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		reason   string
		original string
		message  string
	}{
		{"content length", map[string]any{"message": "Input is too long.", "reason": ReasonContentLength}, ReasonContentLength, "Input is too long.", "Model context limit reached. Conversation size exceeds model capacity."},
		{"monthly", map[string]any{"message": "You have reached the limit.", "reason": ReasonMonthlyCount}, ReasonMonthlyCount, "You have reached the limit.", "Monthly request limit exceeded. Account has reached its monthly quota."},
		{"invalid model", map[string]any{"message": "Model not found.", "reason": ReasonInvalidModel}, ReasonInvalidModel, "Model not found.", "Invalid model ID or insufficient subscription level to use it."},
		{"unknown reason", map[string]any{"message": "Something went wrong.", "reason": "UNKNOWN_FUTURE_ERROR"}, "UNKNOWN_FUTURE_ERROR", "Something went wrong.", "Something went wrong. (reason: UNKNOWN_FUTURE_ERROR)"},
		{"missing reason", map[string]any{"message": "An error occurred."}, ReasonUnknown, "An error occurred.", "An error occurred."},
		{"explicit unknown", map[string]any{"message": "Unknown error.", "reason": ReasonUnknown}, ReasonUnknown, "Unknown error.", "Unknown error."},
		{"empty", map[string]any{}, ReasonUnknown, "Unknown error", "Unknown error"},
		{"missing message", map[string]any{"reason": ReasonContentLength}, ReasonContentLength, "Unknown error", "Model context limit reached. Conversation size exceeds model capacity."},
		{"empty message preserved", map[string]any{"message": "", "reason": "SOME_ERROR"}, "SOME_ERROR", "", " (reason: SOME_ERROR)"},
		{"nulls", map[string]any{"message": nil, "reason": nil}, ReasonUnknown, "Unknown error", "Unknown error"},
		{"case sensitive", map[string]any{"message": "Error.", "reason": "content_length_exceeds_threshold"}, "content_length_exceeds_threshold", "Error.", "Error. (reason: content_length_exceeds_threshold)"},
		{"improper missing reason", map[string]any{"message": "Improperly formed request."}, ReasonUnknown, "Improperly formed request.", "Kiro API rejected the request. If problem persists, open issue with info and attached debug logs at:https://github.com/jwadow/kiro-gateway/issues"},
		{"improper null reason", map[string]any{"message": "Improperly formed request.", "reason": nil}, ReasonUnknown, "Improperly formed request.", "Kiro API rejected the request. If problem persists, open issue with info and attached debug logs at:https://github.com/jwadow/kiro-gateway/issues"},
		{"improper real reason", map[string]any{"message": "Improperly formed request.", "reason": "VALIDATION_ERROR"}, "VALIDATION_ERROR", "Improperly formed request.", "Improperly formed request. (reason: VALIDATION_ERROR)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EnhanceKiroError(test.input)
			want := KiroErrorInfo{Reason: test.reason, UserMessage: test.message, OriginalMessage: test.original}
			if got != want {
				t.Fatalf("EnhanceKiroError() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestClassifyAccountError(t *testing.T) {
	tests := []struct {
		status int
		reason string
		want   AccountErrorType
	}{
		{402, ReasonMonthlyCount, AccountErrorRecoverable}, {402, "", AccountErrorRecoverable},
		{403, "", AccountErrorRecoverable}, {429, "", AccountErrorRecoverable},
		{400, ReasonInvalidModel, AccountErrorRecoverable},
		{400, ReasonContentLength, AccountErrorFatal}, {400, "", AccountErrorFatal},
		{400, "UNKNOWN_VALIDATION_ERROR", AccountErrorFatal}, {401, "", AccountErrorFatal},
		{422, "", AccountErrorFatal}, {500, "", AccountErrorFatal}, {503, "", AccountErrorFatal}, {418, "", AccountErrorFatal},
	}
	for _, test := range tests {
		if got := ClassifyAccountError(test.status, test.reason); got != test.want {
			t.Errorf("ClassifyAccountError(%d, %q) = %q, want %q", test.status, test.reason, got, test.want)
		}
	}
}

func marked(kind NetworkErrorKind, message string) error {
	return &NetworkError{Kind: kind, Type: "ConnectError", Msg: message}
}

func TestClassifyNetworkError(t *testing.T) {
	errno := 11001
	tests := []struct {
		name      string
		err       error
		category  ErrorCategory
		message   string
		retryable bool
		status    int
	}{
		{"dns", &NetworkError{Kind: KindConnect, Type: "ConnectError", Msg: "Connection failed", Cause: &net.DNSError{Err: "getaddrinfo failed", Name: "provider.invalid"}, Errno: &errno}, DNSResolution, "DNS resolution failed - cannot resolve the provider's domain name.", true, 502},
		{"refused", marked(KindConnect, "Connection refused"), ConnectionRefused, "Connection refused - the server is not accepting connections.", true, 502},
		{"refused errno text", marked(KindConnect, "[Errno 111] ECONNREFUSED"), ConnectionRefused, "Connection refused - the server is not accepting connections.", true, 502},
		{"reset", marked(KindConnect, "Connection reset by peer"), ConnectionReset, "Connection reset - the server closed the connection unexpectedly.", true, 502},
		{"unreachable", marked(KindConnect, "No route to host"), NetworkUnreachable, "Network unreachable - cannot reach the server's network.", true, 502},
		{"ssl", marked(KindConnect, "SSL handshake failed"), SSLError, "SSL/TLS error - secure connection could not be established.", false, 502},
		{"certificate", marked(KindConnect, "Certificate verification failed"), SSLError, "SSL/TLS error - secure connection could not be established.", false, 502},
		{"connect fallback", marked(KindConnect, "All connection attempts failed"), Unknown, "Connection failed - unable to establish connection to the server.", true, 502},
		{"connect timeout", marked(KindConnectTimeout, "Connection timeout"), TimeoutConnect, "Connection timeout - server did not respond to connection attempt.", true, 504},
		{"read timeout", marked(KindReadTimeout, "Read timeout"), TimeoutRead, "Read timeout - server stopped responding during data transfer.", true, 504},
		{"generic timeout", marked(KindTimeout, "Timeout"), TimeoutRead, "Request timeout - operation took too long to complete.", true, 504},
		{"proxy", marked(KindProxy, "Proxy failed"), ProxyError, "Proxy connection failed - cannot connect through the configured proxy.", true, 502},
		{"redirect", marked(KindRedirects, "Too many redirects"), TooManyRedirects, "Too many redirects - the server is redirecting in a loop.", false, 502},
		{"request fallback", marked(KindRequest, "Unknown network error"), Unknown, "Network request failed due to an unexpected error.", true, 502},
		{"other", errors.New("Something went wrong"), Unknown, "An unexpected error occurred.", true, 500},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClassifyNetworkError(test.err)
			if got.Category != test.category || got.UserMessage != test.message || got.IsRetryable != test.retryable || got.SuggestedHTTPCode != test.status {
				t.Fatalf("classification = %#v", got)
			}
			if len(got.TroubleshootingSteps) == 0 || got.TechnicalDetails == "" {
				t.Fatalf("missing diagnostics: %#v", got)
			}
		})
	}
	got := ClassifyNetworkError(&NetworkError{Kind: KindConnect, Type: "ConnectError", Msg: "failed", Errno: &errno})
	if !strings.Contains(got.TechnicalDetails, "errno: 11001") {
		t.Fatalf("DNS technical details = %q", got.TechnicalDetails)
	}
}

func TestFormatErrorForUser(t *testing.T) {
	input := NetworkErrorInfo{Category: DNSResolution, UserMessage: "DNS failed", TroubleshootingSteps: []string{"Step 1", "Step 2"}, TechnicalDetails: "Technical info", IsRetryable: true, SuggestedHTTPCode: 502}

	openAI, ok := FormatErrorForUser(input, "openai", true).(OpenAIErrorEnvelope)
	if !ok || openAI.Error.Type != "connectivity_error" || openAI.Error.Code != "dns_resolution" || openAI.Error.Param != nil || openAI.Error.Message != "DNS failed\n\nTroubleshooting steps:\n1. Step 1\n2. Step 2" {
		t.Fatalf("OpenAI envelope = %#v", openAI)
	}
	encoded, err := json.Marshal(openAI)
	if err != nil || !strings.Contains(string(encoded), `"param":null`) {
		t.Fatalf("OpenAI JSON = %s, %v", encoded, err)
	}

	anthropic, ok := FormatErrorForUser(input, "anthropic", false).(AnthropicErrorEnvelope)
	if !ok || anthropic.Type != "error" || anthropic.Error.Type != "connectivity_error" || anthropic.Error.Message != "DNS failed" {
		t.Fatalf("Anthropic envelope = %#v", anthropic)
	}
	generic, ok := FormatErrorForUser(input, "other", false).(GenericErrorEnvelope)
	if !ok || generic.Error.Category != "dns_resolution" || generic.Error.TechnicalDetails != "Technical info" {
		t.Fatalf("generic envelope = %#v", generic)
	}
	if GetShortErrorMessage(input) != "DNS failed" {
		t.Fatal("short message changed")
	}
}

func TestSanitizeValidationErrors(t *testing.T) {
	input := []map[string]any{{
		"type": "json_invalid", "loc": []any{"body", []byte("field")},
		"input": []byte{'{', 0xff, '}'}, "other": map[string]any{"bytes": []byte("preserved")},
	}}
	got := SanitizeValidationErrors(input)
	if got[0]["input"] != "{�}" || !reflect.DeepEqual(got[0]["loc"], []any{"body", "field"}) {
		t.Fatalf("sanitized = %#v", got)
	}
	if _, ok := got[0]["other"].(map[string]any)["bytes"].([]byte); !ok {
		t.Fatal("nested maps should match Python's non-recursive behavior")
	}
	if _, ok := input[0]["input"].([]byte); !ok {
		t.Fatal("input was mutated")
	}
}

func TestValidationErrorEnvelope(t *testing.T) {
	body := []byte(strings.Repeat("界", 501))
	envelope := NewValidationErrorEnvelope([]map[string]any{{"input": []byte("value")}}, body)
	if utf8.RuneCountInString(envelope.Body) != 500 || !utf8.ValidString(envelope.Body) {
		t.Fatalf("body has %d runes and valid=%v", utf8.RuneCountInString(envelope.Body), utf8.ValidString(envelope.Body))
	}
	if envelope.Detail[0]["input"] != "value" {
		t.Fatalf("detail = %#v", envelope.Detail)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil || !strings.Contains(string(encoded), `"detail"`) || !strings.Contains(string(encoded), `"body"`) {
		t.Fatalf("validation JSON = %s, %v", encoded, err)
	}
}
