package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func poolWithKeys(keys ...string) *AccountPool {
	list := make([]AccountConfig, 0, len(keys))
	for i, key := range keys {
		list = append(list, AccountConfig{Name: fmt.Sprintf("acct-%d", i), APIKey: key})
	}
	return NewAccountPool(list)
}

func chatRequestForTest() *ChatRequest {
	return &ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: TextContent("hello")}},
	}
}

func TestAccountPoolRoundRobin(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b", "key-c")
	var order []string
	for i := 0; i < 6; i++ {
		acct := pool.Acquire()
		if acct == nil {
			t.Fatalf("Acquire() = nil at %d", i)
		}
		order = append(order, acct.APIKey)
	}
	want := []string{"key-a", "key-b", "key-c", "key-a", "key-b", "key-c"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("rotation order = %v, want %v", order, want)
		}
	}
}

func TestAccountPoolSkipsDisabledAndRateLimited(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b", "key-c")
	pool.SetEnabled(accountID("key-a"), false)
	// Rate-limit key-b; only key-c remains eligible.
	b := pool.Get(accountID("key-b"))
	b.RecordFailure(&upstreamAPIError{Status: http.StatusTooManyRequests})

	for i := 0; i < 3; i++ {
		acct := pool.Acquire()
		if acct == nil || acct.APIKey != "key-c" {
			t.Fatalf("Acquire() = %v, want key-c", acct)
		}
	}
}

func TestAccountPoolMutationIsSafeWithRoutingReads(t *testing.T) {
	pool := poolWithKeys("key-a")
	id := accountID("key-a")
	done := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-done:
				return
			default:
				pool.Acquire()
				pool.EnabledCount()
				pool.Primary()
				pool.Views()
			}
		}
	}()
	for i := 0; i < 1_000; i++ {
		if !pool.SetEnabled(id, i%2 == 0) || !pool.Rename(id, fmt.Sprintf("name-%d", i)) {
			t.Fatal("account mutation failed")
		}
	}
	close(done)
	readers.Wait()

	pool.SetEnabled(id, true)
	pool.Rename(id, "final")
	view := pool.Views()[0]
	if !view.Enabled || view.Name != "final" {
		t.Fatalf("final account view = %+v", view)
	}
}

func TestAccountPoolKeyMutationKeepsInFlightAccountStable(t *testing.T) {
	pool := poolWithKeys("key-a")
	inFlight := pool.Acquire()
	if inFlight == nil {
		t.Fatal("Acquire() = nil")
	}
	updated := make(chan struct{})
	go func() {
		if _, err := pool.SetKey(inFlight.ID, "key-b"); err != nil {
			t.Errorf("SetKey: %v", err)
		}
		close(updated)
	}()
	for i := 0; i < 1_000; i++ {
		if inFlight.ID != accountID("key-a") || inFlight.APIKey != "key-a" {
			t.Fatalf("in-flight account changed: %+v", inFlight)
		}
	}
	<-updated
	if current := pool.Get(accountID("key-b")); current == nil || current.APIKey != "key-b" {
		t.Fatalf("updated account = %+v", current)
	}
}

func TestAccountPoolConcurrentDuplicateAdd(t *testing.T) {
	pool := NewAccountPool(nil)
	var wg sync.WaitGroup
	var successes atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.Add("same", "shared-key", true); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, errDuplicateAccount) {
				t.Errorf("Add: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := successes.Load(); got != 1 || pool.Len() != 1 {
		t.Fatalf("successful adds = %d, pool length = %d, want 1/1", got, pool.Len())
	}
}

func TestAccountPoolAllLimitedReturnsNil(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b")
	for _, key := range []string{"key-a", "key-b"} {
		pool.Get(accountID(key)).RecordFailure(&upstreamAPIError{Status: http.StatusTooManyRequests})
	}
	if acct := pool.Acquire(); acct != nil {
		t.Fatalf("Acquire() = %v, want nil when all accounts are limited", acct)
	}
	wait := pool.EarliestRateLimitWait(time.Now())
	if wait <= 0 || wait > defaultRateLimitCooldown+time.Second {
		t.Fatalf("EarliestRateLimitWait() = %v", wait)
	}
}

func TestAccountRecordFailureTracksAuthFailures(t *testing.T) {
	a := newAccount("x", "key-x", true)
	a.RecordFailure(&upstreamAPIError{Status: http.StatusUnauthorized, Message: "bad key"})
	a.RecordFailure(&upstreamAPIError{Status: http.StatusForbidden, Message: "no"})
	view := a.View()
	if view.Status != "ok" || view.AuthFailures != 2 {
		t.Fatalf("view = %+v, want ok with 2 auth failures", view)
	}
	a.RecordSuccess()
	view = a.View()
	if view.AuthFailures != 0 || view.LastError != "" {
		t.Fatalf("success did not reset state: %+v", view)
	}
}

