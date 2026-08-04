// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server implements the OpenAI- and Anthropic-compatible HTTP gateway.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/accounts"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	"github.com/jwadow/kiro-gateway-go/internal/convert"
	"github.com/jwadow/kiro-gateway-go/internal/debuglog"
	"github.com/jwadow/kiro-gateway-go/internal/gatewayerr"
	"github.com/jwadow/kiro-gateway-go/internal/mcp"
	"github.com/jwadow/kiro-gateway-go/internal/recovery"
	"github.com/jwadow/kiro-gateway-go/internal/stream"
	"github.com/jwadow/kiro-gateway-go/internal/tokenizer"
	"github.com/jwadow/kiro-gateway-go/internal/upstream"
)

const requestBodyLimit = 32 << 20

type Server struct {
	cfg          config.Config
	accounts     *accounts.Manager
	client       *http.Client
	upstreamHTTP *http.Client
	upstreamMu   sync.Mutex
	upstreams    map[string]*upstream.Client
	log          *slog.Logger
}

type Options struct {
	Accounts *accounts.Manager
	Client   *http.Client
	Logger   *slog.Logger
}

func New(cfg config.Config, opts Options) (*Server, error) {
	manager := opts.Accounts
	var err error
	if manager == nil {
		manager, err = accounts.New(cfg)
		if err != nil {
			return nil, err
		}
	}
	client := opts.Client
	if client == nil {
		client, err = upstream.NewHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}
	upstreamHTTP := client
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, accounts: manager, client: client, upstreamHTTP: upstreamHTTP, upstreams: make(map[string]*upstream.Client), log: logger}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.root)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /openapi.json", s.openapi)
	mux.HandleFunc("GET /docs", s.swaggerDocs)
	mux.HandleFunc("GET /redoc", s.redoc)
	mux.HandleFunc("GET /v1/models", s.protectOpenAI(s.models))
	mux.HandleFunc("POST /v1/chat/completions", s.protectOpenAI(s.chat))
	mux.HandleFunc("POST /v1/messages", s.protectAnthropic(s.messages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.protectAnthropic(s.countTokens))
	mux.HandleFunc("/", s.routeFallback)
	return s.cors(s.logging(mux))
}

func (s *Server) RunStateSaver(ctx context.Context) error { return s.accounts.RunStateSaver(ctx) }
func (s *Server) SaveState() error                        { return s.accounts.SaveState() }
func (s *Server) CloseIdleConnections() {
	if s.upstreamHTTP != nil {
		s.upstreamHTTP.CloseIdleConnections()
	}
}
func (s *Server) Initialize(ctx context.Context) error {
	_, err := s.accounts.InitializeFirstWorking(ctx)
	return err
}

func (s *Server) root(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "message": "Kiro Gateway is running", "version": config.AppVersion})
}
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "healthy", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "version": config.AppVersion})
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, openAPISchema())
}

func (s *Server) swaggerDocs(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Kiro Gateway - Swagger UI</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css"></head>
<body><div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url:'/openapi.json',dom_id:'#swagger-ui',deepLinking:true});</script></body></html>`)
}

func (s *Server) redoc(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Kiro Gateway - ReDoc</title>
<meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body><redoc spec-url="/openapi.json"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script></body></html>`)
}

func (s *Server) routeFallback(w http.ResponseWriter, r *http.Request) {
	if allowed := allowedMethod(r.URL.Path); allowed != "" {
		w.Header().Set("Allow", allowed)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "Method Not Allowed"})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Not Found"})
}

