package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/accounts"
	"github.com/jwadow/kiro-gateway-go/internal/auth"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	"github.com/jwadow/kiro-gateway-go/internal/model"
	"github.com/jwadow/kiro-gateway-go/internal/recovery"
)

type fakeAuth struct{ host string }

func (a *fakeAuth) AccessToken(context.Context) (string, error)  { return "token", nil }
func (a *fakeAuth) ForceRefresh(context.Context) (string, error) { return "token", nil }
func (a *fakeAuth) ProfileARN() string                           { return "arn:test" }
func (a *fakeAuth) Region() string                               { return "us-east-1" }
func (a *fakeAuth) APIHost() string                              { return a.host }
func (a *fakeAuth) QHost() string                                { return a.host }
func (a *fakeAuth) Fingerprint() string                          { return "test" }
func (a *fakeAuth) Type() auth.Type                              { return auth.Desktop }

type fakeInitializer struct{ auth *fakeAuth }

func (i fakeInitializer) Initialize(context.Context, accounts.Credential) (accounts.Initialized, error) {
	cache := model.NewCache(time.Hour, 200000)
	cache.Update([]model.Info{{ModelID: "claude-sonnet-4.5"}, {ModelID: "auto"}})
	return accounts.Initialized{Auth: i.auth, Cache: cache, Resolver: model.NewResolver(cache, nil, map[string]string{"auto-kiro": "auto"}, []string{"auto"})}, nil
}
func (i fakeInitializer) RefreshModels(context.Context, accounts.Credential, accounts.Initialized) ([]model.Info, error) {
	return nil, nil
}

func newIntegrationServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(creds, []byte(`[{"type":"refresh_token","refresh_token":"x"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := accounts.NewWithOptions(accounts.Options{CredentialsFile: creds, StateFile: filepath.Join(dir, "state.json"), Initializer: fakeInitializer{&fakeAuth{host: upstreamURL}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ProxyAPIKey: "test-key", APIRegion: "us-east-1", Region: "us-east-1", MaxRetries: 1, FirstTokenTimeout: time.Second, StreamingReadTimeout: time.Second, FakeReasoningEnabled: false, ToolDescriptionMaxLength: 10000, MaxPayloadBytes: 600000, FallbackModels: []config.Model{{ModelID: "claude-sonnet-4.5"}}}
	s, err := New(cfg, Options{Accounts: manager})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestModelsLazilyInitializeResolverAndApplyFiltering(t *testing.T) {
	s := newIntegrationServer(t, "http://unused.invalid")
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"id":"claude-sonnet-4.5"`) || !strings.Contains(res.Body.String(), `"id":"auto-kiro"`) {
		t.Fatalf("resolver models missing: %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), `"id":"auto"`) {
		t.Fatalf("hidden model leaked: %s", res.Body.String())
	}
}

func TestOpenAICompletionUsesUpstream(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"content":"Hello from Kiro"}`))
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if gotPath != "/generateAssistantResponse" {
		t.Fatalf("path=%q", gotPath)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	choices := out["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "Hello from Kiro" {
		t.Fatalf("response=%v", out)
	}
}

func TestServerReusesUpstreamTransportConnections(t *testing.T) {
	var newConnections int
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":"ok"}{"usage":1}`))
	}))
	up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections++
		}
	}
	up.Start()
	defer up.Close()

	s := newIntegrationServer(t, up.URL)
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}`))
		req.Header.Set("Authorization", "Bearer test-key")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
	}
	if newConnections != 1 {
		t.Fatalf("upstream connections=%d, want 1", newConnections)
	}
	s.CloseIdleConnections()
}

func TestAnthropicStreamingAndAPIKeyHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"content":"Hi"}`))
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`))
	req.Header.Set("x-api-key", "test-key")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "message_start") || !strings.Contains(res.Body.String(), "Hi") {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestStreamingErrorAfterFirstChunkUsesProtocolPayload(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 1000\r\n\r\n{\"content\":\"first\"}")
		_ = rw.Flush()
	}))
	defer up.Close()

	for _, tc := range []struct {
		path   string
		body   string
		header string
		want   []string
	}{
		{path: "/v1/chat/completions", body: `{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"Hi"}]}`, header: "Authorization", want: []string{`"content":"first"`, `"error":`, "data: [DONE]"}},
		{path: "/v1/messages", body: `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, header: "x-api-key", want: []string{`"text":"first"`, "event: error", `"type":"api_error"`}},
	} {
		s := newIntegrationServer(t, up.URL)
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		if tc.header == "Authorization" {
			req.Header.Set(tc.header, "Bearer test-key")
		} else {
			req.Header.Set(tc.header, "test-key")
		}
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		for _, want := range tc.want {
			if !strings.Contains(res.Body.String(), want) {
				t.Fatalf("path=%s missing %s in %s", tc.path, want, res.Body.String())
			}
		}
		stats := s.accounts.Accounts()[0].Stats
		if stats.TotalRequests != 1 || stats.SuccessfulRequests != 0 || stats.FailedRequests != 1 {
			t.Fatalf("path=%s stats=%+v", tc.path, stats)
		}
	}
}

