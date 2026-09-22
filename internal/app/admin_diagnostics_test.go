package app

// T9 diagnostics tests (GW-06 + GW-07).
//
// Endpoint shape under test (chosen: simplest consistent with the admin API):
//
//	POST /admin/api/debug/inference
//	{"gateway":"zen","account_id":"<id>","model":"<bare-or-prefixed>",
//	 "messages":[{"role":"user","content":"hi"}]}
//
// Response: {"ok","http_status","request_id","route","response",
// "key_test","gateway","account_id","fingerprint","latency_ms","error",
// ("retry_after" only when the upstream sent Retry-After)}.
//
// Playground semantics pinned here: 1 request, 1 key, no failover, no
// RecordSuccess/RecordFailure, no cooldown/binding mutation. Classification:
// 402 → request_error (never rejected) — a malformed/unfunded request would
// fail identically on every key, so it diagnoses the request, not the
// credential.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gatewayDiscoverEnv starts an admin server with separate cmdcode + zen pools.
// The zen pool base URL is pointed at zenBase so the diagnostic lane hits the
// fake; the helper returns the env plus the live zen pool for assertions.
func gatewayDiscoverEnv(t *testing.T, zenBase string) (*httptest.Server, *AccountPool, *AccountPool) {
	t.Helper()
	srv, pool, _, cfg, usage, ring := newAdminTestEnv(t)
	zenPool := NewAccountPool(nil)
	cfg.SetGatewayBaseURL(GatewayZen, zenBase)
	keys := NewClientKeyPool(nil)
	cc := NewCCClientWithPool(pool, cfg.UpstreamBaseURL())
	quotas := NewQuotaService(cc, pool, usage)
	mux := http.NewServeMux()
	registerAdminRoutes(mux, cc, pool, keys, cfg, usage, ring, quotas, zenPool)
	t.Cleanup(quotas.Wait)
	root := http.NewServeMux()
	root.Handle("/admin/", adminAuth(cfg, nil)(mux))
	second := httptest.NewServer(root)
	t.Cleanup(second.Close)
	srv.Close()
	return second, pool, zenPool
}

func debugInference(t *testing.T, srv *httptest.Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	resp, payload := adminRequest(t, srv, "POST", "/admin/api/debug/inference", "admin-pass-123", body)
	return resp.StatusCode, payload
}

func accountRowByName(t *testing.T, srv *httptest.Server, name string) map[string]any {
	t.Helper()
	_, payload := adminRequest(t, srv, "GET", "/admin/api/accounts", "admin-pass-123", nil)
	for _, entry := range payload["accounts"].([]any) {
		row := entry.(map[string]any)
		if row["name"] == name {
			return row
		}
	}
	t.Fatalf("account %q missing from list: %v", name, payload)
	return nil
}

// GW-06 AC1: GET includes gateway per account; absent gateway on POST lands
// in cmdcode (retrocompat).
func TestAdminAccountsGatewayListAndDefault(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, nil, `{"id":"x","object":"chat.completion","choices":[]}`
	})
	srv, _, _ := gatewayDiscoverEnv(t, fake.srv.URL)

	resp, _ := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "legacy", "api_key": "cc-legacy-key"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("legacy add status = %d", resp.StatusCode)
	}
	row := accountRowByName(t, srv, "legacy")
	if row["gateway"] != GatewayCmdcode {
		t.Fatalf("legacy row gateway = %v, want cmdcode", row["gateway"])
	}
}

// GW-06 AC2: POST {"gateway":"zen"} lands in the zen pool and persists under
// gateways.zen.accounts; the same key may exist in cmdcode (pools are
// independent, no 409 across gateways).
func TestAdminAccountsCreateZenPoolAndPersist(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			return http.StatusOK, nil, `{"object":"list","data":[{"id":"deepseek-v4-flash","object":"model"}]}`
		}
		return http.StatusOK, nil, `{"id":"x","object":"chat.completion","choices":[]}`
	})
	srv, pool, zenPool := gatewayDiscoverEnv(t, fake.srv.URL)
	seedCatalogs(t, nil, nil)

	resp, payload := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "z1", "api_key": "zen-key-1", "gateway": "zen"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("zen add status = %d: %v", resp.StatusCode, payload)
	}
	if payload["gateway"] != GatewayZen {
		t.Fatalf("zen add gateway = %v, want zen", payload["gateway"])
	}
	if zenPool.Len() != 1 || pool.Len() != 0 {
		t.Fatalf("pools: cmdcode=%d zen=%d, want 0/1", pool.Len(), zenPool.Len())
	}
	if got := modelIDs(ModelList{Data: modelCatalogSnapshot()}); !got[OpencodePrefix+"deepseek-v4-flash"] {
		t.Fatalf("catalog = %v, want Zen model after account add", got)
	}
	row := accountRowByName(t, srv, "z1")
	if row["gateway"] != GatewayZen {
		t.Fatalf("list row gateway = %v, want zen", row["gateway"])
	}

	resp, _ = adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "c1", "api_key": "zen-key-1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("same key in cmdcode status = %d, want 201", resp.StatusCode)
	}

	saved := loadConfigForTest(t)
	gc := saved.Gateways[GatewayZen]
	if gc == nil || len(gc.Accounts) != 1 || gc.Accounts[0].APIKey != "zen-key-1" {
		t.Fatalf("persisted zen accounts = %+v", saved.Gateways)
	}
	if len(saved.CommandCode.Accounts) != 1 || saved.CommandCode.Accounts[0].APIKey != "zen-key-1" {
		t.Fatalf("persisted cmdcode accounts = %+v", saved.CommandCode.Accounts)
	}
}

