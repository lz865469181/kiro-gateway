// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/atomicfile"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	_ "modernc.org/sqlite"
)

type Type string

const (
	Desktop Type = "kiro_desktop"
	OIDC    Type = "aws_sso_oidc"
)

var tokenKeys = []string{"kirocli:social:token", "kirocli:odic:token", "codewhisperer:odic:token"}
var registrationKeys = []string{"kirocli:odic:device-registration", "codewhisperer:odic:device-registration"}

type Options struct {
	RefreshToken, ProfileARN, Region, CredentialsFile, SQLiteFile, APIRegion string
	SQLiteReadOnly, JSONReadOnly                                             bool
	RefreshThreshold                                                         time.Duration
	HTTPClient                                                               *http.Client
	Logger                                                                   *slog.Logger
}
type Manager struct {
	mu                                                                                                     sync.RWMutex
	opts                                                                                                   Options
	accessToken, refreshToken, profileARN, region, ssoRegion, apiRegion, clientID, clientSecret, sqliteKey string
	scopes                                                                                                 []string
	expiresAt                                                                                              time.Time
	authType                                                                                               Type
	fingerprint                                                                                            string
	client                                                                                                 *http.Client
}

func New(opts Options) (*Manager, error) {
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	if err := validateRegion("region", opts.Region); err != nil {
		return nil, err
	}
	if opts.APIRegion != "" {
		if err := validateRegion("API region", opts.APIRegion); err != nil {
			return nil, err
		}
	}
	if opts.RefreshThreshold == 0 {
		opts.RefreshThreshold = 10 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	m := &Manager{opts: opts, refreshToken: opts.RefreshToken, profileARN: opts.ProfileARN, region: opts.Region, apiRegion: opts.APIRegion, authType: Desktop}
	m.client = opts.HTTPClient
	if m.client == nil {
		m.client = &http.Client{Timeout: 30 * time.Second}
	}
	m.fingerprint = fingerprint()
	var err error
	if opts.SQLiteFile != "" {
		err = m.loadSQLite()
	} else if opts.CredentialsFile != "" {
		err = m.loadFile()
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if m.clientID != "" && m.clientSecret != "" {
		m.authType = OIDC
	}
	if m.ssoRegion == "" {
		m.ssoRegion = m.region
	}
	if m.apiRegion == "" {
		m.apiRegion = m.ssoRegion
	}
	if err := validateRegion("SSO region", m.ssoRegion); err != nil {
		return nil, err
	}
	if err := validateRegion("API region", m.apiRegion); err != nil {
		return nil, err
	}
	return m, nil
}

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z0-9]+)+-[0-9]+$`)

func validateRegion(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || !regionPattern.MatchString(value) {
		return fmt.Errorf("invalid %s %q", name, value)
	}
	return nil
}

func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accessToken != "" && !m.expiring() {
		return m.accessToken, nil
	}
	if m.opts.SQLiteFile != "" {
		_ = m.loadSQLite()
		if m.accessToken != "" && !m.expiring() {
			return m.accessToken, nil
		}
	}
	err := m.refresh(ctx)
	if err != nil && m.opts.SQLiteFile != "" && m.accessToken != "" && !m.expired() {
		return m.accessToken, nil
	}
	if err != nil {
		return "", err
	}
	if m.accessToken == "" {
		return "", errors.New("failed to obtain access token")
	}
	return m.accessToken, nil
}
func (m *Manager) ForceRefresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refresh(ctx); err != nil {
		return "", err
	}
	return m.accessToken, nil
}
func (m *Manager) expiring() bool {
	return m.expiresAt.IsZero() || !m.expiresAt.After(time.Now().Add(m.opts.RefreshThreshold))
}
func (m *Manager) expired() bool { return m.expiresAt.IsZero() || !m.expiresAt.After(time.Now()) }
func (m *Manager) refresh(ctx context.Context) error {
	if m.authType == OIDC {
		return m.refreshOIDC(ctx)
	}
	return m.refreshDesktop(ctx)
}
func (m *Manager) refreshDesktop(ctx context.Context) error {
	if m.refreshToken == "" {
		return errors.New("refresh token is not set")
	}
	body, _ := json.Marshal(map[string]any{"refreshToken": m.refreshToken})
	url := "https://prod." + m.ssoRegion + ".auth.desktop.kiro.dev/refreshToken"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build desktop refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "KiroIDE-0.7.45-"+m.fingerprint)
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := m.doJSON(req, &result); err != nil {
		return err
	}
	if result.AccessToken == "" {
		return errors.New("response does not contain accessToken")
	}
	m.accessToken = result.AccessToken
	if result.RefreshToken != "" {
		m.refreshToken = result.RefreshToken
	}
	if result.ProfileARN != "" {
		m.profileARN = result.ProfileARN
	}
	if result.ExpiresIn == 0 {
		result.ExpiresIn = 3600
	}
	m.expiresAt = time.Now().UTC().Truncate(time.Second).Add(time.Duration(result.ExpiresIn-60) * time.Second)
	m.persistBestEffort()
	return nil
}
func (m *Manager) refreshOIDC(ctx context.Context) error {
	err := m.doOIDC(ctx)
	if statusError(err, 400) && m.opts.SQLiteFile != "" {
		if loadErr := m.loadSQLite(); loadErr == nil {
			return m.doOIDC(ctx)
		}
	}
	return err
}
func (m *Manager) doOIDC(ctx context.Context) error {
	if m.refreshToken == "" {
		return errors.New("refresh token is not set")
	}
	if m.clientID == "" {
		return errors.New("client ID is not set (required for AWS SSO OIDC)")
	}
	if m.clientSecret == "" {
		return errors.New("client secret is not set (required for AWS SSO OIDC)")
	}
	body, _ := json.Marshal(map[string]any{"grantType": "refresh_token", "clientId": m.clientID, "clientSecret": m.clientSecret, "refreshToken": m.refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oidc."+m.ssoRegion+".amazonaws.com/token", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build OIDC refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := m.doJSON(req, &result); err != nil {
		return err
	}
	if result.AccessToken == "" {
		return errors.New("AWS SSO OIDC response does not contain accessToken")
	}
	m.accessToken = result.AccessToken
	if result.RefreshToken != "" {
		m.refreshToken = result.RefreshToken
	}
	if result.ExpiresIn == 0 {
		result.ExpiresIn = 3600
	}
	m.expiresAt = time.Now().UTC().Add(time.Duration(result.ExpiresIn-60) * time.Second)
	m.persistBestEffort()
	return nil
}

type HTTPStatusError struct {
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }
func statusError(err error, status int) bool {
	var target *HTTPStatusError
	return errors.As(err, &target) && target.Status == status
}
func (m *Manager) doJSON(req *http.Request, target any) error {
	res, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &HTTPStatusError{res.StatusCode, string(data)}
	}
	return json.Unmarshal(data, target)
}

func (m *Manager) loadFile() error {
	data, err := os.ReadFile(m.opts.CredentialsFile)
	if err != nil {
		return err
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		return err
	}
	m.applyCamel(value)
	if hash, _ := value["clientIdHash"].(string); hash != "" {
		home, _ := os.UserHomeDir()
		deviceData, readErr := os.ReadFile(filepath.Join(home, ".aws", "sso", "cache", hash+".json"))
		if readErr == nil {
			var device map[string]any
			if json.Unmarshal(deviceData, &device) == nil {
				m.applyCamel(device)
			}
		}
	}
	return nil
}
func (m *Manager) applyCamel(v map[string]any) {
	if s, ok := v["accessToken"].(string); ok {
		m.accessToken = s
	}
	if s, ok := v["refreshToken"].(string); ok {
		m.refreshToken = s
	}
	if s, ok := v["profileArn"].(string); ok {
		m.profileARN = s
	}
	if s, ok := v["region"].(string); ok {
		m.ssoRegion = s
		if m.opts.APIRegion == "" {
			m.apiRegion = s
		}
	}
	if s, ok := v["clientId"].(string); ok {
		m.clientID = s
	}
	if s, ok := v["clientSecret"].(string); ok {
		m.clientSecret = s
	}
	if s, ok := v["expiresAt"].(string); ok {
		m.expiresAt = parseTime(s)
	}
}
func (m *Manager) loadSQLite() error {
	db, err := sql.Open("sqlite", m.opts.SQLiteFile)
	if err != nil {
		return err
	}
	defer db.Close()
	var raw string
	for _, key := range tokenKeys {
		if db.QueryRow("SELECT value FROM auth_kv WHERE key = ?", key).Scan(&raw) == nil {
			m.sqliteKey = key
			break
		}
	}
	if raw != "" {
		var v map[string]any
		if json.Unmarshal([]byte(raw), &v) == nil {
			if s, ok := v["access_token"].(string); ok {
				m.accessToken = s
			}
			if s, ok := v["refresh_token"].(string); ok {
				m.refreshToken = s
			}
			if s, ok := v["profile_arn"].(string); ok {
				m.profileARN = s
			}
			if s, ok := v["region"].(string); ok {
				m.ssoRegion = s
			}
			if s, ok := v["expires_at"].(string); ok {
				m.expiresAt = parseTime(s)
			}
			if scopes, ok := v["scopes"].([]any); ok {
				m.scopes = nil
				for _, scope := range scopes {
					if s, ok := scope.(string); ok {
						m.scopes = append(m.scopes, s)
					}
				}
			}
		}
	}
	for _, key := range registrationKeys {
		raw = ""
		if db.QueryRow("SELECT value FROM auth_kv WHERE key = ?", key).Scan(&raw) == nil {
			var v map[string]any
			if json.Unmarshal([]byte(raw), &v) == nil {
				if s, ok := v["client_id"].(string); ok {
					m.clientID = s
				}
				if s, ok := v["client_secret"].(string); ok {
					m.clientSecret = s
				}
				if s, ok := v["region"].(string); ok && m.ssoRegion == "" {
					m.ssoRegion = s
				}
			}
			break
		}
	}
	raw = ""
	if db.QueryRow("SELECT value FROM state WHERE key = 'api.codewhisperer.profile'").Scan(&raw) == nil {
		var v map[string]any
		if json.Unmarshal([]byte(raw), &v) == nil {
			if arn, ok := v["arn"].(string); ok {
				if m.profileARN == "" {
					m.profileARN = arn
				}
				parts := strings.Split(arn, ":")
				if len(parts) > 3 && regexp.MustCompile(`^[a-z]+-[a-z]+-\d+$`).MatchString(parts[3]) && m.opts.APIRegion == "" {
					m.apiRegion = parts[3]
				}
			}
		}
	}
	return nil
}
func (m *Manager) persistBestEffort() {
	if err := m.persist(); err != nil {
		m.opts.Logger.Error("refreshed token persistence failed; using valid in-memory token", "error", err)
	}
}
func (m *Manager) persist() error {
	if m.opts.SQLiteFile != "" {
		if m.opts.SQLiteReadOnly {
			return nil
		}
		return m.saveSQLite()
	}
	if m.opts.CredentialsFile != "" {
		if m.opts.JSONReadOnly {
			return nil
		}
		return m.saveFile()
	}
	return nil
}
func (m *Manager) saveFile() error {
	value := map[string]any{}
	if data, err := os.ReadFile(m.opts.CredentialsFile); err == nil {
		_ = json.Unmarshal(data, &value)
	}
	value["accessToken"] = m.accessToken
	value["refreshToken"] = m.refreshToken
	value["expiresAt"] = m.expiresAt.Format(time.RFC3339Nano)
	if m.profileARN != "" {
		value["profileArn"] = m.profileARN
	}
	data, _ := json.MarshalIndent(value, "", "  ")
	return atomicfile.WriteFile(m.opts.CredentialsFile, data, 0600)
}
func (m *Manager) saveSQLite() error {
	db, err := sql.Open("sqlite", m.opts.SQLiteFile)
	if err != nil {
		return err
	}
	defer db.Close()
	keys := tokenKeys
	if m.sqliteKey != "" {
		keys = append([]string{m.sqliteKey}, keys...)
	}
	for _, key := range keys {
		var raw string
		if db.QueryRow("SELECT value FROM auth_kv WHERE key = ?", key).Scan(&raw) != nil {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(raw), &v) != nil {
			continue
		}
		v["access_token"] = m.accessToken
		v["refresh_token"] = m.refreshToken
		v["expires_at"] = m.expiresAt.Format(time.RFC3339Nano)
		v["region"] = m.ssoRegion
		if len(m.scopes) > 0 {
			v["scopes"] = m.scopes
		}
		data, _ := json.Marshal(v)
		_, err = db.Exec("UPDATE auth_kv SET value = ? WHERE key = ?", string(data), key)
		return err
	}
	return errors.New("no matching SQLite token key")
}
func parseTime(s string) time.Time {
	if i := strings.Index(s, "."); i >= 0 {
		end := i + 1
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end-(i+1) > 9 {
			s = s[:i+10] + s[end:]
		}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
func fingerprint() string {
	host, _ := os.Hostname()
	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	sum := sha256.Sum256([]byte(host + "-" + username + "-kiro-gateway"))
	return hex.EncodeToString(sum[:])
}
func (m *Manager) ProfileARN() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.profileARN
}
func (m *Manager) Region() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.apiRegion
}
func (m *Manager) APIHost() string {
	if host := strings.TrimSpace(os.Getenv("KIRO_API_HOST")); host != "" {
		return strings.TrimRight(host, "/")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return "https://runtime." + m.apiRegion + ".kiro.dev"
}
func (m *Manager) QHost() string {
	if host := strings.TrimSpace(os.Getenv("KIRO_Q_HOST")); host != "" {
		return strings.TrimRight(host, "/")
	}
	if host := strings.TrimSpace(os.Getenv("KIRO_API_HOST")); host != "" {
		return strings.TrimRight(host, "/")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return "https://runtime." + m.apiRegion + ".kiro.dev"
}
func (m *Manager) Fingerprint() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fingerprint
}
func (m *Manager) Type() Type {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.authType
}

func Headers(m *Manager, token string) http.Header {
	fingerprint := m.Fingerprint()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/x-amz-json-1.0")
	h.Set("x-amz-target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
	h.Set("User-Agent", "aws-sdk-js/1.0.27 ua/2.1 os/win32#10.0.19044 lang/js md/nodejs#22.21.1 api/codewhispererstreaming#1.0.27 m/E KiroIDE-0.7.45-"+fingerprint)
	h.Set("x-amz-user-agent", "aws-sdk-js/1.0.27 KiroIDE-0.7.45-"+fingerprint)
	h.Set("x-amzn-codewhisperer-optout", "true")
	h.Set("x-amzn-kiro-agent-mode", "vibe")
	h.Set("amz-sdk-invocation-id", randomUUID())
	h.Set("amz-sdk-request", "attempt=1; max=3")
	return h
}
func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

var _ = config.AppTitle