func TestRouteInjectsTruncationRecovery(t *testing.T) {
	var payload map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"content":"done"}{"usage":1}`))
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	s.cfg.TruncationRecovery = true
	recovery.SaveToolTruncation("call_truncated", "write", map[string]any{"reason": "missing brace"})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"assistant","tool_calls":[{"id":"call_truncated","type":"function","function":{"name":"write","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_truncated","content":"failed"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	encoded, _ := json.Marshal(payload)
	if !strings.Contains(string(encoded), "API Limitation") || !strings.Contains(string(encoded), "Original tool result") {
		t.Fatalf("recovery notice missing from payload: %s", encoded)
	}
	if _, ok := recovery.GetToolTruncation("call_truncated"); ok {
		t.Fatal("recovery entry was not consumed")
	}
}

func TestNonStreamingRequestIsNotReplayed(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"content":"once"}`))
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	s.cfg.FirstTokenMaxRetries = 3
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || calls != 1 || !strings.Contains(res.Body.String(), `"content":"once"`) {
		t.Fatalf("status=%d calls=%d body=%s", res.Code, calls, res.Body.String())
	}
}

func TestEmptySuccessfulUpstreamIsNormalResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	for _, tc := range []struct{ path, auth, body, want string }{
		{"/v1/chat/completions", "Authorization", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}`, `"content":""`},
		{"/v1/messages", "x-api-key", `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Hi"}]}`, `"text":""`},
		{"/v1/messages", "x-api-key", `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, "message_stop"},
	} {
		s := newIntegrationServer(t, up.URL)
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		if tc.auth == "Authorization" {
			req.Header.Set(tc.auth, "Bearer test-key")
		} else {
			req.Header.Set(tc.auth, "test-key")
		}
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), tc.want) {
			t.Fatalf("path=%s status=%d body=%s", tc.path, res.Code, res.Body.String())
		}
		stats := s.accounts.Accounts()[0].Stats
		if stats.SuccessfulRequests != 1 || stats.FailedRequests != 0 {
			t.Fatalf("path=%s stats=%+v", tc.path, stats)
		}
	}
}

func TestAnthropicStrictValidation(t *testing.T) {
	s := newIntegrationServer(t, "http://unused.invalid")
	for _, body := range []string{
		`{"model":"x","max_tokens":1,"messages":[{"role":"system","content":"bad"}]}`,
		`{"model":"x","max_tokens":1,"temperature":-0.01,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"x","max_tokens":1,"top_p":1.01,"messages":[{"role":"assistant","content":"hi"}]}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "test-key")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Body.String(), "invalid_request_error") {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
	}
	countReq := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"x","messages":[{"role":"tool","content":"bad"}]}`))
	countReq.Header.Set("x-api-key", "test-key")
	countRes := httptest.NewRecorder()
	s.Handler().ServeHTTP(countRes, countReq)
	if countRes.Code != http.StatusUnprocessableEntity {
		t.Fatalf("count_tokens role status=%d body=%s", countRes.Code, countRes.Body.String())
	}
	for _, value := range []string{"0", "1"} {
		body := fmt.Sprintf(`{"model":"x","max_tokens":1,"temperature":%s,"top_p":%s,"messages":[{"role":"user","content":"hi"}]}`, value, value)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "test-key")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code == http.StatusUnprocessableEntity {
			t.Fatalf("boundary %s rejected: %s", value, res.Body.String())
		}
	}
}

func TestFirstTokenTimeoutRetriesSameAccount(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
			return
		}
		_, _ = w.Write([]byte(`{"content":"retried"}{"usage":1}`))
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	s.cfg.FirstTokenTimeout = 5 * time.Millisecond
	s.cfg.FirstTokenMaxRetries = 2
	s.cfg.BaseRetryDelay = time.Millisecond
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"Hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if calls != 2 || !strings.Contains(res.Body.String(), "retried") {
		t.Fatalf("calls=%d body=%s", calls, res.Body.String())
	}
}

func TestNativeAnthropicWebSearchJSONAndStreaming(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		params := payload["params"].(map[string]any)
		if params["name"] != "web_search" || r.Header.Get("x-amzn-codewhisperer-optout") != "false" {
			t.Fatalf("payload=%v headers=%v", payload, r.Header)
		}
		results, _ := json.Marshal(map[string]any{"results": []any{map[string]any{"title": "Go", "url": "https://go.dev", "snippet": "Go news"}}, "totalResults": 1})
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(results)}}}})
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	for _, streaming := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"Perform a web search for the query: Go"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}]}`, streaming)
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "test-key")
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		response := res.Body.String()
		if res.Code != http.StatusOK || !strings.Contains(response, "server_tool_use") || !strings.Contains(response, "web_search_tool_result") || !strings.Contains(response, "web_search_result") || !strings.Contains(response, `Search results for \"Go\"`) || strings.Contains(response, "generateAssistantResponse") {
			t.Fatalf("stream=%v status=%d body=%s", streaming, res.Code, response)
		}
		if streaming && (!strings.Contains(response, "input_json_delta") || !strings.Contains(response, "message_stop")) {
			t.Fatalf("incomplete SSE: %s", response)
		}
	}
}

