// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package accounts

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/auth"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	"github.com/jwadow/kiro-gateway-go/internal/model"
)

type CredentialType string

const (
	JSONCredential         CredentialType = "json"
	SQLiteCredential       CredentialType = "sqlite"
	RefreshTokenCredential CredentialType = "refresh_token"
)

// Credential is one expanded account entry. Path is absolute for file-backed
// credentials and ID is stable across process restarts.
type Credential struct {
	ID           string         `json:"-"`
	Type         CredentialType `json:"type"`
	Path         string         `json:"path,omitempty"`
	Enabled      *bool          `json:"enabled,omitempty"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	ProfileARN   string         `json:"profile_arn,omitempty"`
	Region       string         `json:"region,omitempty"`
	APIRegion    string         `json:"api_region,omitempty"`
}

func (c Credential) isEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Auth is the subset of internal/auth used by account-aware runtime code.
type Auth interface {
	AccessToken(context.Context) (string, error)
	ForceRefresh(context.Context) (string, error)
	ProfileARN() string
	Region() string
	APIHost() string
	QHost() string
	Fingerprint() string
	Type() auth.Type
}

// Initialized contains dependencies constructed lazily for an account.
type Initialized struct {
	Auth     Auth
	Cache    *model.Cache
	Resolver *model.Resolver
}

// Initializer isolates token verification and model discovery. Tests can
// provide an offline implementation; DefaultInitializer integrates auth,
// model, and config from the current branch.
type Initializer interface {
	Initialize(context.Context, Credential) (Initialized, error)
	RefreshModels(context.Context, Credential, Initialized) ([]model.Info, error)
}

type Stats struct {
	TotalRequests      uint64 `json:"total_requests"`
	SuccessfulRequests uint64 `json:"successful_requests"`
	FailedRequests     uint64 `json:"failed_requests"`
}

type Account struct {
	ID       string
	Auth     Auth
	Cache    *model.Cache
	Resolver *model.Resolver

	Failures       int
	LastFailure    time.Time
	ModelsCachedAt time.Time
	Stats          Stats

	initMu sync.Mutex
}

type Snapshot struct {
	ID             string
	Initialized    bool
	Failures       int
	LastFailure    time.Time
	ModelsCachedAt time.Time
	Stats          Stats
}

type Options struct {
	CredentialsFile string
	StateFile       string

	RecoveryTimeout          time.Duration
	MaxBackoffMultiplier     float64
	ProbabilisticRetryChance float64
	CacheTTL                 time.Duration
	SaveInterval             time.Duration
	MultiAccount             bool

	Initializer Initializer
	Logger      *slog.Logger
	Now         func() time.Time
	Random      func() float64
}

func optionsFromConfig(cfg config.Config) Options {
	return Options{
		CredentialsFile:          cfg.AccountsConfigFile,
		StateFile:                cfg.AccountsStateFile,
		RecoveryTimeout:          cfg.AccountRecoveryTimeout,
		MaxBackoffMultiplier:     cfg.AccountMaxBackoffMultiplier,
		ProbabilisticRetryChance: cfg.AccountProbabilisticRetryChance,
		CacheTTL:                 cfg.AccountCacheTTL,
		SaveInterval:             cfg.StateSaveInterval,
		MultiAccount:             cfg.AccountSystem,
		Initializer:              NewDefaultInitializer(cfg),
	}
}

func applyDefaults(o *Options) {
	if o.RecoveryTimeout <= 0 {
		o.RecoveryTimeout = time.Minute
	}
	if o.MaxBackoffMultiplier <= 0 {
		o.MaxBackoffMultiplier = 1440
	}
	if o.ProbabilisticRetryChance < 0 {
		o.ProbabilisticRetryChance = 0
	}
	if o.ProbabilisticRetryChance > 1 {
		o.ProbabilisticRetryChance = 1
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 12 * time.Hour
	}
	if o.SaveInterval <= 0 {
		o.SaveInterval = 10 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Random == nil {
		o.Random = rand.Float64
	}
}
