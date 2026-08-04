// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package gatewayerr classifies upstream and network failures and builds the
// public error envelopes returned by the gateway's compatibility APIs.
package gatewayerr

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	ReasonContentLength = "CONTENT_LENGTH_EXCEEDS_THRESHOLD"
	ReasonMonthlyCount  = "MONTHLY_REQUEST_COUNT"
	ReasonInvalidModel  = "INVALID_MODEL_ID"
	ReasonUnknown       = "UNKNOWN"
)

// KiroErrorInfo contains an enhanced message and the unmodified upstream details.
type KiroErrorInfo struct {
	Reason          string
	UserMessage     string
	OriginalMessage string
}

// EnhanceKiroError converts a parsed Kiro API error into its user-facing form.
func EnhanceKiroError(upstream map[string]any) KiroErrorInfo {
	original := stringField(upstream, "message", "Unknown error")
	reason := stringField(upstream, "reason", ReasonUnknown)

	var message string
	switch reason {
	case ReasonContentLength:
		message = "Model context limit reached. Conversation size exceeds model capacity."
	case ReasonMonthlyCount:
		message = "Monthly request limit exceeded. Account has reached its monthly quota."
	case ReasonInvalidModel:
		message = "Invalid model ID or insufficient subscription level to use it."
	default:
		if original == "Improperly formed request." && (reason == ReasonUnknown || reason == "null") {
			message = "Kiro API rejected the request. If problem persists, open issue with info and attached debug logs at:" +
				"https://github.com/jwadow/kiro-gateway/issues"
		} else if _, present := upstream["reason"]; present && reason != ReasonUnknown {
			message = fmt.Sprintf("%s (reason: %s)", original, reason)
		} else {
			message = original
		}
	}
	return KiroErrorInfo{Reason: reason, UserMessage: message, OriginalMessage: original}
}

func stringField(values map[string]any, key, fallback string) string {
	value, ok := values[key]
	if !ok || value == nil {
		return fallback
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

// AccountErrorType controls whether account failover should be attempted.
type AccountErrorType string

const (
	AccountErrorFatal       AccountErrorType = "fatal"
	AccountErrorRecoverable AccountErrorType = "recoverable"
)

// ClassifyAccountError reproduces the Kiro account failover decision table.
func ClassifyAccountError(statusCode int, reason string) AccountErrorType {
	if statusCode == 402 || statusCode == 403 || statusCode == 429 {
		return AccountErrorRecoverable
	}
	if statusCode == 400 && reason == ReasonInvalidModel {
		return AccountErrorRecoverable
	}
	return AccountErrorFatal
}

// ErrorCategory identifies the network failure presented to clients.
type ErrorCategory string

const (
	DNSResolution      ErrorCategory = "dns_resolution"
	ConnectionRefused  ErrorCategory = "connection_refused"
	ConnectionReset    ErrorCategory = "connection_reset"
	NetworkUnreachable ErrorCategory = "network_unreachable"
	TimeoutConnect     ErrorCategory = "timeout_connect"
	TimeoutRead        ErrorCategory = "timeout_read"
	SSLError           ErrorCategory = "ssl_error"
	ProxyError         ErrorCategory = "proxy_error"
	TooManyRedirects   ErrorCategory = "too_many_redirects"
	Unknown            ErrorCategory = "unknown"
)

// NetworkErrorKind supplies distinctions represented by concrete httpx classes
// in Python but not universal in Go transports.
type NetworkErrorKind uint8

const (
	KindOther NetworkErrorKind = iota
	KindRequest
	KindConnect
	KindConnectTimeout
	KindReadTimeout
	KindTimeout
	KindProxy
	KindRedirects
)

// NetworkError can be used by transports to preserve a failure's phase.
type NetworkError struct {
	Kind  NetworkErrorKind
	Type  string
	Msg   string
	Cause error
	Errno *int
}

func (e *NetworkError) Error() string { return e.Msg }
func (e *NetworkError) Unwrap() error { return e.Cause }
func (e *NetworkError) Timeout() bool {
	return e.Kind == KindConnectTimeout || e.Kind == KindReadTimeout || e.Kind == KindTimeout
}

// NetworkErrorInfo is the complete classification and remediation contract.
type NetworkErrorInfo struct {
	Category             ErrorCategory
	UserMessage          string
	TroubleshootingSteps []string
	TechnicalDetails     string
	IsRetryable          bool
	SuggestedHTTPCode    int
}

// ClassifyNetworkError classifies phase-aware NetworkError and standard Go errors.
func ClassifyNetworkError(err error) NetworkErrorInfo {
	technical := technicalDetails(err)
	var marked *NetworkError
	if errors.As(err, &marked) {
		switch marked.Kind {
		case KindProxy:
			return proxyInfo(technical)
		case KindRedirects:
			return redirectsInfo(technical)
		case KindConnectTimeout:
			return connectTimeoutInfo(technical)
		case KindReadTimeout:
			return readTimeoutInfo(technical)
		case KindTimeout:
			return timeoutInfo(technical)
		case KindConnect:
			return classifyConnectError(err, technical, marked.Errno)
		case KindRequest:
			return requestInfo(technical)
		}
	}

	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dnsInfo(technical, nil)
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return timeoutInfo(technical)
	}
	return unexpectedInfo(technical)
}

func classifyConnectError(err error, technical string, explicitErrno *int) NetworkErrorInfo {
	var dns *net.DNSError
	if errors.As(err, &dns) || explicitErrno != nil {
		return dnsInfo(technical, explicitErrno)
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "Connection refused") || strings.Contains(message, "ECONNREFUSED"):
		return info(ConnectionRefused, "Connection refused - the server is not accepting connections.", []string{
			"The service may be temporarily down", "Check if the service is running and accessible",
			"Verify firewall is not blocking the connection", "Try again in a few moments",
		}, technical, true, 502)
	case strings.Contains(message, "Connection reset") || strings.Contains(message, "ECONNRESET"):
		return info(ConnectionReset, "Connection reset - the server closed the connection unexpectedly.", []string{
			"This is usually a temporary server issue", "Try again in a few moments",
			"Check if VPN/proxy is interfering with the connection", "Verify network stability",
		}, technical, true, 502)
	case strings.Contains(message, "Network is unreachable") || strings.Contains(message, "No route to host") || strings.Contains(message, "ENETUNREACH"):
		return info(NetworkUnreachable, "Network unreachable - cannot reach the server's network.", []string{
			"Check your internet connection", "Verify network adapter is enabled and working",
			"Check routing table if using VPN", "Try disabling VPN temporarily", "Restart network adapter or router",
		}, technical, true, 502)
	case strings.Contains(message, "SSL") || strings.Contains(message, "TLS") || strings.Contains(strings.ToLower(message), "certificate"):
		return info(SSLError, "SSL/TLS error - secure connection could not be established.", []string{
			"Check system date and time (incorrect time causes SSL errors)", "Update SSL certificates on your system",
			"Check if antivirus/firewall is intercepting HTTPS traffic", "Verify the server's SSL certificate is valid",
		}, technical, false, 502)
	default:
		return info(Unknown, "Connection failed - unable to establish connection to the server.", []string{
			"Check your internet connection", "Verify firewall/antivirus settings", "Try disabling VPN temporarily",
			"Check if the service is accessible from other devices",
		}, technical, true, 502)
	}
}