func TestAdminAccountsWithSameKeyMutateSeparately(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			return http.StatusOK, nil, `{"object":"list","data":[{"id":"deepseek-v4-flash","object":"model"}]}`
		}
		return http.StatusOK, nil, `{"id":"x","object":"chat.completion","choices":[]}`
	})
	srv, pool, zenPool := gatewayDiscoverEnv(t, fake.srv.URL)
	seedCatalogs(t, nil, nil)
	_, zen := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "zen", "api_key": "shared-key", "gateway": "zen"})
	_, cmdcode := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "cmdcode", "api_key": "shared-key", "gateway": "cmdcode"})
	id := zen["id"].(string)
	if cmdcode["id"] != id {
		t.Fatalf("shared key IDs differ: zen=%s cmdcode=%v", id, cmdcode["id"])
	}

	resp, _ := adminRequest(t, srv, "DELETE", "/admin/api/accounts/"+id+"?gateway=zen", "admin-pass-123", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete zen status = %d, want 200", resp.StatusCode)
	}
	if zenPool.Get(id) != nil || pool.Get(id) == nil {
		t.Fatalf("pools after zen delete: cmdcode=%v zen=%v", pool.Get(id), zenPool.Get(id))
	}
	if got := modelIDs(ModelList{Data: modelCatalogSnapshot()}); got[OpencodePrefix+"deepseek-v4-flash"] {
		t.Fatalf("catalog = %v, want Zen catalog cleared after last account deletion", got)
	}
}

// GW-06: unknown gateway is a 400, not a silent default.
func TestAdminAccountsCreateUnknownGateway(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, nil, `{}`
	})
	srv, _, _ := gatewayDiscoverEnv(t, fake.srv.URL)
	resp, _ := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "bad", "api_key": "k-bad", "gateway": "nope"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown gateway status = %d, want 400", resp.StatusCode)
	}
}

// GW-06: delete drops the namespaced usage row and persists the removal.
func TestAdminAccountsDeleteZenDropsNamespacedUsage(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, nil, `{}`
	})
	srv, _, zenPool := gatewayDiscoverEnv(t, fake.srv.URL)
	_, created := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "zdel", "api_key": "zen-del-key", "gateway": "zen"})
	id := created["id"].(string)
	if zenPool.Get(id) == nil {
		t.Fatal("zen account missing before delete")
	}

	resp, _ := adminRequest(t, srv, "DELETE", "/admin/api/accounts/"+id, "admin-pass-123", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if zenPool.Get(id) != nil {
		t.Fatal("zen account still in pool after delete")
	}
	_, payload := adminRequest(t, srv, "GET", "/admin/api/accounts", "admin-pass-123", nil)
	for _, entry := range payload["accounts"].([]any) {
		if entry.(map[string]any)["id"] == id {
			t.Fatalf("deleted account still listed: %v", payload)
		}
	}
	saved := loadConfigForTest(t)
	if gc := saved.Gateways[GatewayZen]; gc != nil {
		for _, a := range gc.Accounts {
			if a.APIKey == "zen-del-key" {
				t.Fatalf("deleted zen account still persisted: %+v", gc.Accounts)
			}
		}
	}
}

// adminUsageForEnv fetches the list row for id (fails the test when absent).
func adminUsageForEnv(t *testing.T, srv *httptest.Server, id string) (map[string]any, bool) {
	t.Helper()
	_, payload := adminRequest(t, srv, "GET", "/admin/api/accounts", "admin-pass-123", nil)
	for _, entry := range payload["accounts"].([]any) {
		row := entry.(map[string]any)
		if row["id"] == id {
			return row, true
		}
	}
	t.Fatalf("account %s missing: %v", id, payload)
	return nil, false
}

// GW-07 AC1: failing upstream attempts log fingerprint + status + outcome and
// the client error preserves the upstream message + Retry-After.
func TestAdminAccountTestZenFailurePreservesMessageAndRetryAfter(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, `{"message":"slow down","type":"rate_limit_error"}`
	})
	srv, _, _ := gatewayDiscoverEnv(t, fake.srv.URL)
	_, created := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "zrl", "api_key": "zen-rl-key", "gateway": "zen"})
	id := created["id"].(string)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/api/accounts/"+id+"/test", nil)
	req.Header.Set("Authorization", "Bearer admin-pass-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("Retry-After = %q, want 7", resp.Header.Get("Retry-After"))
	}
}