func allowedMethod(path string) string {
	switch path {
	case "/", "/health", "/openapi.json", "/docs", "/redoc", "/v1/models":
		return http.MethodGet
	case "/v1/chat/completions", "/v1/messages", "/v1/messages/count_tokens":
		return http.MethodPost
	default:
		return ""
	}
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func openAPISchema() map[string]any {
	openAIKey := []any{map[string]any{"OpenAIAuth": []any{}}}
	anthropicKeys := []any{map[string]any{"AnthropicAPIKey": []any{}}, map[string]any{"OpenAIAuth": []any{}}}
	jsonBody := func(schema map[string]any) map[string]any {
		return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
	}
	ref := func(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }
	response := func(description string) map[string]any { return map[string]any{"description": description} }
	return map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": config.AppTitle, "description": config.AppDescription, "version": config.AppVersion},
		"paths": map[string]any{
			"/":                         map[string]any{"get": map[string]any{"summary": "Root", "responses": map[string]any{"200": response("Successful Response")}}},
			"/health":                   map[string]any{"get": map[string]any{"summary": "Health", "responses": map[string]any{"200": response("Successful Response")}}},
			"/v1/models":                map[string]any{"get": map[string]any{"summary": "Get Models", "security": openAIKey, "responses": map[string]any{"200": response("Successful Response")}}},
			"/v1/chat/completions":      map[string]any{"post": map[string]any{"summary": "Chat Completions", "security": openAIKey, "requestBody": jsonBody(ref("ChatCompletionRequest")), "responses": map[string]any{"200": response("Successful Response"), "422": response("Validation Error")}}},
			"/v1/messages":              map[string]any{"post": map[string]any{"summary": "Messages", "security": anthropicKeys, "requestBody": jsonBody(ref("AnthropicMessagesRequest")), "responses": map[string]any{"200": response("Successful Response"), "422": response("Validation Error")}}},
			"/v1/messages/count_tokens": map[string]any{"post": map[string]any{"summary": "Count Tokens", "security": anthropicKeys, "requestBody": jsonBody(ref("AnthropicTokenCountRequest")), "responses": map[string]any{"200": response("Successful Response"), "422": response("Validation Error")}}},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"OpenAIAuth":      map[string]any{"type": "apiKey", "in": "header", "name": "Authorization"},
				"AnthropicAPIKey": map[string]any{"type": "apiKey", "in": "header", "name": "x-api-key"},
			},
			"schemas": map[string]any{
				"ChatMessage":                map[string]any{"type": "object", "required": []any{"role"}, "properties": map[string]any{"role": map[string]any{"type": "string"}, "content": map[string]any{}, "name": map[string]any{"type": []any{"string", "null"}}, "tool_calls": map[string]any{"type": []any{"array", "null"}, "items": map[string]any{}}, "tool_call_id": map[string]any{"type": []any{"string", "null"}}}, "additionalProperties": true},
				"ChatCompletionRequest":      map[string]any{"type": "object", "required": []any{"model", "messages"}, "properties": map[string]any{"model": map[string]any{"type": "string"}, "messages": map[string]any{"type": "array", "minItems": 1, "items": ref("ChatMessage")}, "stream": map[string]any{"type": "boolean", "default": false}, "n": map[string]any{"type": []any{"integer", "null"}, "default": 1}, "max_tokens": map[string]any{"type": []any{"integer", "null"}}, "max_completion_tokens": map[string]any{"type": []any{"integer", "null"}}, "tools": map[string]any{"type": []any{"array", "null"}, "items": map[string]any{"type": "object", "additionalProperties": true}}}, "additionalProperties": true},
				"AnthropicMessage":           map[string]any{"type": "object", "required": []any{"role", "content"}, "properties": map[string]any{"role": map[string]any{"type": "string"}, "content": map[string]any{}}, "additionalProperties": true},
				"AnthropicMessagesRequest":   map[string]any{"type": "object", "required": []any{"model", "messages", "max_tokens"}, "properties": map[string]any{"model": map[string]any{"type": "string"}, "messages": map[string]any{"type": "array", "minItems": 1, "items": ref("AnthropicMessage")}, "max_tokens": map[string]any{"type": "integer"}, "system": map[string]any{}, "stream": map[string]any{"type": "boolean", "default": false}, "tools": map[string]any{"type": []any{"array", "null"}, "items": map[string]any{"type": "object", "additionalProperties": true}}}, "additionalProperties": true},
				"AnthropicTokenCountRequest": map[string]any{"type": "object", "required": []any{"model", "messages"}, "properties": map[string]any{"model": map[string]any{"type": "string"}, "messages": map[string]any{"type": "array", "minItems": 1, "items": ref("AnthropicMessage")}, "system": map[string]any{}, "tools": map[string]any{"type": []any{"array", "null"}, "items": map[string]any{"type": "object", "additionalProperties": true}}}, "additionalProperties": true},
			},
		},
	}
}

