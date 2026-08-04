// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package accounts

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/jwadow/kiro-gateway-go/internal/auth"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	"github.com/jwadow/kiro-gateway-go/internal/model"
	"github.com/jwadow/kiro-gateway-go/internal/upstream"
)

type DefaultInitializer struct {
	cfg      config.Config
	fallback []model.Info
	client   *http.Client
	clientMu sync.Mutex
}

func NewDefaultInitializer(cfg config.Config) *DefaultInitializer {
	return NewDefaultInitializerWithClient(cfg, nil)
}

func NewDefaultInitializerWithClient(cfg config.Config, client *http.Client) *DefaultInitializer {
	fallback := make([]model.Info, 0, len(cfg.FallbackModels))
	for _, item := range cfg.FallbackModels {
		fallback = append(fallback, model.Info{
			ModelID: item.ModelID, ModelName: item.ModelName,
			Description: item.Description, TokenLimits: item.TokenLimits,
		})
	}
	return &DefaultInitializer{cfg: cfg, fallback: fallback, client: client}
}

func (i *DefaultInitializer) httpClient() (*http.Client, error) {
	i.clientMu.Lock()
	defer i.clientMu.Unlock()
	if i.client != nil {
		return i.client, nil
	}
	client, err := upstream.NewHTTPClient(i.cfg)
	if err != nil {
		return nil, err
	}
	i.client = client
	return client, nil
}

func (i *DefaultInitializer) Initialize(ctx context.Context, cred Credential) (Initialized, error) {
	client, err := i.httpClient()
	if err != nil {
		return Initialized{}, err
	}
	apiRegion := apiRegionForCredential(cred, i.cfg.APIRegion, i.cfg.APIRegionExplicit)
	opts := auth.Options{
		RefreshToken: cred.RefreshToken, ProfileARN: cred.ProfileARN,
		Region: cred.Region, APIRegion: apiRegion,
		RefreshThreshold: i.cfg.TokenRefreshThreshold,
		SQLiteReadOnly:   i.cfg.SQLiteReadOnly,
		JSONReadOnly:     i.cfg.JSONReadOnly,
		HTTPClient:       client,
	}
	switch cred.Type {
	case JSONCredential:
		opts.CredentialsFile = cred.Path
	case SQLiteCredential:
		opts.SQLiteFile = cred.Path
	case RefreshTokenCredential:
	default:
		return Initialized{}, fmt.Errorf("unsupported credential type %q", cred.Type)
	}
	manager, err := auth.New(opts)
	if err != nil {
		return Initialized{}, err
	}
	if _, err := manager.AccessToken(ctx); err != nil {
		return Initialized{}, err
	}

	cache := model.NewCache(i.cfg.ModelCacheTTL, i.cfg.DefaultMaxInputTokens)
	models := i.fallback
	if !isRuntimeEndpoint(manager.APIHost()) {
		if discovered, discoverErr := upstream.ListAvailableModels(ctx, manager, i.cfg, i.client); discoverErr == nil {
			models = discovered
		}
	}
	cache.Update(models)
	resolver := model.NewResolver(cache, cloneMap(i.cfg.HiddenModels), cloneMap(i.cfg.ModelAliases), append([]string(nil), i.cfg.HiddenFromList...))
	return Initialized{Auth: manager, Cache: cache, Resolver: resolver}, nil
}

// RefreshModels uses static fallback models for runtime.kiro.dev, whose API
// does not implement ListAvailableModels. Legacy endpoints dynamically refresh
// their catalog and preserve the stale cache when discovery fails.
func (i *DefaultInitializer) RefreshModels(ctx context.Context, _ Credential, initialized Initialized) ([]model.Info, error) {
	if initialized.Auth == nil {
		return nil, fmt.Errorf("cannot refresh models without initialized auth")
	}
	if isRuntimeEndpoint(initialized.Auth.APIHost()) {
		return append([]model.Info(nil), i.fallback...), nil
	}
	client, err := i.httpClient()
	if err != nil {
		return nil, err
	}
	return upstream.ListAvailableModels(ctx, initialized.Auth, i.cfg, client)
}

func apiRegionForCredential(cred Credential, global string, globalExplicit bool) string {
	if cred.APIRegion != "" {
		return cred.APIRegion
	}
	if globalExplicit {
		return global
	}
	return ""
}

func isRuntimeEndpoint(host string) bool {
	return strings.Contains(strings.ToLower(host), "://runtime.")
}

func cloneMap(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

func modelIDs(resolver *model.Resolver) []string {
	if resolver == nil {
		return nil
	}
	ids := resolver.Available()
	sort.Strings(ids)
	return ids
}

var _ Initializer = (*DefaultInitializer)(nil)
var _ Auth = (*auth.Manager)(nil)
