package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jwadow/kiro-gateway-go/internal/config"
)

func TestHealthcheckCommandFormsAreAccepted(t *testing.T) {
	for _, args := range [][]string{{"healthcheck"}, {"--healthcheck"}} {
		t.Run(args[0], func(t *testing.T) {
			err := run(append([]string{"--host", "127.0.0.1", "--port", "1"}, args...))
			if err == nil || strings.Contains(err.Error(), "unexpected command") {
				t.Fatalf("run(%v) error = %v", args, err)
			}
		})
	}
}

func TestProxyAPIKeyValidation(t *testing.T) {
	for _, value := range []string{"", "   ", "my-super-secret-password-123", "your-gateway-password", "**-*****-******-********-***"} {
		if err := validateProxyAPIKey(value); err == nil {
			t.Fatalf("validateProxyAPIKey(%q) succeeded", value)
		}
	}
	if err := validateProxyAPIKey("unique-secret"); err != nil {
		t.Fatal(err)
	}
}

func TestVersionExemptFromProxyAPIKeyValidation(t *testing.T) {
	t.Setenv("PROXY_API_KEY", "")
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCredentialsFromRefreshToken(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{AccountsConfigFile: filepath.Join(dir, "credentials.json"), RefreshToken: "token", Region: "us-east-1", APIRegion: "us-east-1"}
	if err := prepareCredentials(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.AccountsConfigFile); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCredentialsRequiresCredentials(t *testing.T) {
	cfg := config.Config{AccountsConfigFile: filepath.Join(t.TempDir(), "missing.json")}
	if err := prepareCredentials(cfg); err == nil {
		t.Fatal("expected missing credentials error")
	}
}