func dnsInfo(technical string, errno *int) NetworkErrorInfo {
	if errno != nil {
		technical += fmt.Sprintf(" (errno: %d)", *errno)
	}
	return info(DNSResolution, "DNS resolution failed - cannot resolve the provider's domain name.", []string{
		"Check your internet connection",
		"Try changing DNS servers to Google DNS (8.8.8.8, 8.8.4.4) or Cloudflare (1.1.1.1, 1.0.0.1)",
		"Temporarily disable VPN if you're using one", "Check if firewall/antivirus is blocking DNS requests",
		"Verify the domain name is correct and the service is operational",
	}, technical, true, 502)
}

func connectTimeoutInfo(technical string) NetworkErrorInfo {
	return info(TimeoutConnect, "Connection timeout - server did not respond to connection attempt.", []string{
		"Check your internet connection speed", "The server may be overloaded or slow to respond",
		"Try again in a few moments", "Check if firewall is delaying connections",
	}, technical, true, 504)
}

func readTimeoutInfo(technical string) NetworkErrorInfo {
	return info(TimeoutRead, "Read timeout - server stopped responding during data transfer.", []string{
		"The server may be processing a complex request", "Check your internet connection stability",
		"Try again with a simpler request", "The service may be experiencing high load",
	}, technical, true, 504)
}

func timeoutInfo(technical string) NetworkErrorInfo {
	return info(TimeoutRead, "Request timeout - operation took too long to complete.", []string{
		"Check your internet connection", "The server may be slow or overloaded", "Try again in a few moments",
	}, technical, true, 504)
}

func proxyInfo(technical string) NetworkErrorInfo {
	return info(ProxyError, "Proxy connection failed - cannot connect through the configured proxy.", []string{
		"Check proxy configuration (HTTP_PROXY, HTTPS_PROXY environment variables)", "Verify proxy server is accessible",
		"Try disabling proxy temporarily", "Check proxy authentication credentials if required",
	}, technical, true, 502)
}

func redirectsInfo(technical string) NetworkErrorInfo {
	return info(TooManyRedirects, "Too many redirects - the server is redirecting in a loop.", []string{
		"This is likely a server-side configuration issue", "Try accessing the service directly without the gateway",
		"Contact the service provider if the issue persists",
	}, technical, false, 502)
}

func requestInfo(technical string) NetworkErrorInfo {
	return info(Unknown, "Network request failed due to an unexpected error.", []string{
		"Check your internet connection", "Verify firewall/antivirus settings", "Try again in a few moments",
		"Check the debug logs for more details",
	}, technical, true, 502)
}

