package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsAndEnv(t *testing.T) {
	t.Setenv("SERVER_PORT", "9000")
	t.Setenv("DEBUG_MODE", "all")
	t.Setenv("FAKE_REASONING_HANDLING", "strip_tags")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerPort != 9000 || cfg.DebugMode != "all" || cfg.FakeReasoningHandling != "strip_tags" {
		t.Fatalf("cfg=%+v", cfg)
	}
}
func TestReadOnlyCredentialConfiguration(t *testing.T) {
	t.Setenv("SQLITE_READONLY", "true")
	t.Setenv("JSON_READONLY", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SQLiteReadOnly || !cfg.JSONReadOnly {
		t.Fatalf("cfg=%+v", cfg)
	}
}

func TestInvalidPort(t *testing.T) {
	t.Setenv("SERVER_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("expected port error")
	}
}

func TestInvalidRegions(t *testing.T) {
	for _, tc := range []struct {
		name, region, apiRegion string
	}{
		{name: "region", region: "us east 1"},
		{name: "API region", region: "us-east-1", apiRegion: "https://evil.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KIRO_REGION", tc.region)
			t.Setenv("KIRO_API_REGION", tc.apiRegion)
			if _, err := Load(); err == nil {
				t.Fatal("expected region validation error")
			}
		})
	}
}
func TestDotEnvPreservesWindowsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("TEST_RAW_PATH=\"D:\\\\Accounts\\\\kiro.json\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_RAW_PATH", "")
	_ = os.Unsetenv("TEST_RAW_PATH")
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("TEST_RAW_PATH"); got != `D:\\Accounts\\kiro.json` {
		t.Fatalf("path=%q", got)
	}
}
func TestModelJSONEnv(t *testing.T) {
	t.Setenv("MODEL_ALIASES", `{"custom":"auto"}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelAliases["custom"] != "auto" {
		t.Fatalf("aliases=%v", cfg.ModelAliases)
	}
}