func (s *Server) protectOpenAI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.ProxyAPIKey {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Invalid or missing API Key"})
			return
		}
		next(w, r)
	}
}
func (s *Server) protectAnthropic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == s.cfg.ProxyAPIKey || r.Header.Get("Authorization") == "Bearer "+s.cfg.ProxyAPIKey {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "authentication_error",
				"message": "Invalid or missing API key. Use x-api-key header or Authorization Bearer token.",
			},
		}})
	}
}
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	ids := s.accounts.AvailableModels()
	if len(ids) == 0 {
		if _, err := s.accounts.InitializeFirstWorking(r.Context()); err != nil {
			s.publicError(w, "openai", http.StatusServiceUnavailable, err.Error())
			return
		}
		ids = s.accounts.AvailableModels()
	}
	created := time.Now().Unix()
	data := make([]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "created": created, "owned_by": "anthropic", "description": "Claude model via Kiro API"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.validation(w, "openai", body, err)
		return
	}
	var req convert.ChatCompletionRequest
	if err = json.Unmarshal(body, &req); err != nil || req.Model == "" || len(req.Messages) == 0 {
		if err == nil {
			err = errors.New("model and messages are required")
		}
		s.validation(w, "openai", body, err)
		return
	}
	if req.ReasoningEffort != "" && !contains([]string{"none", "minimal", "low", "medium", "high", "xhigh"}, req.ReasoningEffort) {
		s.validation(w, "openai", body, errors.New("reasoning_effort must be one of none, minimal, low, medium, high, xhigh"))
		return
	}
	if s.cfg.TruncationRecovery {
		injectOpenAIRecovery(&req)
	}
	if s.cfg.WebSearchEnabled && !hasOpenAITool(req.Tools, "web_search") {
		d := "Search the web for current information. Use when you need up-to-date data from the internet."
		req.Tools = append(req.Tools, convert.OpenAITool{Type: "function", Function: &convert.ToolFunction{Name: "web_search", Description: &d, Parameters: webSearchSchema()}})
	}
	prompt := tokenizer.EstimateRequestTokens(req.Messages, req.Tools, nil, false).TotalTokens
	s.execute(w, r, req.Model, req.Stream, "openai", prompt, func(a *accounts.Account) (map[string]any, error) {
		return convert.BuildOpenAIKiroPayloadWithOptions(req, conversationID(), a.Auth.ProfileARN(), convertOptions(s.cfg))
	})
}
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.validation(w, "anthropic", body, err)
		return
	}
	var req convert.AnthropicMessagesRequest
	var rawFields map[string]json.RawMessage
	fieldsErr := json.Unmarshal(body, &rawFields)
	maxTokens, hasMaxTokens := rawFields["max_tokens"]
	if err = json.Unmarshal(body, &req); err != nil || fieldsErr != nil || req.Model == "" || len(req.Messages) == 0 || !hasMaxTokens || bytes.Equal(bytes.TrimSpace(maxTokens), []byte("null")) {
		if err == nil {
			err = errors.New("model, messages, and max_tokens are required")
		}
		s.validation(w, "anthropic", body, err)
		return
	}
	if err = validateAnthropicRequest(req.Messages, req.Tools, req.Temperature, req.TopP, req.TopK); err != nil {
		s.validation(w, "anthropic", body, err)
		return
	}
	if s.cfg.TruncationRecovery {
		injectAnthropicRecovery(&req)
	}
	// Native Anthropic web_search tools are fulfilled directly through Kiro MCP.
	for _, tool := range req.Tools {
		if strings.HasPrefix(tool.Type, "web_search") {
			s.nativeWebSearch(w, r, &req, tool)
			return
		}
	}
	if s.cfg.WebSearchEnabled && !hasAnthropicTool(req.Tools, "web_search") {
		d := "Search the web for current information. Use when you need up-to-date data from the internet."
		req.Tools = append(req.Tools, convert.AnthropicTool{Name: "web_search", Description: &d, InputSchema: webSearchSchema()})
	}
	prompt := tokenizer.EstimateRequestTokens(req.Messages, req.Tools, req.System, false).TotalTokens
	s.execute(w, r, req.Model, req.Stream, "anthropic", prompt, func(a *accounts.Account) (map[string]any, error) {
		return convert.AnthropicToKiroWithOptions(req, conversationID(), a.Auth.ProfileARN(), convertOptions(s.cfg))
	})
}
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		s.validation(w, "anthropic", body, err)
		return
	}
	var req struct {
		Model    string                     `json:"model"`
		Messages []convert.AnthropicMessage `json:"messages"`
		System   any                        `json:"system"`
		Tools    []convert.AnthropicTool    `json:"tools"`
	}
	if err = json.Unmarshal(body, &req); err != nil || req.Model == "" || len(req.Messages) == 0 {
		if err == nil {
			err = errors.New("model and messages are required")
		}
		s.validation(w, "anthropic", body, err)
		return
	}
	if err = validateAnthropicRequest(req.Messages, req.Tools, nil, nil, nil); err != nil {
		s.validation(w, "anthropic", body, err)
		return
	}
	writeJSON(w, 200, map[string]any{"input_tokens": tokenizer.EstimateRequestTokens(req.Messages, req.Tools, req.System).TotalTokens})
}