func TestOpenAIPathBInjectsAndInterceptsWebSearch(t *testing.T) {
	var generatedPayload map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generateAssistantResponse":
			_ = json.NewDecoder(r.Body).Decode(&generatedPayload)
			_, _ = w.Write([]byte(`{"name":"web_search","toolUseId":"call_search"}{"input":"{\"query\":\"Go\"}"}{"stop":true}{"usage":1}`))
		case "/mcp":
			results, _ := json.Marshal(map[string]any{"results": []any{map[string]any{"title": "Go", "url": "https://go.dev", "snippet": "Go news"}}})
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []any{map[string]any{"text": string(results)}}}})
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	s.cfg.WebSearchEnabled = true
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"latest Go"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `Search results for \"Go\"`) || strings.Contains(res.Body.String(), `"name":"web_search"`) {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	encoded, _ := json.Marshal(generatedPayload)
	if !strings.Contains(string(encoded), "web_search") {
		t.Fatalf("web_search not injected: %s", encoded)
	}
}

func TestHTTPAPICompatibilityContracts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"upstream rejected request","reason":"BAD_REQUEST"}`))
	}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	handler := s.Handler()

	t.Run("OpenAI upstream error shape", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}`))
		req.Header.Set("Authorization", "Bearer test-key")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		errorBody := body["error"].(map[string]any)
		if len(errorBody) != 3 || errorBody["type"] != "kiro_api_error" || errorBody["code"] != float64(http.StatusBadRequest) || errorBody["message"] != "upstream rejected request (reason: BAD_REQUEST)" {
			t.Fatalf("error=%#v", errorBody)
		}
	})

	t.Run("Anthropic x-api-key wins over bad authorization", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("x-api-key", "test-key")
		req.Header.Set("Authorization", "Bearer wrong")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
	})

	t.Run("Anthropic authentication error detail", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		var body struct {
			Detail struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"detail"`
		}
		if res.Code != http.StatusUnauthorized || json.Unmarshal(res.Body.Bytes(), &body) != nil || body.Detail.Type != "error" || body.Detail.Error.Type != "authentication_error" || body.Detail.Error.Message != "Invalid or missing API key. Use x-api-key header or Authorization Bearer token." {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
	})

	t.Run("OpenAI retains Authorization semantics", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("x-api-key", "test-key")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized || res.Body.String() != "{\"detail\":\"Invalid or missing API Key\"}\n" {
			t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
		}
	})

	t.Run("schema compatible relaxed values", func(t *testing.T) {
		openAI := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x","n":2,"messages":[{"role":"user","content":"hello"}]}`))
		openAI.Header.Set("Authorization", "Bearer test-key")
		openAIRes := httptest.NewRecorder()
		handler.ServeHTTP(openAIRes, openAI)
		if openAIRes.Code == http.StatusUnprocessableEntity {
			t.Fatalf("n was rejected: %s", openAIRes.Body.String())
		}

		anthropic := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x","max_tokens":0,"messages":[{"role":"user","content":"hello"}]}`))
		anthropic.Header.Set("x-api-key", "test-key")
		anthropicRes := httptest.NewRecorder()
		handler.ServeHTTP(anthropicRes, anthropic)
		if anthropicRes.Code == http.StatusUnprocessableEntity {
			t.Fatalf("zero max_tokens was rejected: %s", anthropicRes.Body.String())
		}

		missingMax := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hello"}]}`))
		missingMax.Header.Set("x-api-key", "test-key")
		missingMaxRes := httptest.NewRecorder()
		handler.ServeHTTP(missingMaxRes, missingMax)
		if missingMaxRes.Code != http.StatusUnprocessableEntity {
			t.Fatalf("missing max_tokens status=%d body=%s", missingMaxRes.Code, missingMaxRes.Body.String())
		}
	})
}