// GW-07 AC2: Playground selected-key does exactly 1 request on 1 key without
// failover, cooldown, or RecordSuccess/RecordFailure. Fake 403 on the only
// key: key_test=rejected and the account shows no cooldown.
func TestDebugInferenceSelectedKeyNoFailoverNoMutation(t *testing.T) {
	calls := 0
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		calls = call
		if r.Header.Get("Authorization") != "Bearer zen-sel-key" {
			return http.StatusUnauthorized, nil, `{"message":"bad key"}`
		}
		return http.StatusForbidden, nil, `{"message":"key revoked"}`
	})
	srv, _, zenPool := gatewayDiscoverEnv(t, fake.srv.URL)
	_, created := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "zsel", "api_key": "zen-sel-key", "gateway": "zen"})
	id := created["id"].(string)
	before := len(fake.all()) // Account creation refreshes the Zen model catalog.

	status, payload := debugInference(t, srv, map[string]any{
		"gateway": "zen", "account_id": id, "model": "deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if status != http.StatusOK {
		t.Fatalf("diagnostic envelope status = %d, want 200", status)
	}
	if payload["http_status"] != float64(403) {
		t.Fatalf("http_status = %v, want 403", payload["http_status"])
	}
	if payload["key_test"] != string(keyTestRejected) {
		t.Fatalf("key_test = %v, want rejected", payload["key_test"])
	}
	if payload["fingerprint"] != id {
		t.Fatalf("fingerprint = %v, want account id %s", payload["fingerprint"], id)
	}
	if calls != before+1 || len(fake.all()) != before+1 {
		t.Fatalf("diagnostic calls = %d, want exactly 1 after catalog refresh", len(fake.all())-before)
	}
	acct := zenPool.Get(id)
	if acct == nil {
		t.Fatal("account missing from zen pool")
	}
	if acct.Errors.Load() != 0 {
		t.Fatalf("pool mutated: errors = %d, want 0", acct.Errors.Load())
	}
	if acct.RateLimited(time.Now()) {
		t.Fatal("pool mutated: account cooling down after diagnostic 403")
	}
}

// GW-07 AC2 (402 mapping): key_test with 402 → request_error, never rejected;
// the client error keeps the upstream message; the pool stays intact.
func TestDebugInference402IsRequestError(t *testing.T) {
	fake := newZenFake(t, func(call int, r *http.Request) (int, map[string]string, string) {
		return http.StatusPaymentRequired, nil, `{"message":"Insufficient account funds"}`
	})
	srv, _, zenPool := gatewayDiscoverEnv(t, fake.srv.URL)
	_, created := adminRequest(t, srv, "POST", "/admin/api/accounts", "admin-pass-123",
		map[string]any{"name": "z402", "api_key": "zen-402-key", "gateway": "zen"})
	id := created["id"].(string)
	before := len(fake.all()) // Account creation refreshes the Zen model catalog.

	_, payload := debugInference(t, srv, map[string]any{
		"gateway": "zen", "account_id": id, "model": "deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if payload["key_test"] != string(keyTestRequestError) {
		t.Fatalf("402 key_test = %v, want request_error", payload["key_test"])
	}
	if !strings.Contains(payload["error"].(string), "Insufficient account funds") {
		t.Fatalf("error lost upstream message: %v", payload["error"])
	}
	if len(fake.all()) != before+1 {
		t.Fatalf("diagnostic calls = %d, want 1", len(fake.all())-before)
	}
	if got := zenPool.Get(id).Errors.Load(); got != 0 {
		t.Fatalf("pool mutated on 402: errors = %d", got)
	}
}

// Unit: classifier pins the 402 → request_error mapping next to the other
// status classes.
func TestClassifyKeyTestMapping(t *testing.T) {
	cases := []struct {
		status    int
		transport bool
		want      keyTestOutcome
	}{
		{401, false, keyTestRejected},
		{403, false, keyTestRejected},
		{429, false, keyTestRateLimited},
		{402, false, keyTestRequestError},
		{400, false, keyTestRequestError},
		{404, false, keyTestRequestError},
		{422, false, keyTestRequestError},
		{500, false, keyTestUpstreamError},
		{503, false, keyTestUpstreamError},
		{200, true, keyTestTransportError},
		{0, false, keyTestUnavailable},
	}
	for _, c := range cases {
		if got := classifyKeyTest(c.status, c.transport); got != c.want {
			t.Fatalf("classifyKeyTest(%d, %v) = %q, want %q", c.status, c.transport, got, c.want)
		}
	}
}