func TestAccountPoolConfigRoundTrip(t *testing.T) {
	pool := poolWithKeys("key-a", "key-b")
	pool.SetEnabled(accountID("key-b"), false)
	pool.Rename(accountID("key-a"), "primary")

	cfg := &Config{}
	pool.SyncToConfig(cfg)
	if got := cfg.Gateways[GatewayCmdcode]; got == nil || len(got.Accounts) != 2 {
		t.Fatalf("gateways.cmdcode = %+v, want 2 accounts", got)
	}
	if len(cfg.CommandCode.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(cfg.CommandCode.Accounts))
	}
	if cfg.CommandCode.Accounts[0].Name != "primary" {
		t.Fatalf("name = %q", cfg.CommandCode.Accounts[0].Name)
	}
	if cfg.CommandCode.Accounts[1].IsEnabled() {
		t.Fatalf("disabled account did not round-trip")
	}

	reloaded := NewAccountPool(cfg.CommandCode.Accounts)
	if reloaded.Len() != 2 || reloaded.EnabledCount() != 1 {
		t.Fatalf("reloaded pool = %d/%d, want 2/1", reloaded.Len(), reloaded.EnabledCount())
	}
}

func TestAccountPoolSyncPersistsGatewayAccounts(t *testing.T) {
	path := writeTempConfig(t, "gateways:\n  cmdcode:\n    base_url: https://api.commandcode.ai\n    accounts:\n    - name: old\n      api_key: old-key\n")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewAccountPool(cfg.CommandCode.Accounts)
	if _, err := pool.SetKey(accountID("old-key"), "new-key"); err != nil {
		t.Fatal(err)
	}
	pool.SyncToConfig(cfg)
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	accounts := reloaded.Gateways[GatewayCmdcode].Accounts
	if len(accounts) != 1 || accounts[0].APIKey != "new-key" {
		t.Fatalf("persisted cmdcode accounts = %+v, want updated key", accounts)
	}
}

func TestLoadConfigMigratesLegacySingleKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yamlData := "api_key: ccgw-local\ncommandcode:\n  api_key: cc-legacy\n  base_url: https://api.commandcode.ai\n"
	if err := os.WriteFile(path, []byte(yamlData), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CommandCode.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(cfg.CommandCode.Accounts))
	}
	if cfg.CommandCode.Accounts[0].APIKey != "cc-legacy" || cfg.CommandCode.Accounts[0].Name != "default" {
		t.Fatalf("migrated account = %+v", cfg.CommandCode.Accounts[0])
	}
	if !cfg.CommandCode.Accounts[0].IsEnabled() {
		t.Fatalf("migrated account should be enabled")
	}
}

func TestSaveConfigClearsLegacyKeyWhenAccountsExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{APIKey: "ccgw-local"}
	cfg.CommandCode.APIKey = "cc-legacy"
	cfg.CommandCode.Accounts = []AccountConfig{{Name: "main", APIKey: "cc-new"}}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "cc-legacy") {
		t.Fatalf("legacy key should not be written when accounts exist:\n%s", data)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.CommandCode.Accounts) != 1 || loaded.CommandCode.Accounts[0].APIKey != "cc-new" {
		t.Fatalf("reloaded accounts = %+v", loaded.CommandCode.Accounts)
	}
}

func TestCCClientFailoverOnRateLimit(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seen = append(seen, key)
		if key == "key-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":1,\"completionTokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	client := NewCCClientWithPool(poolWithKeys("key-a", "key-b"), upstream.URL)

	resp, acct, err := client.Send(context.Background(), chatRequestForTest())
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	resp.Body.Close()
	if acct == nil || acct.APIKey != "key-b" {
		t.Fatalf("served by %v, want key-b", acct)
	}
	if len(seen) != 2 || seen[0] != "key-a" || seen[1] != "key-b" {
		t.Fatalf("upstream saw %v, want [key-a key-b]", seen)
	}
	if !client.Pool.Get(accountID("key-a")).RateLimited(time.Now()) {
		t.Fatalf("key-a should be rate limited after a 429")
	}

	// Second request must skip the cooling account entirely.
	seen = nil
	resp, acct, err = client.Send(context.Background(), chatRequestForTest())
	if err != nil {
		t.Fatalf("second Send error: %v", err)
	}
	resp.Body.Close()
	if acct == nil || acct.APIKey != "key-b" {
		t.Fatalf("second request served by %v, want key-b", acct)
	}
	if len(seen) != 1 || seen[0] != "key-b" {
		t.Fatalf("second request hit upstream %v, want [key-b]", seen)
	}
}

