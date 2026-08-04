// Kiro Gateway
// Copyright (C) 2025 Jwadow
// SPDX-License-Identifier: AGPL-3.0-or-later

package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/atomicfile"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	"github.com/jwadow/kiro-gateway-go/internal/model"
)

type Manager struct {
	mu     sync.RWMutex
	saveMu sync.Mutex

	opts        Options
	credentials map[string]Credential
	accounts    map[string]*Account
	order       []string
	modelMap    map[string][]string
	sticky      int
	version     uint64
	saved       uint64
}

func New(cfg config.Config) (*Manager, error) { return NewWithOptions(optionsFromConfig(cfg)) }

func NewWithOptions(opts Options) (*Manager, error) {
	applyDefaults(&opts)
	if opts.Initializer == nil {
		return nil, errors.New("accounts initializer is required")
	}
	m := &Manager{
		opts: opts, credentials: make(map[string]Credential),
		accounts: make(map[string]*Account), modelMap: make(map[string][]string),
	}
	if err := m.LoadCredentials(); err != nil {
		return nil, err
	}
	if err := m.LoadState(); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadCredentials replaces the configured account set while retaining runtime
// state for IDs that still exist.
func (m *Manager) LoadCredentials() error {
	credentials, err := ExpandCredentials(m.opts.CredentialsFile)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	accounts := make(map[string]*Account, len(credentials))
	configByID := make(map[string]Credential, len(credentials))
	order := make([]string, 0, len(credentials))
	for _, cred := range credentials {
		account := m.accounts[cred.ID]
		if account == nil {
			account = &Account{ID: cred.ID}
		}
		accounts[cred.ID], configByID[cred.ID] = account, cred
		order = append(order, cred.ID)
	}
	m.accounts, m.credentials, m.order = accounts, configByID, order
	if len(order) == 0 || m.sticky >= len(order) {
		m.sticky = 0
	}
	m.filterMappingsLocked()
	return nil
}

func (m *Manager) LoadState() error {
	if m.opts.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(m.opts.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read accounts state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		m.opts.Logger.Warn("ignoring malformed accounts state", "path", m.opts.StateFile, "error", err)
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sticky = state.CurrentAccountIndex
	if len(m.order) == 0 || m.sticky < 0 || m.sticky >= len(m.order) {
		m.sticky = 0
	}
	for id, saved := range state.Accounts {
		if account := m.accounts[id]; account != nil {
			account.Failures = saved.Failures
			account.LastFailure = unixTime(saved.LastFailureTime)
			account.ModelsCachedAt = unixTime(saved.ModelsCachedAt)
			account.Stats = saved.Stats
		}
	}
	m.modelMap = make(map[string][]string, len(state.ModelToAccounts))
	for name, saved := range state.ModelToAccounts {
		m.modelMap[name] = uniqueStrings(saved.Accounts)
	}
	m.filterMappingsLocked()
	return nil
}

// GetNextAccount selects from the global sticky position. Excluded IDs are
// normally the accounts already attempted by one request's failover loop.
func (m *Manager) GetNextAccount(ctx context.Context, modelName string, excluded map[string]struct{}) (*Account, error) {
	m.mu.RLock()
	order := append([]string(nil), m.order...)
	start := m.sticky
	m.mu.RUnlock()
	if len(order) == 0 {
		return nil, nil
	}
	if !m.opts.MultiAccount {
		id := order[0]
		if _, skip := excluded[id]; skip {
			return nil, nil
		}
		account, err := m.ensureInitialized(ctx, id)
		if err != nil {
			return nil, err
		}
		return account, nil
	}
	if start < 0 || start >= len(order) {
		start = 0
	}

	for offset := range order {
		id := order[(start+offset)%len(order)]
		if _, skip := excluded[id]; skip {
			continue
		}
		if len(order) > 1 && !m.retryEligible(id) {
			continue
		}
		account, err := m.ensureInitialized(ctx, id)
		if err != nil {
			m.markInitializationFailure(id)
			continue
		}
		if err := m.refreshIfStale(ctx, id); err != nil {
			// A stale cache remains usable, matching the Python implementation.
		}
		return account, nil
	}
	return nil, nil
}

func (m *Manager) retryEligible(id string) bool {
	m.mu.RLock()
	account := m.accounts[id]
	if account == nil || account.Failures == 0 {
		m.mu.RUnlock()
		return account != nil
	}
	failures, failedAt := account.Failures, account.LastFailure
	m.mu.RUnlock()
	if failedAt.IsZero() || !m.opts.Now().Before(failedAt.Add(m.Backoff(failures))) {
		return true
	}
	return m.opts.Random() <= m.opts.ProbabilisticRetryChance
}

func (m *Manager) ensureInitialized(ctx context.Context, id string) (*Account, error) {
	m.mu.RLock()
	account, cred := m.accounts[id], m.credentials[id]
	m.mu.RUnlock()
	if account == nil {
		return nil, errors.New("account no longer exists")
	}
	account.initMu.Lock()
	defer account.initMu.Unlock()

	m.mu.RLock()
	initialized := account.Auth != nil
	m.mu.RUnlock()
	if initialized {
		return account, nil
	}
	value, err := m.opts.Initializer.Initialize(ctx, cred)
	if err != nil {
		return nil, err
	}
	if value.Auth == nil || value.Cache == nil || value.Resolver == nil {
		return nil, errors.New("initializer returned incomplete account dependencies")
	}
	now := m.opts.Now()
	m.mu.Lock()
	if m.accounts[id] != account {
		m.mu.Unlock()
		return nil, errors.New("account removed during initialization")
	}
	account.Auth, account.Cache, account.Resolver = value.Auth, value.Cache, value.Resolver
	account.ModelsCachedAt = now
	m.learnModelsLocked(id, modelIDs(value.Resolver))
	m.markDirtyLocked()
	m.mu.Unlock()
	return account, nil
}

func (m *Manager) refreshIfStale(ctx context.Context, id string) error {
	m.mu.RLock()
	account, cred := m.accounts[id], m.credentials[id]
	if account == nil || account.ModelsCachedAt.IsZero() || m.opts.Now().Sub(account.ModelsCachedAt) <= m.opts.CacheTTL {
		m.mu.RUnlock()
		return nil
	}
	m.mu.RUnlock()
	account.initMu.Lock()
	defer account.initMu.Unlock()

	m.mu.RLock()
	if m.opts.Now().Sub(account.ModelsCachedAt) <= m.opts.CacheTTL {
		m.mu.RUnlock()
		return nil
	}
	initialized := Initialized{Auth: account.Auth, Cache: account.Cache, Resolver: account.Resolver}
	m.mu.RUnlock()
	models, err := m.opts.Initializer.RefreshModels(ctx, cred, initialized)
	if err != nil {
		return err
	}
	initialized.Cache.Update(models)
	m.mu.Lock()
	account.ModelsCachedAt = m.opts.Now()
	m.learnModelsLocked(id, modelIDs(initialized.Resolver))
	m.markDirtyLocked()
	m.mu.Unlock()
	return nil
}

func (m *Manager) markInitializationFailure(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if account := m.accounts[id]; account != nil {
		account.Failures++
		account.LastFailure = m.opts.Now()
		m.markDirtyLocked()
	}
}

func (m *Manager) ReportSuccess(accountID, modelName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.accounts[accountID]
	if account == nil {
		return
	}
	account.Failures = 0
	account.Stats.TotalRequests++
	account.Stats.SuccessfulRequests++
	m.learnModelsLocked(accountID, []string{model.Normalize(modelName)})
	for index, id := range m.order {
		if id == accountID {
			m.sticky = index
			break
		}
	}
	m.markDirtyLocked()
}

func (m *Manager) ReportFailure(accountID, modelName string, kind ErrorType, statusCode int, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.accounts[accountID]
	if account == nil {
		return
	}
	if reason == "INVALID_MODEL_ID" {
		account.Stats.TotalRequests++
		m.markDirtyLocked()
		return
	}
	if kind == Recoverable {
		account.Failures++
		account.LastFailure = m.opts.Now()
	}
	account.Stats.TotalRequests++
	account.Stats.FailedRequests++
	m.markDirtyLocked()
}

func (m *Manager) Backoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	multiplier := math.Pow(2, float64(failures-1))
	if multiplier > m.opts.MaxBackoffMultiplier || math.IsInf(multiplier, 1) {
		multiplier = m.opts.MaxBackoffMultiplier
	}
	return time.Duration(float64(m.opts.RecoveryTimeout) * multiplier)
}

// InitializeFirstWorking initializes one account, beginning at the persisted
// sticky index and trying every configured account once. It is used at startup
// and by endpoints that require account-owned state before the first request.
func (m *Manager) InitializeFirstWorking(ctx context.Context) (*Account, error) {
	m.mu.RLock()
	order := append([]string(nil), m.order...)
	start := m.sticky
	m.mu.RUnlock()
	if len(order) == 0 {
		return nil, errors.New("no accounts configured")
	}
	if !m.opts.MultiAccount {
		account, err := m.ensureInitialized(ctx, order[0])
		if err != nil {
			return nil, fmt.Errorf("failed to initialize first account %s: %w", order[0], err)
		}
		return account, nil
	}
	if start < 0 || start >= len(order) {
		start = 0
	}
	var failures []error
	for offset := range order {
		id := order[(start+offset)%len(order)]
		account, err := m.ensureInitialized(ctx, id)
		if err == nil {
			return account, nil
		}
		m.markInitializationFailure(id)
		failures = append(failures, fmt.Errorf("account %s: %w", id, err))
	}
	return nil, fmt.Errorf("failed to initialize any account: %w", errors.Join(failures...))
}

func (m *Manager) FirstInitialized() (*Account, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.order {
		if account := m.accounts[id]; account != nil && account.Auth != nil {
			return account, nil
		}
	}
	return nil, errors.New("no initialized accounts available")
}

func (m *Manager) AvailableModels() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := make(map[string]struct{})
	for _, account := range m.accounts {
		if account.Resolver != nil {
			for _, id := range account.Resolver.Available() {
				set[id] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) Accounts() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Snapshot, 0, len(m.order))
	for _, id := range m.order {
		a := m.accounts[id]
		out = append(out, Snapshot{id, a.Auth != nil, a.Failures, a.LastFailure, a.ModelsCachedAt, a.Stats})
	}
	return out
}

// AccountsForModel returns the persisted, dynamically learned model mapping.
func (m *Manager) AccountsForModel(modelName string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.modelMap[model.Normalize(modelName)]...)
}

