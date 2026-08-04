package upstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/auth"
	"github.com/jwadow/kiro-gateway-go/internal/config"
)

type stubRuntimeAuth struct {
	token      string
	refreshes  int
	profileARN string
	qHost      string
	authType   auth.Type
}

func (a *stubRuntimeAuth) AccessToken(context.Context) (string, error) { return a.token, nil }
func (a *stubRuntimeAuth) ForceRefresh(context.Context) (string, error) {
	a.refreshes++
	a.token = "refreshed"
	return a.token, nil
}
func (a *stubRuntimeAuth) Fingerprint() string { return "fingerprint" }
func (a *stubRuntimeAuth) ProfileARN() string  { return a.profileARN }
func (a *stubRuntimeAuth) QHost() string       { return a.qHost }
func (a *stubRuntimeAuth) Type() auth.Type     { return a.authType }

func TestListAvailableModelsParametersRetriesAndDecode(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/ListAvailableModels" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("origin") != "AI_EDITOR" || r.URL.Query().Get("profileArn") != "arn:test" {
			t.Fatalf("query=%v", r.URL.Query())
		}
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"modelId": "dynamic-model", "tokenLimits": map[string]any{"maxInputTokens": 12345}}}})
	}))
	defer server.Close()
	authn := &stubRuntimeAuth{token: "token", profileARN: "arn:test", qHost: server.URL, authType: auth.Desktop}
	models, err := ListAvailableModels(context.Background(), authn, config.Config{MaxRetries: 2, BaseRetryDelay: time.Millisecond}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(models) != 1 || models[0].ModelID != "dynamic-model" {
		t.Fatalf("calls=%d models=%+v", calls, models)
	}
}

