// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/atomicfile"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	gatewayserver "github.com/jwadow/kiro-gateway-go/internal/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("kiro-gateway", flag.ContinueOnError)
	host := flags.String("host", cfg.ServerHost, "server host address")
	flags.StringVar(host, "H", cfg.ServerHost, "server host address")
	port := flags.Int("port", cfg.ServerPort, "server port")
	flags.IntVar(port, "p", cfg.ServerPort, "server port")
	version := flags.Bool("version", false, "print version")
	flags.BoolVar(version, "v", false, "print version")
	healthcheck := flags.Bool("healthcheck", false, "check the configured server health endpoint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	positional := flags.Args()
	if len(positional) == 1 && positional[0] == "healthcheck" {
		*healthcheck = true
	} else if len(positional) != 0 {
		return fmt.Errorf("unexpected command %q", positional[0])
	}
	if *version {
		fmt.Println("kiro-gateway " + config.AppVersion)
		return nil
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	cfg.ServerHost, cfg.ServerPort = *host, *port
	if *healthcheck {
		return checkHealth(cfg)
	}
	if err := validateProxyAPIKey(cfg.ProxyAPIKey); err != nil {
		return err
	}
	if err := prepareCredentials(cfg); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	gateway, err := gatewayserver.New(cfg, gatewayserver.Options{Logger: logger})
	if err != nil {
		return fmt.Errorf("initialize gateway: %w", err)
	}
	defer gateway.CloseIdleConnections()
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelStartup()
	if err := gateway.Initialize(startupCtx); err != nil {
		return fmt.Errorf("initialize gateway account: %w", err)
	}
	server := &http.Server{
		Addr: net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.ServerPort)), Handler: gateway.Handler(),
		ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 5 * time.Minute, IdleTimeout: 120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	saverCtx, cancelSaver := context.WithCancel(context.Background())
	defer cancelSaver()
	go func() {
		if err := gateway.RunStateSaver(saverCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("state saver stopped", "error", err)
		}
	}()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", server.Addr, "version", config.AppVersion)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	select {
	case <-ctx.Done():
	case err = <-errCh:
		if err != nil {
			cancelSaver()
			return err
		}
	}
	cancelSaver()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		return err
	}
	return gateway.SaveState()
}

func checkHealth(cfg config.Config) error {
	host := cfg.ServerHost
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(host, strconv.Itoa(cfg.ServerPort)) + "/health")
	if err != nil {
		return fmt.Errorf("healthcheck failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck failed: HTTP %d", res.StatusCode)
	}
	fmt.Println("healthy")
	return nil
}

func validateProxyAPIKey(value string) error {
	key := strings.TrimSpace(value)
	placeholders := map[string]bool{
		"my-super-secret-password-123": true,
		"your-gateway-password":        true,
		"changeme_proxy_secret":        true,
		"**-*****-******-********-***": true,
	}
	if key == "" || placeholders[strings.ToLower(key)] {
		return errors.New("PROXY_API_KEY is required and must be changed from the example placeholder")
	}
	return nil
}

// prepareCredentials preserves the Python gateway's legacy-to-account migration.
func prepareCredentials(cfg config.Config) error {
	if _, err := os.Stat(cfg.AccountsConfigFile); err == nil && cfg.AccountSystem {
		return nil
	}
	type credential struct {
		Type         string `json:"type"`
		Path         string `json:"path,omitempty"`
		RefreshToken string `json:"refresh_token,omitempty"`
		ProfileARN   string `json:"profile_arn,omitempty"`
		Region       string `json:"region,omitempty"`
		APIRegion    string `json:"api_region,omitempty"`
	}
	var entry credential
	switch {
	case fileExists(cfg.CLIDatabaseFile):
		entry = credential{Type: "sqlite", Path: cfg.CLIDatabaseFile}
	case fileExists(cfg.CredentialsFile):
		entry = credential{Type: "json", Path: cfg.CredentialsFile}
	case cfg.RefreshToken != "":
		entry = credential{Type: "refresh_token", RefreshToken: cfg.RefreshToken}
	default:
		if fileExists(cfg.AccountsConfigFile) {
			return nil
		}
		return errors.New("no Kiro credentials configured; set REFRESH_TOKEN, KIRO_CREDS_FILE, KIRO_CLI_DB_FILE, or create credentials.json")
	}
	entry.ProfileARN, entry.Region, entry.APIRegion = cfg.ProfileARN, cfg.Region, cfg.APIRegion
	data, err := json.MarshalIndent([]credential{entry}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(cfg.AccountsConfigFile), 0700); err != nil {
		return err
	}
	return atomicfile.WriteFile(cfg.AccountsConfigFile, data, 0600)
}
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