func (s *Server) execute(w http.ResponseWriter, r *http.Request, requested string, isStream bool, format string, prompt int, build func(*accounts.Account) (map[string]any, error)) {
	excluded := map[string]struct{}{}
	var lastStatus int
	var lastBody []byte
	var lastErr error
	for {
		a, err := s.accounts.GetNextAccount(r.Context(), requested, excluded)
		if err != nil {
			lastErr = err
			break
		}
		if a == nil {
			break
		}
		excluded[a.ID] = struct{}{}
		attemptCtx := upstream.WithAttemptBudget(r.Context(), maxAttempts(s.cfg))
		payload, err := build(a)
		if err != nil {
			s.publicError(w, format, 400, err.Error())
			return
		}
		resolution := a.Resolver.Resolve(requested)
		setPayloadModel(payload, resolution.InternalID)
		dbg := s.debugger()
		dbg.Prepare()
		requestBytes, _ := json.Marshal(payload)
		dbg.KiroRequest(requestBytes)
		res, err := s.doUpstream(attemptCtx, a, payload, isStream)
		if err != nil {
			lastErr = err
			s.accounts.ReportFailure(a.ID, requested, accounts.Recoverable, 502, "")
			continue
		}
		if res.StatusCode == 200 {
			body := io.Reader(res.Body)
			if isStream {
				firstTokenAttempts := s.cfg.FirstTokenMaxRetries
				if firstTokenAttempts < 1 {
					firstTokenAttempts = 1
				}
				var prefix []byte
				var probeErr error
				for attempt := 0; attempt < firstTokenAttempts; attempt++ {
					prefix, probeErr = stream.ProbeFirstEvent(r.Context(), res.Body, s.cfg.FirstTokenTimeout)
					if probeErr == nil || (errors.Is(probeErr, io.EOF) && len(prefix) == 0) {
						probeErr = nil
						break
					}
					_ = res.Body.Close()
					if attempt+1 < firstTokenAttempts {
						if delayErr := waitContext(r.Context(), retryDelay(s.cfg.BaseRetryDelay, attempt)); delayErr != nil {
							probeErr = delayErr
							break
						}
						res, probeErr = s.doUpstream(attemptCtx, a, payload, true)
						if probeErr != nil {
							continue
						}
						if res.StatusCode != http.StatusOK {
							break
						}
					}
				}
				if probeErr != nil || res.StatusCode != http.StatusOK {
					if res != nil {
						_ = res.Body.Close()
					}
					lastErr = probeErr
					s.accounts.ReportFailure(a.ID, requested, accounts.Recoverable, 502, "first_token_timeout")
					continue
				}
				body = io.MultiReader(bytes.NewReader(prefix), res.Body)
			}
			search := s.webSearchFunc(a)
			if isStream {
				var streamErr error
				if format == "openai" {
					streamErr = stream.OpenAI(r.Context(), w, io.TeeReader(body, debugWriter{dbg}), s.cfg.FirstTokenTimeout, stream.OpenAIOptions{Model: requested, PromptTokens: prompt, ModelCache: a.Cache, ReasoningMode: s.cfg.FakeReasoningHandling, InitialBufferSize: s.cfg.FakeReasoningInitialBufferSize, IdleTimeout: s.cfg.StreamingReadTimeout, WebSearch: search})
				} else {
					streamErr = stream.Anthropic(r.Context(), w, io.TeeReader(body, debugWriter{dbg}), s.cfg.FirstTokenTimeout, stream.AnthropicOptions{Model: requested, InputTokens: prompt, ModelCache: a.Cache, ReasoningMode: s.cfg.FakeReasoningHandling, InitialBufferSize: s.cfg.FakeReasoningInitialBufferSize, IdleTimeout: s.cfg.StreamingReadTimeout, WebSearch: search})
				}
				_ = res.Body.Close()
				if streamErr != nil {
					s.accounts.ReportFailure(a.ID, requested, accounts.Recoverable, 502, "stream_error")
					dbg.FlushError(502, streamErr.Error())
				} else {
					s.accounts.ReportSuccess(a.ID, requested)
				}
				return
			}
			result, collectErr := stream.CollectWithOptions(r.Context(), io.TeeReader(body, debugWriter{dbg}), s.cfg.FirstTokenTimeout, stream.ParseOptions{ReasoningMode: s.cfg.FakeReasoningHandling, InitialBufferSize: s.cfg.FakeReasoningInitialBufferSize, IdleTimeout: s.cfg.StreamingReadTimeout})
			res.Body.Close()
			if collectErr != nil {
				s.accounts.ReportFailure(a.ID, requested, accounts.Recoverable, 502, "stream_error")
				dbg.FlushError(502, collectErr.Error())
				s.publicNetworkError(w, format, collectErr)
				return
			}
			result, searches, interceptErr := stream.InterceptWebSearch(r.Context(), result, search)
			if interceptErr != nil {
				s.accounts.ReportFailure(a.ID, requested, accounts.Recoverable, 502, "stream_error")
				dbg.FlushError(502, interceptErr.Error())
				s.publicNetworkError(w, format, interceptErr)
				return
			}
			s.accounts.ReportSuccess(a.ID, requested)
			if format == "openai" {
				writeJSON(w, 200, stream.OpenAIResponse(result, requested, prompt, a.Cache))
			} else {
				writeJSON(w, 200, stream.AnthropicResponseWithSearches(result, searches, requested, prompt, a.Cache))
			}
			return
		}
		lastStatus = res.StatusCode
		lastBody, _ = io.ReadAll(io.LimitReader(res.Body, 2<<20))
		res.Body.Close()
		dbg.Raw(lastBody)
		reason, message := upstreamError(lastBody)
		kind := accounts.ClassifyError(lastStatus, reason)
		s.accounts.ReportFailure(a.ID, requested, kind, lastStatus, reason)
		if kind == accounts.Fatal {
			dbg.FlushError(lastStatus, message)
			if format == "openai" {
				s.openAIUpstreamError(w, lastStatus, message)
			} else {
				s.publicError(w, format, lastStatus, message)
			}
			return
		}
	}
	if lastErr != nil {
		s.publicNetworkError(w, format, lastErr)
		return
	}
	if lastStatus == 0 {
		lastStatus = 503
	}
	_, message := upstreamError(lastBody)
	if message == "" {
		message = "No accounts available"
	}
	if format == "openai" && len(lastBody) > 0 {
		s.openAIUpstreamError(w, lastStatus, message)
		return
	}
	s.publicError(w, format, lastStatus, message)
}