func (m *Manager) learnModelsLocked(id string, names []string) {
	for _, name := range names {
		if name == "" || contains(m.modelMap[name], id) {
			continue
		}
		m.modelMap[name] = append(m.modelMap[name], id)
	}
}

func (m *Manager) filterMappingsLocked() {
	for name, ids := range m.modelMap {
		filtered := ids[:0]
		for _, id := range ids {
			if _, ok := m.accounts[id]; ok && !contains(filtered, id) {
				filtered = append(filtered, id)
			}
		}
		if len(filtered) == 0 {
			delete(m.modelMap, name)
		} else {
			m.modelMap[name] = filtered
		}
	}
}

func (m *Manager) markDirtyLocked() { m.version++ }
func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}
func unixTime(seconds float64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	whole, fraction := math.Modf(seconds)
	return time.Unix(int64(whole), int64(fraction*float64(time.Second)))
}

// RunStateSaver persists dirty state until ctx is cancelled. Transient write
// failures leave the current version dirty and are retried with bounded backoff.
func (m *Manager) RunStateSaver(ctx context.Context) error {
	const maxRetryDelay = 30 * time.Second
	baseDelay := m.opts.SaveInterval
	if baseDelay > maxRetryDelay {
		baseDelay = maxRetryDelay
	}
	timer := time.NewTimer(m.opts.SaveInterval)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			if err := m.SaveState(); err != nil {
				m.opts.Logger.Warn("final accounts state save failed", "error", err)
			}
			return ctx.Err()
		case <-timer.C:
			delay := m.opts.SaveInterval
			if err := m.SaveState(); err != nil {
				failures++
				shift := failures - 1
				if shift > 20 {
					shift = 20
				}
				delay = baseDelay * time.Duration(1<<shift)
				if delay > maxRetryDelay || delay < 0 {
					delay = maxRetryDelay
				}
				jitter := 0.5 + m.opts.Random()
				delay = time.Duration(float64(delay) * jitter)
				if delay > maxRetryDelay {
					delay = maxRetryDelay
				}
				m.opts.Logger.Warn("accounts state save failed; retrying", "error", err, "retry_in", delay)
			} else {
				failures = 0
			}
			timer.Reset(delay)
		}
	}
}

