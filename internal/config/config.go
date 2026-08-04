// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	AppTitle       = "Kiro Gateway"
	AppDescription = "Proxy gateway for Kiro API. OpenAI and Anthropic compatible."
	AppVersion     = "2.4.dev.13-go"
)

type Model struct {
	ModelID     string         `json:"modelId"`
	ModelName   string         `json:"modelName,omitempty"`
	Description string         `json:"description,omitempty"`
	TokenLimits map[string]any `json:"tokenLimits,omitempty"`
}

type Config struct {
	ServerHost  string
	ServerPort  int
	ProxyAPIKey string
	VPNProxyURL string

	RefreshToken      string
	ProfileARN        string
	Region            string
	APIRegion         string
	APIRegionExplicit bool
	CredentialsFile   string
	CLIDatabaseFile   string
	SQLiteReadOnly    bool
	JSONReadOnly      bool

	MaxRetries               int
	BaseRetryDelay           time.Duration
	TokenRefreshThreshold    time.Duration
	FirstTokenTimeout        time.Duration
	StreamingReadTimeout     time.Duration
	FirstTokenMaxRetries     int
	ModelCacheTTL            time.Duration
	DefaultMaxInputTokens    int
	ToolDescriptionMaxLength int
	TruncationRecovery       bool
	Debug                    bool
	DebugMode                string
	DebugDir                 string

	FakeReasoningEnabled           bool
	FakeReasoningHandling          string
	FakeReasoningMaxTokens         int
	FakeReasoningBudgetCap         int
	FakeReasoningInitialBufferSize int

	MaxPayloadBytes  int
	AutoTrimPayload  bool
	WebSearchEnabled bool

	AccountSystem                   bool
	AccountsConfigFile              string
	AccountsStateFile               string
	AccountRecoveryTimeout          time.Duration
	AccountMaxBackoffMultiplier     float64
	AccountProbabilisticRetryChance float64
	AccountCacheTTL                 time.Duration
	StateSaveInterval               time.Duration

	HiddenModels   map[string]string
	ModelAliases   map[string]string
	HiddenFromList []string
	FallbackModels []Model
}