func (s *Server) doUpstream(ctx context.Context, a *accounts.Account, payload any, isStream bool) (*http.Response, error) {
	ctx = upstream.WithAttemptBudget(ctx, maxAttempts(s.cfg))
	host := strings.TrimSpace(a.Auth.APIHost())
	if host == "" {
		host = s.cfg.APIHost(a.Auth.Region())
	}
	s.upstreamMu.Lock()
	client := s.upstreams[a.ID]
	if client == nil {
		var err error
		client, err = upstream.NewForAuth(a.Auth, s.cfg, s.upstreamHTTP)
		if err != nil {
			s.upstreamMu.Unlock()
			return nil, err
		}
		s.upstreams[a.ID] = client
	}
	s.upstreamMu.Unlock()
	return client.Do(ctx, http.MethodPost, strings.TrimRight(host, "/")+"/generateAssistantResponse", payload, isStream)
}

func (s *Server) nativeWebSearch(w http.ResponseWriter, r *http.Request, req *convert.AnthropicMessagesRequest, tool convert.AnthropicTool) {
	query := ""
	if len(req.Messages) > 0 {
		query = extractSearchQuery(convert.ConvertAnthropicContentToText(req.Messages[0].Content))
	}
	if query == "" {
		s.validation(w, "anthropic", nil, errors.New("cannot extract search query from messages"))
		return
	}
	excluded := map[string]struct{}{}
	var result mcp.Response
	var selected *accounts.Account
	var lastErr error
	for {
		a, err := s.accounts.GetNextAccount(r.Context(), req.Model, excluded)
		if err != nil || a == nil {
			if err != nil {
				lastErr = err
			}
			break
		}
		excluded[a.ID] = struct{}{}
		client := &mcp.Client{Auth: a.Auth, HTTP: s.client}
		result, err = client.WebSearch(r.Context(), query)
		if httpErr := new(mcp.HTTPError); errors.As(err, &httpErr) && httpErr.Status == http.StatusForbidden {
			if _, refreshErr := a.Auth.ForceRefresh(r.Context()); refreshErr == nil {
				result, err = client.WebSearch(r.Context(), query)
			}
		}
		if err == nil {
			selected = a
			s.accounts.ReportSuccess(a.ID, req.Model)
			break
		}
		lastErr = err
		status := 502
		if httpErr := new(mcp.HTTPError); errors.As(err, &httpErr) {
			status = httpErr.Status
		}
		s.accounts.ReportFailure(a.ID, req.Model, accounts.Recoverable, status, "mcp_web_search")
		if !s.cfg.AccountSystem {
			break
		}
	}
	if selected == nil {
		if lastErr != nil {
			s.publicError(w, "anthropic", 502, "Web search failed. Please try again.")
		} else {
			s.publicError(w, "anthropic", 503, "No initialized accounts available")
		}
		return
	}
	text := mcp.Summary(query, result.Results)
	inputTokens := tokenizer.CountMessageTokens(req.Messages, false)
	search := stream.SearchResult{Query: query, Response: result, Summary: text}
	if req.Stream {
		if err := stream.AnthropicWebSearch(w, req.Model, inputTokens, search); err != nil {
			s.publicNetworkError(w, "anthropic", err)
		}
		return
	}
	response := map[string]any{"id": "msg_" + conversationID(), "type": "message", "role": "assistant", "model": req.Model, "content": []any{
		map[string]any{"type": "server_tool_use", "id": result.ToolUseID, "name": "web_search", "input": map[string]any{"query": query}},
		map[string]any{"type": "web_search_tool_result", "tool_use_id": result.ToolUseID, "content": mcp.ResultContent(result.Results)},
		map[string]any{"type": "text", "text": text},
	}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": tokenizer.CountTokens(text, false)}}
	writeJSON(w, 200, response)
	_ = tool
}

func (s *Server) debugger() *debuglog.Logger {
	mode := debuglog.Mode(s.cfg.DebugMode)
	if mode != debuglog.Errors && mode != debuglog.All {
		mode = debuglog.Off
	}
	if s.cfg.Debug && mode == debuglog.Off {
		mode = debuglog.All
	}
	dir := s.cfg.DebugDir
	if dir == "" {
		dir = "debug_logs"
	}
	if mode != debuglog.Off {
		dir = filepath.Join(dir, conversationID())
	}
	return &debuglog.Logger{Mode: mode, Dir: dir}
}

type debugWriter struct{ logger *debuglog.Logger }

func (d debugWriter) Write(p []byte) (int, error) { d.logger.Raw(p); return len(p), nil }
func convertOptions(c config.Config) convert.Options {
	return convert.Options{ToolDescriptionMaxLength: c.ToolDescriptionMaxLength, FakeReasoningEnabled: c.FakeReasoningEnabled, FakeReasoningMaxTokens: c.FakeReasoningMaxTokens, FakeReasoningBudgetCap: c.FakeReasoningBudgetCap, TruncationRecovery: c.TruncationRecovery, MaxPayloadBytes: c.MaxPayloadBytes, AutoTrimPayload: c.AutoTrimPayload, HiddenModels: c.HiddenModels}
}
func setPayloadModel(payload map[string]any, id string) {
	state, _ := payload["conversationState"].(map[string]any)
	current, _ := state["currentMessage"].(map[string]any)
	user, _ := current["userInputMessage"].(map[string]any)
	if user != nil && id != "" {
		user["modelId"] = id
	}
}
func upstreamError(body []byte) (string, string) {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		return "", strings.TrimSpace(string(body))
	}
	info := gatewayerr.EnhanceKiroError(v)
	return info.Reason, info.UserMessage
}
func (s *Server) publicNetworkError(w http.ResponseWriter, format string, err error) {
	info := gatewayerr.ClassifyNetworkError(err)
	writeJSON(w, info.SuggestedHTTPCode, gatewayerr.FormatErrorForUser(info, format, true))
}
func (s *Server) openAIUpstreamError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, gatewayerr.OpenAIUpstreamErrorEnvelope{Error: gatewayerr.OpenAIUpstreamError{Message: message, Type: "kiro_api_error", Code: status}})
}
func (s *Server) publicError(w http.ResponseWriter, format string, status int, message string) {
	if status < 400 {
		status = 500
	}
	if format == "anthropic" {
		writeJSON(w, status, gatewayerr.AnthropicErrorEnvelope{Type: "error", Error: gatewayerr.AnthropicError{Type: "api_error", Message: message}})
	} else {
		writeJSON(w, status, gatewayerr.OpenAIErrorEnvelope{Error: gatewayerr.OpenAIError{Message: message, Type: "api_error", Code: "upstream_error"}})
	}
}
func (s *Server) validation(w http.ResponseWriter, format string, body []byte, err error) {
	detail := []map[string]any{{"type": "value_error", "loc": []any{"body"}, "msg": err.Error()}}
	if format == "anthropic" {
		writeJSON(w, 422, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}})
		return
	}
	writeJSON(w, 422, gatewayerr.NewValidationErrorEnvelope(detail, body))
}
func validateAnthropicRequest(messages []convert.AnthropicMessage, tools []convert.AnthropicTool, temperature, topP *float64, topK *int) error {
	for index, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			return fmt.Errorf("messages.%d.role must be user or assistant", index)
		}
	}
	for name, value := range map[string]*float64{"temperature": temperature, "top_p": topP} {
		if value != nil && (*value < 0 || *value > 1) {
			return fmt.Errorf("%s must be between 0 and 1", name)
		}
	}
	if topK != nil && *topK < 0 {
		return errors.New("top_k must be greater than or equal to 0")
	}
	for index, tool := range tools {
		if tool.Type == "" && tool.InputSchema == nil {
			return fmt.Errorf("tools.%d.input_schema is required for user-defined tools", index)
		}
	}
	return nil
}