func TestHTTPMetadataRoutesAndJSONFallbacks(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	s := newIntegrationServer(t, up.URL)
	handler := s.Handler()

	for _, tc := range []struct {
		path        string
		contentType string
		contains    []string
	}{
		{path: "/openapi.json", contentType: "application/json", contains: []string{`"openapi":"3.1.0"`, `"/v1/chat/completions"`, `"/v1/messages/count_tokens"`, `"AnthropicAPIKey"`}},
		{path: "/docs", contentType: "text/html; charset=utf-8", contains: []string{"SwaggerUIBundle", "/openapi.json"}},
		{path: "/redoc", contentType: "text/html; charset=utf-8", contains: []string{"<redoc", "/openapi.json"}},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK || res.Header().Get("Content-Type") != tc.contentType {
			t.Fatalf("path=%s status=%d content-type=%q", tc.path, res.Code, res.Header().Get("Content-Type"))
		}
		for _, want := range tc.contains {
			if !strings.Contains(res.Body.String(), want) {
				t.Fatalf("path=%s missing %q in %s", tc.path, want, res.Body.String())
			}
		}
	}

	for _, tc := range []struct {
		method string
		path   string
		status int
		body   string
	}{
		{method: http.MethodGet, path: "/missing", status: http.StatusNotFound, body: `{"detail":"Not Found"}` + "\n"},
		{method: http.MethodGet, path: "/v1/messages", status: http.StatusMethodNotAllowed, body: `{"detail":"Method Not Allowed"}` + "\n"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != tc.status || res.Header().Get("Content-Type") != "application/json" || res.Body.String() != tc.body {
			t.Fatalf("%s %s: status=%d content-type=%q body=%q", tc.method, tc.path, res.Code, res.Header().Get("Content-Type"), res.Body.String())
		}
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/v1/messages", nil)
	preflightRes := httptest.NewRecorder()
	handler.ServeHTTP(preflightRes, preflight)
	if preflightRes.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("allow-credentials=%q", preflightRes.Header().Get("Access-Control-Allow-Credentials"))
	}
}
