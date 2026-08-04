package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testAuth struct{ host string }

func (a testAuth) AccessToken(context.Context) (string, error)  { return "token", nil }
func (a testAuth) ForceRefresh(context.Context) (string, error) { return "token", nil }
func (a testAuth) QHost() string                                { return a.host }

func TestWebSearchRequestAndNestedResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("x-amzn-codewhisperer-optout") != "false" {
			t.Fatalf("request=%s headers=%v", r.URL.Path, r.Header)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		params := request["params"].(map[string]any)
		arguments := params["arguments"].(map[string]any)
		if request["jsonrpc"] != "2.0" || request["method"] != "tools/call" || params["name"] != "web_search" || arguments["query"] != "Go" || !strings.HasPrefix(request["id"].(string), "web_search_tooluse_") {
			t.Fatalf("request=%v", request)
		}
		text, _ := json.Marshal(map[string]any{"results": []any{map[string]any{"title": "Go"}}, "totalResults": 1})
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "isError": false}})
	}))
	defer server.Close()
	response, err := (&Client{Auth: testAuth{host: server.URL}, HTTP: server.Client()}).WebSearch(context.Background(), "Go")
	if err != nil || !strings.HasPrefix(response.ToolUseID, "srvtoolu_") || response.Results["totalResults"] != float64(1) {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestWebSearchRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": -32600, "message": "bad request"}})
	}))
	defer server.Close()
	_, err := (&Client{Auth: testAuth{host: server.URL}, HTTP: server.Client()}).WebSearch(context.Background(), "Go")
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("err=%v", err)
	}
}

func TestSummary(t *testing.T) {
	summary := Summary("go", map[string]any{"results": []any{map[string]any{"title": "Go", "url": "https://go.dev", "snippet": "The Go language"}}})
	for _, want := range []string{"<web_search>", `Search results for "go"`, "Go", "https://go.dev", "The Go language", "</web_search>"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("missing %q in %q", want, summary)
		}
	}
}
