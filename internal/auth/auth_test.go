package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	data := `{"accessToken":"access","refreshToken":"refresh","profileArn":"arn:test","region":"eu-west-1","expiresAt":"2099-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{CredentialsFile: path, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := m.AccessToken(context.Background())
	if err != nil || token != "access" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if m.Region() != "eu-west-1" || m.ProfileARN() != "arn:test" {
		t.Fatalf("region=%s arn=%s", m.Region(), m.ProfileARN())
	}
}
func TestGlobalAPIRegionOverridesCredentialRegion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	data := `{"accessToken":"access","refreshToken":"refresh","region":"eu-west-1","expiresAt":"2099-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{CredentialsFile: path, Region: "us-east-1", APIRegion: "ap-southeast-2"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Region() != "ap-southeast-2" || !strings.Contains(m.APIHost(), ".ap-southeast-2.") {
		t.Fatalf("region=%s host=%s", m.Region(), m.APIHost())
	}
}

func TestDesktopRefresh(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/refreshToken") {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "new-access", "refreshToken": "new-refresh", "expiresIn": 3600})
	}))
	defer server.Close()
	transport := rewriteTransport{base: http.DefaultTransport, target: server.URL}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	m, err := New(Options{RefreshToken: "old", Region: "us-east-1", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	token, err := m.AccessToken(context.Background())
	if err != nil || token != "new-access" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if body["refreshToken"] != "old" {
		t.Fatalf("body=%v", body)
	}
}

type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target, _ := http.NewRequest(req.Method, r.target+req.URL.Path, req.Body)
	clone.URL = target.URL
	return r.base.RoundTrip(clone)
}
func TestRefreshUsesTokenWhenPersistenceFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "usable", "expiresIn": 3600})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "missing", "creds.json")
	m, err := New(Options{RefreshToken: "refresh", Region: "us-east-1", CredentialsFile: path, HTTPClient: &http.Client{Transport: rewriteTransport{base: http.DefaultTransport, target: server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := m.AccessToken(context.Background())
	if err != nil || token != "usable" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestJSONReadOnlySkipsWriteBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "usable", "expiresIn": 3600})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(path, []byte(`{"refreshToken":"refresh"}`), 0400); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{CredentialsFile: path, Region: "us-east-1", JSONReadOnly: true, HTTPClient: &http.Client{Transport: rewriteTransport{base: http.DefaultTransport, target: server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	if token, err := m.AccessToken(context.Background()); err != nil || token != "usable" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestHeaders(t *testing.T) {
	m, _ := New(Options{RefreshToken: "x", Region: "us-east-1"})
	h := Headers(m, "token")
	if h.Get("Authorization") != "Bearer token" || h.Get("x-amz-target") == "" || h.Get("amz-sdk-invocation-id") == "" {
		t.Fatalf("headers=%v", h)
	}
}

func TestMalformedRegionsRejected(t *testing.T) {
	for _, opts := range []Options{
		{RefreshToken: "x", Region: "us east 1"},
		{RefreshToken: "x", Region: "us-east-1", APIRegion: "bad/region"},
	} {
		if _, err := New(opts); err == nil {
			t.Fatalf("New(%+v) unexpectedly succeeded", opts)
		}
	}

	path := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(path, []byte(`{"refreshToken":"x","region":"bad region"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{CredentialsFile: path, Region: "us-east-1"}); err == nil {
		t.Fatal("malformed credential region unexpectedly accepted")
	}
}

func TestConcurrentRefreshAndMetadataReads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "access", "refreshToken": "refresh", "profileArn": "arn:test", "expiresIn": 3600,
		})
	}))
	defer server.Close()
	m, err := New(Options{
		RefreshToken: "refresh", ProfileARN: "arn:initial", Region: "us-east-1",
		HTTPClient: &http.Client{Transport: rewriteTransport{base: http.DefaultTransport, target: server.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(refresh bool) {
			defer wg.Done()
			for range 50 {
				if refresh {
					if _, err := m.ForceRefresh(context.Background()); err != nil {
						t.Errorf("ForceRefresh: %v", err)
						return
					}
				}
				_ = m.ProfileARN()
				_ = m.Region()
				_ = m.APIHost()
				_ = m.QHost()
				_ = m.Fingerprint()
				_ = m.Type()
				_ = Headers(m, "token")
			}
		}(worker%4 == 0)
	}
	wg.Wait()
}