func unexpectedInfo(technical string) NetworkErrorInfo {
	return info(Unknown, "An unexpected error occurred.", []string{
		"Check the debug logs for details", "Try again in a few moments", "Report this issue if it persists",
	}, technical, true, 500)
}

func info(category ErrorCategory, message string, steps []string, technical string, retryable bool, status int) NetworkErrorInfo {
	return NetworkErrorInfo{category, message, steps, technical, retryable, status}
}

func technicalDetails(err error) string {
	if err == nil {
		return "<nil>: <nil>"
	}
	name := reflect.TypeOf(err).String()
	var marked *NetworkError
	if errors.As(err, &marked) && marked.Type != "" {
		name = marked.Type
	}
	return fmt.Sprintf("%s: %s", name, err)
}

// OpenAIErrorEnvelope is the OpenAI-compatible public error shape.
type OpenAIErrorEnvelope struct {
	Error OpenAIError `json:"error"`
}
type OpenAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Param   *string `json:"param"`
}

// OpenAIUpstreamErrorEnvelope matches routes_openai's Kiro HTTP error shape.
// It intentionally has no param field and uses the upstream HTTP status as a
// numeric code.
type OpenAIUpstreamErrorEnvelope struct {
	Error OpenAIUpstreamError `json:"error"`
}
type OpenAIUpstreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    int    `json:"code"`
}

// AnthropicErrorEnvelope is the Anthropic-compatible public error shape.
type AnthropicErrorEnvelope struct {
	Type  string         `json:"type"`
	Error AnthropicError `json:"error"`
}
type AnthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// GenericErrorEnvelope is the fallback shape, including technical details.
type GenericErrorEnvelope struct {
	Error GenericError `json:"error"`
}
type GenericError struct {
	Type             string `json:"type"`
	Category         string `json:"category"`
	Message          string `json:"message"`
	TechnicalDetails string `json:"technical_details"`
}

// FormatErrorForUser returns OpenAI, Anthropic, or generic public envelopes.
func FormatErrorForUser(errorInfo NetworkErrorInfo, formatType string, includeTroubleshooting bool) any {
	message := formatMessage(errorInfo, includeTroubleshooting)
	switch formatType {
	case "openai":
		return OpenAIErrorEnvelope{Error: OpenAIError{Message: message, Type: "connectivity_error", Code: string(errorInfo.Category)}}
	case "anthropic":
		return AnthropicErrorEnvelope{Type: "error", Error: AnthropicError{Type: "connectivity_error", Message: message}}
	default:
		return GenericErrorEnvelope{Error: GenericError{Type: "connectivity_error", Category: string(errorInfo.Category), Message: message, TechnicalDetails: errorInfo.TechnicalDetails}}
	}
}

func formatMessage(errorInfo NetworkErrorInfo, include bool) string {
	message := errorInfo.UserMessage
	if include && len(errorInfo.TroubleshootingSteps) > 0 {
		message += "\n\nTroubleshooting steps:\n"
		for index, step := range errorInfo.TroubleshootingSteps {
			message += fmt.Sprintf("%d. %s\n", index+1, step)
		}
	}
	return strings.TrimSpace(message)
}

// GetShortErrorMessage returns the single-line message used for logs.
func GetShortErrorMessage(errorInfo NetworkErrorInfo) string { return errorInfo.UserMessage }

// SanitizeValidationErrors makes []byte values JSON-safe. It mirrors Python's
// one-level list conversion rather than recursively changing arbitrary maps.
func SanitizeValidationErrors(validationErrors []map[string]any) []map[string]any {
	sanitized := make([]map[string]any, 0, len(validationErrors))
	for _, validationError := range validationErrors {
		copy := make(map[string]any, len(validationError))
		for key, value := range validationError {
			copy[key] = sanitizeValue(value)
		}
		sanitized = append(sanitized, copy)
	}
	return sanitized
}

func sanitizeValue(value any) any {
	switch value := value.(type) {
	case []byte:
		return strings.ToValidUTF8(string(value), string(utf8.RuneError))
	case []any:
		out := make([]any, len(value))
		for index, item := range value {
			if bytes, ok := item.([]byte); ok {
				out[index] = strings.ToValidUTF8(string(bytes), string(utf8.RuneError))
			} else {
				out[index] = item
			}
		}
		return out
	default:
		return value
	}
}

// ValidationErrorEnvelope is the public 422 response body.
type ValidationErrorEnvelope struct {
	Detail []map[string]any `json:"detail"`
	Body   string           `json:"body"`
}

// NewValidationErrorEnvelope sanitizes details, replaces malformed UTF-8, and
// truncates the body to 500 Unicode characters.
func NewValidationErrorEnvelope(validationErrors []map[string]any, body []byte) ValidationErrorEnvelope {
	text := []rune(strings.ToValidUTF8(string(body), string(utf8.RuneError)))
	if len(text) > 500 {
		text = text[:500]
	}
	return ValidationErrorEnvelope{Detail: SanitizeValidationErrors(validationErrors), Body: string(text)}
}
