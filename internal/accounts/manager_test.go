package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jwadow/kiro-gateway-go/internal/auth"
	"github.com/jwadow/kiro-gateway-go/internal/config"
	"github.com/jwadow/kiro-gateway-go/internal/model"
)

type fakeAuth struct{ id string }

func (a *fakeAuth) AccessToken(context.Context) (string, error)  { return "token-" + a.id, nil }
func (a *fakeAuth) ForceRefresh(context.Context) (string, error) { return "token-" + a.id, nil }
func (a *fakeAuth) ProfileARN() string                           { return "" }
func (a *fakeAuth) Region() string                               { return "us-east-1" }
func (a *fakeAuth) APIHost() string                              { return "https://runtime.us-east-1.kiro.dev" }
func (a *fakeAuth) QHost() string                                { return a.APIHost() }
func (a *fakeAuth) Fingerprint() string                          { return a.id }
func (a *fakeAuth) Type() auth.Type                              { return auth.Desktop }

type fakeInitializer struct {
	mu        sync.Mutex
	calls     map[string]int
	refreshes map[string]int
	models    []model.Info
	gate      chan struct{}
	fail      map[string]error
}

func (f *fakeInitializer) Initialize(ctx context.Context, cred Credential) (Initialized, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[cred.ID]++
	failure := f.fail[cred.ID]
	f.mu.Unlock()
	if failure != nil {
		return Initialized{}, failure
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return Initialized{}, ctx.Err()
		}
	}
	cache := model.NewCache(time.Hour, 200000)
	cache.Update(f.models)
	return Initialized{
		Auth: &fakeAuth{id: cred.ID}, Cache: cache,
		Resolver: model.NewResolver(cache, nil, nil, nil),
	}, nil
}
func (f *fakeInitializer) RefreshModels(_ context.Context, cred Credential, _ Initialized) ([]model.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshes == nil {
		f.refreshes = map[string]int{}
	}
	f.refreshes[cred.ID]++
	return append([]model.Info(nil), f.models...), nil
}
func (f *fakeInitializer) callCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}
func (f *fakeInitializer) refreshCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes[id]
}

func makeManager(t *testing.T, count int, init Initializer, now *time.Time, random func() float64) (*Manager, []string, string) {
	t.Helper()
	dir := t.TempDir()
	entries := make([]map[string]any, 0, count)
	ids := make([]string, 0, count)
	for index := range count {
		path := filepath.Join(dir, "account-"+string(rune('a'+index))+".json")
		if err := os.WriteFile(path, []byte(`{"refreshToken":"token"}`), 0600); err != nil {
			t.Fatal(err)
		}
		abs, _ := filepath.Abs(path)
		ids = append(ids, abs)
		entries = append(entries, map[string]any{"type": "json", "path": path})
	}
	data, _ := json.Marshal(entries)
	credentials := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(credentials, data, 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state.json")
	manager, err := NewWithOptions(Options{
		CredentialsFile: credentials, StateFile: state, Initializer: init,
		RecoveryTimeout: time.Minute, MaxBackoffMultiplier: 8,
		ProbabilisticRetryChance: .1, CacheTTL: time.Hour, MultiAccount: true,
		Now: func() time.Time { return *now }, Random: random,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, ids, state
}

func TestLegacyModeUsesOnlyFirstAccount(t *testing.T) {
	now := time.Unix(1000, 0)
	initializer := &fakeInitializer{models: []model.Info{{ModelID: "model"}}}
	manager, ids, _ := makeManager(t, 2, initializer, &now, func() float64 { return 1 })
	manager.opts.MultiAccount = false
	initializer.fail = map[string]error{ids[0]: context.DeadlineExceeded}
	if _, err := manager.InitializeFirstWorking(context.Background()); err == nil {
		t.Fatal("expected first account failure")
	}
	if initializer.callCount(ids[1]) != 0 {
		t.Fatalf("second account initialized %d times", initializer.callCount(ids[1]))
	}
}

func TestInitializeFirstWorkingTriesFullCircle(t *testing.T) {
	now := time.Unix(1000, 0)
	initializer := &fakeInitializer{models: []model.Info{{ModelID: "dynamic-model"}}}
	manager, ids, _ := makeManager(t, 2, initializer, &now, func() float64 { return 1 })
	initializer.fail = map[string]error{ids[0]: context.DeadlineExceeded}
	account, err := manager.InitializeFirstWorking(context.Background())
	if err != nil || account == nil || account.ID != ids[1] {
		t.Fatalf("account=%v err=%v", account, err)
	}
	if initializer.callCount(ids[0]) != 1 || initializer.callCount(ids[1]) != 1 {
		t.Fatalf("calls=%v", initializer.calls)
	}
	models := manager.AvailableModels()
	if len(models) != 1 || models[0] != "dynamic-model" {
		t.Fatalf("models=%v", models)
	}
}

func TestInitializeFirstWorkingErrorsWithoutAccounts(t *testing.T) {
	now := time.Unix(1000, 0)
	manager, _, _ := makeManager(t, 0, &fakeInitializer{}, &now, func() float64 { return 1 })
	if _, err := manager.InitializeFirstWorking(context.Background()); err == nil {
		t.Fatal("expected no accounts error")
	}
}

func TestDefaultInitializerRuntimeStaticAndLegacyDynamic(t *testing.T) {
	fallback := []config.Model{{ModelID: "fallback-model"}}
	base := config.Config{FallbackModels: fallback, ModelCacheTTL: time.Hour, DefaultMaxInputTokens: 200000, MaxRetries: 1}

	t.Run("runtime stays static", func(t *testing.T) {
		initializer := NewDefaultInitializer(base)
		cache := model.NewCache(time.Hour, 200000)
		cache.Update([]model.Info{{ModelID: "old"}})
		models, err := initializer.RefreshModels(context.Background(), Credential{}, Initialized{Auth: &fakeAuth{id: "runtime"}, Cache: cache})
		if err != nil || len(models) != 1 || models[0].ModelID != "fallback-model" {
			t.Fatalf("models=%v err=%v", models, err)
		}
	})

	t.Run("legacy fetches dynamic and reports refresh errors", func(t *testing.T) {
		fail := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"modelId": "dynamic-model"}}})
		}))
		defer server.Close()
		authn := &legacyFakeAuth{fakeAuth: fakeAuth{id: "legacy"}, host: server.URL}
		initializer := NewDefaultInitializerWithClient(base, server.Client())
		models, err := initializer.RefreshModels(context.Background(), Credential{}, Initialized{Auth: authn})
		if err != nil || len(models) != 1 || models[0].ModelID != "dynamic-model" {
			t.Fatalf("models=%v err=%v", models, err)
		}
		fail = true
		if _, err := initializer.RefreshModels(context.Background(), Credential{}, Initialized{Auth: authn}); err == nil {
			t.Fatal("expected refresh error so caller can preserve stale cache")
		}
	})
}