func TestRuntimeHeadersRefreshAndRateLimitRetry(t *testing.T) {
	authn := &stubRuntimeAuth{token: "initial"}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("x-amz-target"); got != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
			t.Fatalf("target header = %q", got)
		}
		switch calls {
		case 1:
			if r.Header.Get("Authorization") != "Bearer initial" {
				t.Fatalf("first auth = %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusForbidden)
		case 2:
			if r.Header.Get("Authorization") != "Bearer refreshed" {
				t.Fatalf("refreshed auth = %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{ "ok": true }`)
		}
	}))
	defer server.Close()
	client, err := NewForAuth(authn, config.Config{MaxRetries: 4, BaseRetryDelay: time.Millisecond}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(context.Background(), http.MethodPost, server.URL, map[string]any{"x": 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || calls != 3 || authn.refreshes != 1 {
		t.Fatalf("status=%d calls=%d refreshes=%d", res.StatusCode, calls, authn.refreshes)
	}
}

func TestStreamTransportRetriesUseMaxRetriesAndSharedBudget(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := NewForAuth(&stubRuntimeAuth{token: "token"}, config.Config{MaxRetries: 2, FirstTokenMaxRetries: 9, BaseRetryDelay: time.Nanosecond}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithAttemptBudget(context.Background(), 2)
	res, err := client.Do(ctx, http.MethodPost, server.URL, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
	if _, err = client.Do(ctx, http.MethodPost, server.URL, nil, true); err == nil {
		t.Fatal("expected exhausted shared budget")
	}
	if calls != 2 {
		t.Fatalf("budget allowed extra call: %d", calls)
	}
}

func TestRetryAfterSecondsAndDate(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	if got := retryAfter("2", now, time.Second); got != 2*time.Second {
		t.Fatalf("seconds=%v", got)
	}
	if got := retryAfter(now.Add(3*time.Second).Format(http.TimeFormat), now, time.Second); got != 3*time.Second {
		t.Fatalf("date=%v", got)
	}
	if got := retryAfter("999", now, time.Second); got != 30*time.Second {
		t.Fatalf("cap=%v", got)
	}
}

func TestNewForAuthSelectsConfiguredProxyTransport(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	client, err := NewForAuth(&stubRuntimeAuth{}, config.Config{VPNProxyURL: "http://proxy.example:8080"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("proxy transport not configured: %#v", client.client.Transport)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://remote.example", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil || proxyURL == nil || proxyURL.Host != "proxy.example:8080" {
		t.Fatalf("proxy=%v err=%v", proxyURL, err)
	}
}

func TestRetryServerError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()
	manager, _ := auth.New(auth.Options{CredentialsFile: credentialFile(t), Region: "us-east-1"})
	client, err := New(manager, config.Config{MaxRetries: 2, BaseRetryDelay: time.Millisecond, StreamingReadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(context.Background(), http.MethodPost, server.URL, map[string]any{"x": 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if calls != 2 || res.StatusCode != http.StatusOK {
		t.Fatalf("calls=%d status=%d", calls, res.StatusCode)
	}
}

func TestParseProxyURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "default HTTP", raw: "proxy.example:8080", want: "http://proxy.example:8080"},
		{name: "HTTPS", raw: "HTTPS://proxy.example:8443", want: "https://proxy.example:8443"},
		{name: "SOCKS5 auth", raw: "socks5://user:p%40ss@proxy.example:1080", want: "socks5://user:p%40ss@proxy.example:1080"},
		{name: "SOCKS5H", raw: "socks5h://proxy.example:1080", want: "socks5h://proxy.example:1080"},
		{name: "missing host", raw: "http://", wantErr: true},
		{name: "path", raw: "http://proxy.example:8080/path", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProxyURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseProxyURL(%q) unexpectedly succeeded", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigureProxyRejectsUnsupportedScheme(t *testing.T) {
	err := configureProxy(http.DefaultTransport.(*http.Transport).Clone(), "ftp://proxy.example:21")
	if err == nil || !strings.Contains(err.Error(), "unsupported VPN proxy scheme") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAppendNoProxyPreservesAndDeduplicates(t *testing.T) {
	got := appendNoProxy("internal.corp, localhost,*.example.com", "127.0.0.1", "localhost", "::1")
	want := "internal.corp,localhost,*.example.com,127.0.0.1,::1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHTTPProxyAndAuthentication(t *testing.T) {
	t.Setenv("NO_PROXY", "internal.example")
	var mu sync.Mutex
	var requests int
	var gotAuth string
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		gotAuth = r.Header.Get("Proxy-Authorization")
		mu.Unlock()
		if r.URL.Host != "upstream.invalid" {
			t.Errorf("proxy received host %q", r.URL.Host)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxyServer.Close()

	proxyURL, _ := url.Parse(proxyServer.URL)
	proxyURL.User = url.UserPassword("user", "p@ss")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := configureProxy(transport, proxyURL.String()); err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{Transport: transport}).Get("http://upstream.invalid/resource")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("proxy requests = %d, want 1", requests)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:p@ss"))
	if gotAuth != wantAuth {
		t.Fatalf("Proxy-Authorization = %q, want %q", gotAuth, wantAuth)
	}
}

func TestHTTPProxyBypassesLocalhostAndPreservesNoProxy(t *testing.T) {
	t.Setenv("NO_PROXY", "example.test")
	var proxyRequests int
	proxyServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		proxyRequests++
	}))
	defer proxyServer.Close()
	proxyURL, _ := url.Parse(proxyServer.URL)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := configureProxy(transport, proxyURL.String()); err != nil {
		t.Fatal(err)
	}
	proxyFor := transport.Proxy
	for _, rawURL := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080", "http://example.test"} {
		u, _ := url.Parse(rawURL)
		got, err := proxyFor(&http.Request{URL: u})
		if err != nil || got != nil {
			t.Errorf("%s proxy = %v, err = %v; want direct", rawURL, got, err)
		}
	}
	u, _ := url.Parse("https://remote.example")
	got, err := proxyFor(&http.Request{URL: u})
	if err != nil || got == nil || got.String() != proxyURL.String() {
		t.Fatalf("remote proxy = %v, err = %v", got, err)
	}
	if proxyRequests != 0 {
		t.Fatalf("proxy unexpectedly received %d requests", proxyRequests)
	}
}

func TestHTTPSProxyConnectAndAuthentication(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "through-connect")
	}))
	defer target.Close()
	targetURL, _ := url.Parse(target.URL)
	proxyServer, connectAuth := newConnectProxy(t, targetURL.Host)
	requestURL := "https://target.invalid:" + targetURL.Port()
	defer proxyServer.Close()

	proxyURL, _ := url.Parse(proxyServer.URL)
	proxyURL.User = url.UserPassword("alice", "secret")
	transport := target.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	if err := configureProxy(transport, proxyURL.String()); err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{Transport: transport}).Get(requestURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "through-connect" {
		t.Fatalf("body = %q", body)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if got := <-connectAuth; got != wantAuth {
		t.Fatalf("CONNECT auth = %q, want %q", got, wantAuth)
	}
}

func TestSOCKS5ProxyAuthenticationAndDNSModes(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	for _, tt := range []struct {
		scheme   string
		wantType byte
	}{
		{scheme: "socks5", wantType: 1},
		{scheme: "socks5h", wantType: 3},
	} {
		t.Run(tt.scheme, func(t *testing.T) {
			server := newSOCKSServer(t, "user", "p@ss")
			defer server.Close()
			proxyURL := fmt.Sprintf("%s://user:p%%40ss@%s", tt.scheme, server.Addr())
			transport := http.DefaultTransport.(*http.Transport).Clone()
			if err := configureProxy(transport, proxyURL); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: transport}
			res, err := client.Get("http://example.com:80/resource")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			request := <-server.requests
			if request.addressType != tt.wantType {
				t.Fatalf("address type = %d, want %d", request.addressType, tt.wantType)
			}
			if tt.scheme == "socks5h" && request.host != "example.com" {
				t.Fatalf("SOCKS5H host = %q", request.host)
			}
			if request.username != "user" || request.password != "p@ss" {
				t.Fatalf("auth = %q:%q", request.username, request.password)
			}
		})
	}
}

func TestSOCKSProxyBypassesLocalhost(t *testing.T) {
	t.Setenv("NO_PROXY", "")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddress := listener.Addr().String()
	listener.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := configureProxy(transport, "socks5://"+proxyAddress); err != nil {
		t.Fatal(err)
	}
	res, err := (&http.Client{Transport: transport}).Get(target.URL)
	if err != nil {
		t.Fatalf("localhost request used unavailable SOCKS proxy: %v", err)
	}
	res.Body.Close()
}

func newConnectProxy(t *testing.T, targetAddress string) (*httptest.Server, <-chan string) {
	t.Helper()
	authHeader := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		authHeader <- r.Header.Get("Proxy-Authorization")
		upstream, err := net.Dial("tcp", targetAddress)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			t.Error("response writer cannot hijack")
			return
		}
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			t.Error(err)
			return
		}
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		go tunnel(client, upstream)
		go tunnel(upstream, client)
	}))
	return server, authHeader
}