func (m *Manager) SaveState() error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.RLock()
	state, version := m.persistedLocked(), m.version
	alreadySaved := version == m.saved
	m.mu.RUnlock()
	if m.opts.StateFile == "" {
		return nil
	}
	if alreadySaved {
		if _, err := os.Stat(m.opts.StateFile); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(m.opts.StateFile, data, 0600); err != nil {
		return err
	}
	m.mu.Lock()
	if version > m.saved {
		m.saved = version
	}
	m.mu.Unlock()
	return nil
}

type persistedState struct {
	CurrentAccountIndex int                               `json:"current_account_index"`
	Accounts            map[string]persistedAccount       `json:"accounts"`
	ModelToAccounts     map[string]persistedModelAccounts `json:"model_to_accounts"`
}
type persistedAccount struct {
	Failures        int     `json:"failures"`
	LastFailureTime float64 `json:"last_failure_time"`
	ModelsCachedAt  float64 `json:"models_cached_at"`
	Stats           Stats   `json:"stats"`
}
type persistedModelAccounts struct {
	Accounts []string `json:"accounts"`
}

func (m *Manager) persistedLocked() persistedState {
	state := persistedState{m.sticky, make(map[string]persistedAccount, len(m.accounts)), make(map[string]persistedModelAccounts, len(m.modelMap))}
	for id, a := range m.accounts {
		state.Accounts[id] = persistedAccount{a.Failures, floatSeconds(a.LastFailure), floatSeconds(a.ModelsCachedAt), a.Stats}
	}
	for name, ids := range m.modelMap {
		state.ModelToAccounts[name] = persistedModelAccounts{append([]string(nil), ids...)}
	}
	return state
}
func floatSeconds(value time.Time) float64 {
	if value.IsZero() {
		return 0
	}
	return float64(value.Unix()) + float64(value.Nanosecond())/float64(time.Second)
}
