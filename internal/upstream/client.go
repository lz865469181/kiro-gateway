// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/auth"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	"github.com/jwadow/kiro-gateway-go/internal/model"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"
)

type Client struct {
	auth   Auth
	cfg    config.Config
	client *http.Client
}

type Auth interface {
	AccessToken(context.Context) (string, error)
	ForceRefresh(context.Context) (string, error)
	Fingerprint() string
}

type ModelAuth interface {
	Auth
	ProfileARN() string
	QHost() string
	Type() auth.Type
}

// ListAvailableModels fetches the model catalog exposed by legacy Q endpoints.
// Runtime endpoints intentionally do not call this API.
func ListAvailableModels(ctx context.Context, manager ModelAuth, cfg config.Config, injected *http.Client) ([]model.Info, error) {
	client := injected
	var err error
	if client == nil {
		client, err = NewHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}
	attempts := cfg.MaxRetries
	if attempts < 1 {
		attempts = 1
	}
	endpoint, err := url.Parse(strings.TrimRight(manager.QHost(), "/") + "/ListAvailableModels")
	if err != nil {
		return nil, fmt.Errorf("build ListAvailableModels URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("origin", "AI_EDITOR")
	if manager.Type() == auth.Desktop && manager.ProfileARN() != "" {
		query.Set("profileArn", manager.ProfileARN())
	}
	endpoint.RawQuery = query.Encode()

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		token, tokenErr := manager.AccessToken(ctx)
		if tokenErr != nil {
			return nil, tokenErr
		}
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if requestErr != nil {
			return nil, requestErr
		}
		req.Header = runtimeHeaders(manager, token)
		res, requestErr := client.Do(req)
		if requestErr != nil {
			lastErr = requestErr
		} else if res.StatusCode == http.StatusOK {
			var payload struct {
				Models []model.Info `json:"models"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&payload)
			_ = res.Body.Close()
			if decodeErr != nil {
				return nil, fmt.Errorf("decode ListAvailableModels response: %w", decodeErr)
			}
			return payload.Models, nil
		} else {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
			_ = res.Body.Close()
			lastErr = fmt.Errorf("ListAvailableModels HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
			if res.StatusCode == http.StatusForbidden {
				if _, refreshErr := manager.ForceRefresh(ctx); refreshErr != nil {
					return nil, refreshErr
				}
			} else if res.StatusCode != http.StatusTooManyRequests && res.StatusCode < 500 {
				return nil, lastErr
			}
		}
		if attempt+1 < attempts {
			delay := cfg.BaseRetryDelay
			if delay <= 0 {
				delay = time.Second
			}
			if err := sleep(ctx, delay*time.Duration(1<<attempt)); err != nil {
				return nil, err
			}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("ListAvailableModels retries exhausted")
	}
	return nil, lastErr
}

func New(manager *auth.Manager, cfg config.Config) (*Client, error) {
	return NewForAuth(manager, cfg, nil)
}

// NewForAuth builds the runtime client used by account-aware routes. An
// injected client is retained for tests; otherwise the configured proxy is
// applied to a dedicated transport.
func NewForAuth(manager Auth, cfg config.Config, injected *http.Client) (*Client, error) {
	if injected != nil {
		return &Client{auth: manager, cfg: cfg, client: injected}, nil
	}
	client, err := NewHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{auth: manager, cfg: cfg, client: client}, nil
}

// NewHTTPClient returns a proxy-capable client for non-runtime upstream calls.
func NewHTTPClient(cfg config.Config) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.IdleConnTimeout = 90 * time.Second
	transport.ResponseHeaderTimeout = cfg.FirstTokenTimeout
	if cfg.VPNProxyURL != "" {
		if err := configureProxy(transport, cfg.VPNProxyURL); err != nil {
			return nil, err
		}
	}
	return &http.Client{Transport: transport}, nil
}

func configureProxy(transport *http.Transport, raw string) error {
	proxyURL, err := parseProxyURL(raw)
	if err != nil {
		return err
	}

	noProxy := appendNoProxy(environmentNoProxy(), "127.0.0.1", "localhost", "::1")
	bypass := proxyBypass(noProxy)
	switch proxyURL.Scheme {
	case "http", "https":
		cfg := &httpproxy.Config{
			HTTPProxy:  proxyURL.String(),
			HTTPSProxy: proxyURL.String(),
			NoProxy:    noProxy,
		}
		proxyForURL := cfg.ProxyFunc()
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			return proxyForURL(req.URL)
		}
	case "socks5", "socks5h":
		var proxyAuth *proxy.Auth
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			proxyAuth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, proxyAuth, proxy.Direct)
		if err != nil {
			return fmt.Errorf("configure %s proxy: %w", proxyURL.Scheme, err)
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return fmt.Errorf("configure %s proxy: dialer does not support contexts", proxyURL.Scheme)
		}
		transport.Proxy = nil
		transport.DialContext = (&socksDialer{
			proxy:     contextDialer,
			direct:    (&net.Dialer{}).DialContext,
			bypass:    bypass,
			remoteDNS: proxyURL.Scheme == "socks5h",
		}).DialContext
	default:
		return fmt.Errorf("unsupported VPN proxy scheme %q", proxyURL.Scheme)
	}
	return nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	proxyURL, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse VPN proxy URL: %w", err)
	}
	proxyURL.Scheme = strings.ToLower(proxyURL.Scheme)
	if proxyURL.Host == "" || proxyURL.Hostname() == "" {
		return nil, errors.New("parse VPN proxy URL: proxy host is required")
	}
	if proxyURL.Path != "" || proxyURL.RawQuery != "" || proxyURL.Fragment != "" {
		return nil, errors.New("parse VPN proxy URL: path, query, and fragment are not supported")
	}
	return proxyURL, nil
}

func environmentNoProxy() string {
	if value, ok := os.LookupEnv("NO_PROXY"); ok {
		return value
	}
	return os.Getenv("no_proxy")
}

func appendNoProxy(value string, hosts ...string) string {
	entries := strings.Split(value, ",")
	seen := make(map[string]bool, len(entries)+len(hosts))
	result := make([]string, 0, len(entries)+len(hosts))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry != "" && !seen[strings.ToLower(entry)] {
			seen[strings.ToLower(entry)] = true
			result = append(result, entry)
		}
	}
	for _, host := range hosts {
		if !seen[strings.ToLower(host)] {
			seen[strings.ToLower(host)] = true
			result = append(result, host)
		}
	}
	return strings.Join(result, ",")
}

func proxyBypass(noProxy string) func(string) bool {
	cfg := &httpproxy.Config{
		HTTPProxy:  "http://proxy.invalid",
		HTTPSProxy: "http://proxy.invalid",
		NoProxy:    noProxy,
	}
	proxyForURL := cfg.ProxyFunc()
	return func(address string) bool {
		proxyURL, err := proxyForURL(&url.URL{Scheme: "http", Host: address})
		return err == nil && proxyURL == nil
	}
}

type socksDialer struct {
	proxy     proxy.ContextDialer
	direct    func(context.Context, string, string) (net.Conn, error)
	bypass    func(string) bool
	remoteDNS bool
}

func (d *socksDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.bypass(address) {
		return d.direct(ctx, network, address)
	}
	if !d.remoteDNS {
		resolved, err := resolveAddress(ctx, network, address)
		if err != nil {
			return nil, err
		}
		address = resolved
	}
	return d.proxy.DialContext(ctx, network, address)
}

func resolveAddress(ctx context.Context, network, address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return address, err
	}
	addresses, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return "", err
	}
	if len(addresses) == 0 {
		return "", &net.DNSError{Name: host, Err: "no addresses"}
	}
	return net.JoinHostPort(addresses[0], port), nil
}
func (c *Client) CloseIdleConnections() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

type attemptBudgetKey struct{}
type attemptBudget struct {
	mu        sync.Mutex
	remaining int
}

// WithAttemptBudget shares one upstream-attempt limit across transport and
// first-token retry layers. Existing budgets are preserved for nested calls.
func WithAttemptBudget(ctx context.Context, total int) context.Context {
	if _, ok := ctx.Value(attemptBudgetKey{}).(*attemptBudget); ok {
		return ctx
	}
	if total < 1 {
		total = 1
	}
	return context.WithValue(ctx, attemptBudgetKey{}, &attemptBudget{remaining: total})
}

func consumeAttempt(ctx context.Context) bool {
	budget, ok := ctx.Value(attemptBudgetKey{}).(*attemptBudget)
	if !ok {
		return true
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.remaining <= 0 {
		return false
	}
	budget.remaining--
	return true
}

func (c *Client) Do(ctx context.Context, method, url string, payload any, stream bool) (*http.Response, error) {
	attempts := c.cfg.MaxRetries
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	var lastResponse *http.Response
	for attempt := 0; attempt < attempts; attempt++ {
		if !consumeAttempt(ctx) {
			lastErr = errors.New("shared request attempt budget exhausted")
			break
		}
		token, err := c.auth.AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		var body io.Reader
		if payload != nil {
			data, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, err
		}
		req.Header = runtimeHeaders(c.auth, token)
		if stream {
			req.Header.Set("Accept", "application/vnd.amazon.eventstream")
			req.Header.Set("Connection", "close")
		}
		res, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			if !retryableNetwork(err) || attempt == attempts-1 {
				break
			}
			if err = sleep(ctx, c.backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if res.StatusCode == http.StatusOK {
			return res, nil
		}
		if res.StatusCode == http.StatusForbidden {
			drainClose(res)
			if _, err = c.auth.ForceRefresh(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			if attempt < attempts-1 {
				delay := c.backoff(attempt)
				if res.StatusCode == http.StatusTooManyRequests {
					delay = retryAfter(res.Header.Get("Retry-After"), time.Now(), delay)
				}
				drainClose(res)
				if err = sleep(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			return res, nil
		}
		return res, nil
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	if lastErr == nil {
		lastErr = errors.New("request retries exhausted")
	}
	return nil, &NetworkError{Err: lastErr, Message: networkMessage(lastErr)}
}

func runtimeHeaders(manager Auth, token string) http.Header {
	if concrete, ok := manager.(*auth.Manager); ok {
		return auth.Headers(concrete, token)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/x-amz-json-1.0")
	h.Set("x-amz-target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
	h.Set("User-Agent", "aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-0.7.45-"+manager.Fingerprint())
	h.Set("x-amz-user-agent", "aws-sdk-js/1.0.27 KiroIDE-0.7.45-"+manager.Fingerprint())
	h.Set("x-amzn-codewhisperer-optout", "true")
	h.Set("x-amzn-kiro-agent-mode", "vibe")
	h.Set("amz-sdk-invocation-id", fmt.Sprintf("%d", time.Now().UnixNano()))
	h.Set("amz-sdk-request", "attempt=1; max=3")
	return h
}

func (c *Client) backoff(attempt int) time.Duration {
	const capDelay = 30 * time.Second
	delay := c.cfg.BaseRetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	for i := 0; i < attempt && delay < capDelay; i++ {
		delay *= 2
	}
	if delay > capDelay {
		delay = capDelay
	}
	// Full jitter avoids synchronized retry storms while preserving the cap.
	return time.Duration(rand.Float64() * float64(delay))
}

func retryAfter(value string, now time.Time, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds >= 0 {
		if seconds > 30*time.Second {
			return 30 * time.Second
		}
		return seconds
	}
	if date, err := http.ParseTime(value); err == nil {
		delay := date.Sub(now)
		if delay < 0 {
			return 0
		}
		if delay > 30*time.Second {
			return 30 * time.Second
		}
		return delay
	}
	return fallback
}
func drainClose(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
}
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func retryableNetwork(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTemporary || dns.IsTimeout
	}
	return true
}

type NetworkError struct {
	Err     error
	Message string
}

func (e *NetworkError) Error() string { return e.Message }
func (e *NetworkError) Unwrap() error { return e.Err }
func networkMessage(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "DNS resolution failed. Check network connectivity and DNS settings."
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "Connection to Kiro API timed out. Check network or proxy settings."
	}
	return "Unable to connect to Kiro API: " + err.Error()
}
func ReadError(res *http.Response) (string, error) {
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	return string(body), err
}