func Load() (Config, error) {
	_ = loadDotEnv(".env")
	apiRegion, apiRegionExplicit := os.LookupEnv("KIRO_API_REGION")
	cliDBFile := os.Getenv("KIRO_CLI_DB_FILE")
	if cliDBFile == "" {
		cliDBFile = autoDiscoverSQLite()
	}
	cfg := Config{
		ServerHost:                      env("SERVER_HOST", "0.0.0.0"),
		ProxyAPIKey:                     env("PROXY_API_KEY", "my-super-secret-password-123"),
		VPNProxyURL:                     os.Getenv("VPN_PROXY_URL"),
		RefreshToken:                    os.Getenv("REFRESH_TOKEN"),
		ProfileARN:                      os.Getenv("PROFILE_ARN"),
		Region:                          env("KIRO_REGION", "us-east-1"),
		APIRegion:                       apiRegion,
		APIRegionExplicit:               apiRegionExplicit && apiRegion != "",
		CredentialsFile:                 expandPath(os.Getenv("KIRO_CREDS_FILE")),
		CLIDatabaseFile:                 expandPath(cliDBFile),
		SQLiteReadOnly:                  envBool("SQLITE_READONLY", false),
		JSONReadOnly:                    envBool("JSON_READONLY", false),
		MaxRetries:                      envInt("MAX_RETRIES", 3),
		BaseRetryDelay:                  envDurationSeconds("BASE_RETRY_DELAY", 1),
		TokenRefreshThreshold:           10 * time.Minute,
		FirstTokenTimeout:               envDurationSeconds("FIRST_TOKEN_TIMEOUT", 15),
		StreamingReadTimeout:            envDurationSeconds("STREAMING_READ_TIMEOUT", 300),
		FirstTokenMaxRetries:            envInt("FIRST_TOKEN_MAX_RETRIES", 3),
		ModelCacheTTL:                   time.Hour,
		DefaultMaxInputTokens:           200000,
		ToolDescriptionMaxLength:        envInt("TOOL_DESCRIPTION_MAX_LENGTH", 10000),
		TruncationRecovery:              envBool("TRUNCATION_RECOVERY", true),
		Debug:                           envBool("DEBUG", false),
		DebugMode:                       env("DEBUG_MODE", "off"),
		DebugDir:                        env("DEBUG_DIR", "debug_logs"),
		FakeReasoningEnabled:            !envFalse("FAKE_REASONING_ENABLED"),
		FakeReasoningHandling:           env("FAKE_REASONING_HANDLING", "as_reasoning_content"),
		FakeReasoningMaxTokens:          envInt("FAKE_REASONING_MAX_TOKENS", 4000),
		FakeReasoningBudgetCap:          envInt("FAKE_REASONING_BUDGET_CAP", 10000),
		FakeReasoningInitialBufferSize:  envInt("FAKE_REASONING_INITIAL_BUFFER_SIZE", 20),
		MaxPayloadBytes:                 envInt("KIRO_MAX_PAYLOAD_BYTES", 600000),
		AutoTrimPayload:                 envBool("AUTO_TRIM_PAYLOAD", false),
		WebSearchEnabled:                envBool("WEB_SEARCH_ENABLED", true),
		AccountSystem:                   envBool("ACCOUNT_SYSTEM", false),
		AccountsConfigFile:              expandPath(env("ACCOUNTS_CONFIG_FILE", "credentials.json")),
		AccountsStateFile:               expandPath(env("ACCOUNTS_STATE_FILE", "state.json")),
		AccountRecoveryTimeout:          envDurationSeconds("ACCOUNT_RECOVERY_TIMEOUT", 60),
		AccountMaxBackoffMultiplier:     envFloat("ACCOUNT_MAX_BACKOFF_MULTIPLIER", 1440),
		AccountProbabilisticRetryChance: envFloat("ACCOUNT_PROBABILISTIC_RETRY_CHANCE", .1),
		AccountCacheTTL:                 envDurationSeconds("ACCOUNT_CACHE_TTL", 43200),
		StateSaveInterval:               envDurationSeconds("STATE_SAVE_INTERVAL_SECONDS", 10),
		HiddenModels:                    map[string]string{},
		ModelAliases:                    map[string]string{"auto-kiro": "auto"},
		HiddenFromList:                  []string{"auto"},
		FallbackModels:                  fallbackModels(),
	}
	cfg.ServerPort = envInt("SERVER_PORT", 8000)
	if cfg.ServerPort < 1 || cfg.ServerPort > 65535 {
		return Config{}, fmt.Errorf("SERVER_PORT must be between 1 and 65535")
	}
	if err := jsonEnv("MODEL_ALIASES", &cfg.ModelAliases); err != nil {
		return Config{}, err
	}
	if err := jsonEnv("HIDDEN_MODELS", &cfg.HiddenModels); err != nil {
		return Config{}, err
	}
	if err := jsonEnv("HIDDEN_FROM_LIST", &cfg.HiddenFromList); err != nil {
		return Config{}, err
	}
	if cfg.APIRegion == "" {
		cfg.APIRegion = cfg.Region
	}
	if err := validateRegion("KIRO_REGION", cfg.Region); err != nil {
		return Config{}, err
	}
	if err := validateRegion("KIRO_API_REGION", cfg.APIRegion); err != nil {
		return Config{}, err
	}
	switch cfg.DebugMode {
	case "off", "errors", "all":
	default:
		cfg.DebugMode = "off"
	}
	if cfg.FakeReasoningHandling != "as_reasoning_content" && cfg.FakeReasoningHandling != "remove" && cfg.FakeReasoningHandling != "pass" && cfg.FakeReasoningHandling != "strip_tags" {
		cfg.FakeReasoningHandling = "as_reasoning_content"
	}
	return cfg, nil
}

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z0-9]+)+-[0-9]+$`)

// autoDiscoverSQLite checks default kiro-cli database locations when
// KIRO_CLI_DB_FILE is not explicitly configured.
func autoDiscoverSQLite() string {
	candidates := defaultSQLitePaths()
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func defaultSQLitePaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	// Standard Linux/macOS kiro-cli location (~/.local/share/...)
	// Works on Windows too if the CLI uses POSIX-style paths
	out = append(out, filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3"))
	out = append(out, filepath.Join(home, ".local", "share", "amazon-q", "data.sqlite3"))
	// Windows-style local app data
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		out = append(out, filepath.Join(localAppData, "kiro-cli", "data.sqlite3"))
		out = append(out, filepath.Join(localAppData, "amazon-q", "data.sqlite3"))
	}
	return out
}

func validateRegion(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || !regionPattern.MatchString(value) {
		return fmt.Errorf("%s must be a valid AWS region, got %q", name, value)
	}
	return nil
}

func (c Config) APIHost(region string) string {
	if override := os.Getenv("KIRO_API_HOST"); override != "" {
		return strings.TrimRight(override, "/")
	}
	if region == "" {
		region = c.APIRegion
	}
	return "https://runtime." + region + ".kiro.dev"
}
func (c Config) QHost(region string) string {
	if override := os.Getenv("KIRO_Q_HOST"); override != "" {
		return strings.TrimRight(override, "/")
	}
	return c.APIHost(region)
}
func (c Config) DesktopRefreshURL(region string) string {
	return "https://prod." + region + ".auth.desktop.kiro.dev/refreshToken"
}
func (c Config) OIDCURL(region string) string {
	return "https://oidc." + region + ".amazonaws.com/token"
}

func fallbackModels() []Model {
	ids := []string{"auto", "claude-sonnet-4", "claude-sonnet-4.5", "claude-sonnet-4.6", "claude-haiku-4.5", "claude-opus-4.5", "claude-opus-4.6", "claude-opus-4.7", "deepseek-3.2", "glm-5", "minimax-m2.1", "minimax-m2.5", "qwen3-coder-next"}
	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, Model{ModelID: id, ModelName: id, TokenLimits: map[string]any{"maxInputTokens": 200000}})
	}
	return out
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return s.Err()
}
func expandPath(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		return abs
	}
	return filepath.Clean(path)
}
func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
func envInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
func envFloat(name string, fallback float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return n
}
func envBool(name string, fallback bool) bool {
	v := strings.ToLower(os.Getenv(name))
	if v == "" {
		return fallback
	}
	return v == "true" || v == "1" || v == "yes"
}
func envFalse(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "false", "0", "no", "disabled", "off":
		return true
	}
	return false
}
func envDurationSeconds(name string, fallback float64) time.Duration {
	return time.Duration(envFloat(name, fallback) * float64(time.Second))
}
func jsonEnv(name string, target any) error {
	raw := os.Getenv(name)
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	return nil
}