func readBody(r *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(nil, r.Body, requestBodyLimit)
	b, err := io.ReadAll(limited)
	if err != nil {
		return b, err
	}
	if len(b) == 0 {
		return b, errors.New("empty request body")
	}
	return b, nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func conversationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
func injectOpenAIRecovery(req *convert.ChatCompletionRequest) {
	out := make([]convert.ChatMessage, 0, len(req.Messages)+1)
	for _, msg := range req.Messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if info, ok := recovery.GetToolTruncation(msg.ToolCallID); ok {
				notice := recovery.GenerateTruncationToolResult(info.ToolName, msg.ToolCallID, info.TruncationInfo)
				msg.Content = fmt.Sprintf("%s\n\n---\n\nOriginal tool result:\n%s", notice["content"], contentText(msg.Content))
			}
		}
		out = append(out, msg)
		if msg.Role == "assistant" {
			text := contentText(msg.Content)
			if text != "" {
				if _, ok := recovery.GetContentTruncation(text); ok {
					out = append(out, convert.ChatMessage{Role: "user", Content: recovery.GenerateTruncationUserMessage()})
				}
			}
		}
	}
	req.Messages = out
}

func injectAnthropicRecovery(req *convert.AnthropicMessagesRequest) {
	out := make([]convert.AnthropicMessage, 0, len(req.Messages)+1)
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			if blocks, ok := msg.Content.([]any); ok {
				modified := append([]any(nil), blocks...)
				for i, raw := range modified {
					block, ok := raw.(map[string]any)
					if !ok || block["type"] != "tool_result" {
						continue
					}
					id, _ := block["tool_use_id"].(string)
					if info, found := recovery.GetToolTruncation(id); found {
						notice := recovery.GenerateTruncationToolResult(info.ToolName, id, info.TruncationInfo)
						copyBlock := make(map[string]any, len(block))
						for key, value := range block {
							copyBlock[key] = value
						}
						copyBlock["content"] = fmt.Sprintf("%s\n\n---\n\nOriginal tool result:\n%s", notice["content"], contentText(block["content"]))
						modified[i] = copyBlock
					}
				}
				msg.Content = modified
			}
		}
		out = append(out, msg)
		if msg.Role == "assistant" {
			text := convert.ConvertAnthropicContentToText(msg.Content)
			if text != "" {
				if _, ok := recovery.GetContentTruncation(text); ok {
					out = append(out, convert.AnthropicMessage{Role: "user", Content: []any{map[string]any{"type": "text", "text": recovery.GenerateTruncationUserMessage()}}})
				}
			}
		}
	}
	req.Messages = out
}