type legacyFakeAuth struct {
	fakeAuth
	host string
}

func (a *legacyFakeAuth) APIHost() string { return a.host }
func (a *legacyFakeAuth) QHost() string   { return a.host }

func TestConcurrentLazyInitializationOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	initializer := &fakeInitializer{models: []model.Info{{ModelID: "claude-sonnet-4"}}, gate: make(chan struct{})}
	manager, ids, _ := makeManager(t, 1, initializer, &now, func() float64 { return 1 })

	const workers = 32
	results := make(chan *Account, workers)
	errors := make(chan error, workers)
	for range workers {
		go func() {
			account, err := manager.GetNextAccount(context.Background(), "claude-sonnet-4", nil)
			results <- account
			errors <- err
		}()
	}
	close(initializer.gate)
	for range workers {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		if account := <-results; account == nil || account.ID != ids[0] {
			t.Fatalf("account=%v", account)
		}
	}
	if got := initializer.callCount(ids[0]); got != 1 {
		t.Fatalf("Initialize called %d times", got)
	}
}

func TestFailoverStickyCooldownAndProbabilisticRetry(t *testing.T) {
	now := time.Unix(1000, 0)
	random := .5
	initializer := &fakeInitializer{models: []model.Info{{ModelID: "claude-sonnet-4"}}}
	manager, ids, _ := makeManager(t, 2, initializer, &now, func() float64 { return random })

	first, err := manager.GetNextAccount(context.Background(), "claude-sonnet-4", nil)
	if err != nil || first.ID != ids[0] {
		t.Fatalf("first=%v err=%v", first, err)
	}
	manager.ReportFailure(ids[0], "claude-sonnet-4", Recoverable, 429, "")
	second, err := manager.GetNextAccount(context.Background(), "claude-sonnet-4", nil)
	if err != nil || second.ID != ids[1] {
		t.Fatalf("second=%v err=%v", second, err)
	}
	manager.ReportSuccess(ids[1], "new-model")
	sticky, _ := manager.GetNextAccount(context.Background(), "another-model", nil)
	if sticky.ID != ids[1] {
		t.Fatalf("sticky account=%q, want %q", sticky.ID, ids[1])
	}

	manager.ReportFailure(ids[1], "claude-sonnet-4", Recoverable, 429, "")
	random = .05
	probe, _ := manager.GetNextAccount(context.Background(), "claude-sonnet-4", nil)
	if probe.ID != ids[1] {
		t.Fatalf("probabilistic probe=%q, want sticky %q", probe.ID, ids[1])
	}

	now = now.Add(time.Minute)
	random = .5
	manager.ReportSuccess(ids[0], "claude-sonnet-4")
	recovered, _ := manager.GetNextAccount(context.Background(), "claude-sonnet-4", nil)
	if recovered.ID != ids[0] {
		t.Fatalf("recovered=%q", recovered.ID)
	}
	if manager.Backoff(1) != time.Minute || manager.Backoff(2) != 2*time.Minute || manager.Backoff(10) != 8*time.Minute {
		t.Fatalf("unexpected backoff: %v %v %v", manager.Backoff(1), manager.Backoff(2), manager.Backoff(10))
	}
}