func tunnel(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

type socksRequest struct {
	addressType              byte
	host, username, password string
}

type socksServer struct {
	net.Listener
	requests chan socksRequest
}

func newSOCKSServer(t *testing.T, username, password string) *socksServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &socksServer{Listener: listener, requests: make(chan socksRequest, 1)}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		header := readBytes(t, reader, 2)
		methods := readBytes(t, reader, int(header[1]))
		method := byte(0)
		if username != "" {
			method = 2
		}
		if !strings.Contains(string(methods), string([]byte{method})) {
			t.Errorf("SOCKS method %d not offered", method)
			return
		}
		_, _ = conn.Write([]byte{5, method})
		request := socksRequest{}
		if method == 2 {
			auth := readBytes(t, reader, 2)
			request.username = string(readBytes(t, reader, int(auth[1])))
			passLen := readBytes(t, reader, 1)[0]
			request.password = string(readBytes(t, reader, int(passLen)))
			status := byte(0)
			if request.username != username || request.password != password {
				status = 1
			}
			_, _ = conn.Write([]byte{1, status})
			if status != 0 {
				return
			}
		}
		connect := readBytes(t, reader, 4)
		request.addressType = connect[3]
		switch request.addressType {
		case 1:
			request.host = net.IP(readBytes(t, reader, 4)).String()
		case 3:
			request.host = string(readBytes(t, reader, int(readBytes(t, reader, 1)[0])))
		case 4:
			request.host = net.IP(readBytes(t, reader, 16)).String()
		}
		_ = readBytes(t, reader, 2)
		server.requests <- request
		_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		_, _ = io.WriteString(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
	}()
	return server
}

func readBytes(t *testing.T, reader io.Reader, size int) []byte {
	t.Helper()
	buffer := make([]byte, size)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Error(err)
		return make([]byte, size)
	}
	return buffer
}

func credentialFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/creds.json"
	data := []byte(`{"accessToken":"valid","refreshToken":"refresh","expiresAt":"2099-01-01T00:00:00Z"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