func contentText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	return convert.ExtractTextContent(content)
}

func maxAttempts(cfg config.Config) int {
	total := cfg.MaxRetries
	if cfg.FirstTokenMaxRetries > total {
		total = cfg.FirstTokenMaxRetries
	}
	if total < 1 {
		return 1
	}
	return total
}

func retryDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	return base * time.Duration(1<<attempt)
}
func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func appendUnique(xs []string, v string) []string {
	if !contains(xs, v) {
		return append(xs, v)
	}
	return xs
}
func hasAnthropicTool(xs []convert.AnthropicTool, name string) bool {
	for _, x := range xs {
		if x.Name == name {
			return true
		}
	}
	return false
}

func hasOpenAITool(xs []convert.OpenAITool, name string) bool {
	for _, x := range xs {
		if x.Function != nil && x.Function.Name == name {
			return true
		}
	}
	return false
}

func webSearchSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string", "description": "Search query"}}, "required": []any{"query"}}
}

func extractSearchQuery(text string) string {
	text = strings.TrimSpace(text)
	return strings.TrimSpace(strings.TrimPrefix(text, "Perform a web search for the query: "))
}

func (s *Server) webSearchFunc(a *accounts.Account) stream.WebSearchFunc {
	if a == nil || a.Auth == nil {
		return nil
	}
	client := &mcp.Client{Auth: a.Auth, HTTP: s.client}
	return client.WebSearch
}