func TestSingleAccountBypassesCooldownAndRefreshesStaleCache(t *testing.T) {
	now := time.Unix(1000, 0)
	initializer := &fakeInitializer{models: []model.Info{{ModelID: "claude-sonnet-4"}}}
	manager, ids, _ := makeManager(t, 1, initializer, &now, func() float64 { return 1 })
	account, _ := manager.GetNextAccount(context.Background(), "x", nil)
	manager.ReportFailure(ids[0], "x", Recoverable, 429, "")
	now = now.Add(2 * time.Hour)
	account, err := manager.GetNextAccount(context.Background(), "x", nil)
	if err != nil || account == nil {
		t.Fatalf("account=%v err=%v", account, err)
	}
	if got := initializer.refreshCount(ids[0]); got != 1 {
		t.Fatalf("refresh count=%d", got)
	}
}

func TestFailureAccountingAndAtomicStateRoundTrip(t *testing.T) {
	now := time.Unix(1704114000, 250000000)
	initializer := &fakeInitializer{models: []model.Info{{ModelID: "claude-sonnet-4"}}}
	manager, ids, state := makeManager(t, 1, initializer, &now, func() float64 { return 1 })
	if _, err := manager.GetNextAccount(context.Background(), "claude-sonnet-4", nil); err != nil {
		t.Fatal(err)
	}
	manager.ReportFailure(ids[0], "missing", Recoverable, 400, "INVALID_MODEL_ID")
	snapshot := manager.Accounts()[0]
	if snapshot.Failures != 0 || snapshot.Stats.TotalRequests != 1 || snapshot.Stats.FailedRequests != 0 {
		t.Fatalf("invalid-model snapshot=%+v", snapshot)
	}
	manager.ReportFailure(ids[0], "claude-sonnet-4", Fatal, 400, "CONTENT_LENGTH_EXCEEDS_THRESHOLD")
	manager.ReportFailure(ids[0], "claude-sonnet-4", Recoverable, 429, "")
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("state tmp remains: %v", err)
	}

	reloaded, err := NewWithOptions(Options{
		CredentialsFile: manager.opts.CredentialsFile, StateFile: state, Initializer: initializer,
		RecoveryTimeout: time.Minute, MaxBackoffMultiplier: 8, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Accounts()[0]
	if got.Failures != 1 || got.Stats.TotalRequests != 3 || got.Stats.FailedRequests != 2 {
		t.Fatalf("reloaded snapshot=%+v", got)
	}
	if !got.LastFailure.Equal(now) {
		t.Fatalf("last failure=%v want=%v", got.LastFailure, now)
	}
	mapped := reloaded.AccountsForModel("claude-sonnet-4")
	if len(mapped) != 1 || mapped[0] != ids[0] {
		t.Fatalf("persisted model mapping=%v", mapped)
	}
}

func TestMalformedStateIsIgnored(t *testing.T) {
	now := time.Unix(1000, 0)
	manager, ids, state := makeManager(t, 1, &fakeInitializer{}, &now, func() float64 { return 1 })
	if err := os.WriteFile(state, []byte(`{"accounts":`), 0600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewWithOptions(Options{
		CredentialsFile: manager.opts.CredentialsFile, StateFile: state,
		Initializer: &fakeInitializer{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("malformed state prevented startup: %v", err)
	}
	got := reloaded.Accounts()
	if len(got) != 1 || got[0].ID != ids[0] || got[0].Failures != 0 {
		t.Fatalf("accounts=%+v", got)
	}
}

func TestDefaultInitializerUsesOnlyExplicitGlobalAPIRegion(t *testing.T) {
	if got := apiRegionForCredential(Credential{}, "eu-west-1", true); got != "eu-west-1" {
		t.Fatalf("apiRegion=%q", got)
	}
	if got := apiRegionForCredential(Credential{}, "us-east-1", false); got != "" {
		t.Fatalf("implicit default suppressed credential detection: %q", got)
	}
	if got := apiRegionForCredential(Credential{APIRegion: "ap-southeast-1"}, "eu-west-1", true); got != "ap-southeast-1" {
		t.Fatalf("credential apiRegion=%q", got)
	}
}

func TestSaveEmptyState(t *testing.T) {
	now := time.Unix(1000, 0)
	manager, _, state := makeManager(t, 0, &fakeInitializer{}, &now, func() float64 { return 1 })
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatal(err)
	}
}

func TestRunStateSaverRecoversFromTransientWriteFailure(t *testing.T) {
	now := time.Unix(1000, 0)
	manager, ids, state := makeManager(t, 1, &fakeInitializer{}, &now, func() float64 { return 0 })
	manager.opts.SaveInterval = 5 * time.Millisecond
	manager.ReportFailure(ids[0], "test", Recoverable, 502, "")

	blockingFile := filepath.Dir(state)
	manager.opts.StateFile = filepath.Join(blockingFile, "blocked", "state.json")
	if err := os.WriteFile(filepath.Join(blockingFile, "blocked"), []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.RunStateSaver(ctx) }()
	time.Sleep(15 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("state saver exited after transient failure: %v", err)
	default:
	}
	if err := os.Remove(filepath.Join(blockingFile, "blocked")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(manager.opts.StateFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state saver did not recover")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("state saver error=%v", err)
	}
}