func TestCCClientNoFailoverOnInvalidRequest(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad messages"}`))
	}))
	defer upstream.Close()

	client := NewCCClientWithPool(poolWithKeys("key-a", "key-b"), upstream.URL)
	_, acct, err := client.Send(context.Background(), chatRequestForTest())
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) || upstreamErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 upstreamAPIError", err)
	}
	if acct == nil || acct.APIKey != "key-a" {
		t.Fatalf("account = %v, want key-a", acct)
	}
	if calls != 1 {
		t.Fatalf("upstream called %d times, want 1 (no failover on 400)", calls)
	}
}

func TestCCClientAllAccountsRateLimited(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called when every account is cooling down")
	}))
	defer upstream.Close()

	client := NewCCClientWithPool(poolWithKeys("key-a", "key-b"), upstream.URL)
	for _, key := range []string{"key-a", "key-b"} {
		client.Pool.Get(accountID(key)).RecordFailure(&upstreamAPIError{
			Status:     http.StatusTooManyRequests,
			RetryAfter: "42",
		})
	}

	_, _, err := client.Send(context.Background(), chatRequestForTest())
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("error = %v, want upstreamAPIError", err)
	}
	if upstreamErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", upstreamErr.Status)
	}
	if upstreamErr.RetryAfter != "42" {
		t.Fatalf("RetryAfter = %q, want 42 (earliest recovery)", upstreamErr.RetryAfter)
	}
}

func TestCCClientNoEnabledAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called")
	}))
	defer upstream.Close()

	pool := poolWithKeys("key-a")
	pool.SetEnabled(accountID("key-a"), false)
	client := NewCCClientWithPool(pool, upstream.URL)

	_, _, err := client.Send(context.Background(), chatRequestForTest())
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) || upstreamErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want 503 upstreamAPIError", err)
	}
}

func TestShouldFailoverMatrix(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&upstreamAPIError{Status: 401}, true},
		{&upstreamAPIError{Status: 403}, true},
		{&upstreamAPIError{Status: 429}, true},
		{&upstreamAPIError{Status: 500}, true},
		{&upstreamAPIError{Status: 503}, true},
		{&upstreamAPIError{Status: 400}, false},
		{&upstreamAPIError{Status: 404}, false},
		{&upstreamAPIError{Status: 422}, false},
		{&invalidRequestError{message: "bad"}, false},
		{fmt.Errorf("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		if got := shouldFailover(tc.err); got != tc.want {
			t.Fatalf("shouldFailover(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestUsageForAccountRecordsSeparately(t *testing.T) {
	usage := &UsageTracker{}
	acct := newAccount("main", "key-a", true)

	usage.ForAccount(acct).Record(10, 20, 0, 0)
	usage.ForAccount(acct).Record(1, 2, 3, 0)
	usage.ForAccount(nil).Record(100, 100, 0, 0)

	snap := usage.Snapshot()
	if snap.TotalRequests != 3 || snap.PromptTokens != 111 || snap.CompletionTokens != 122 {
		t.Fatalf("totals = %+v", snap)
	}
	acc := usage.AccountUsage(acct.ID)
	if acc.Requests != 2 || acc.PromptTokens != 11 || acc.CompletionTokens != 22 || acc.CacheReadTokens != 3 {
		t.Fatalf("account usage = %+v", acc)
	}

	usage.DropAccount(acct.ID)
	if got := usage.AccountUsage(acct.ID); got.Requests != 0 {
		t.Fatalf("dropped account usage = %+v", got)
	}
}

func TestUsageSnapshotPersistsAccounts(t *testing.T) {
	dir := t.TempDir()
	oldFile := usageFile
	usageFile = filepath.Join(dir, "usage.json")
	t.Cleanup(func() { usageFile = oldFile })

	usage := &UsageTracker{}
	usage.ForAccount(newAccount("main", "key-a", true)).Record(5, 6, 0, 0)
	if err := usage.save(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadUsage()
	acc := reloaded.AccountUsage(accountID("key-a"))
	if acc.Requests != 1 || acc.PromptTokens != 5 || acc.CompletionTokens != 6 {
		t.Fatalf("persisted account usage = %+v", acc)
	}
}

func TestLogRingAfterSequence(t *testing.T) {
	ring := newLogRing()
	fmt.Fprint(ring, "first line\n")
	fmt.Fprint(ring, "\x1b[31mred line\x1b[0m\n")
	fmt.Fprint(ring, "third line\n")

	entries, last := ring.After(0)
	if len(entries) != 3 || last != 3 {
		t.Fatalf("entries = %d, last = %d, want 3/3", len(entries), last)
	}
	if entries[1].Line != "red line" {
		t.Fatalf("ANSI not stripped: %q", entries[1].Line)
	}

	entries, _ = ring.After(2)
	if len(entries) != 1 || entries[0].Line != "third line" {
		t.Fatalf("After(2) = %+v", entries)
	}
}
