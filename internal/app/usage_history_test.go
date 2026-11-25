package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pinUsageClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	cur := at
	old := usageNow
	usageNow = func() time.Time { return cur }
	t.Cleanup(func() { usageNow = old })
	return &cur
}

func TestRequestRecorderBucketsByDayKeyAndModel(t *testing.T) {
	now := pinUsageClock(t, time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC))
	u := &UsageTracker{}
	acct := &Account{ID: "a1"}

	u.RequestRecorder(GatewayCmdcode, acct, "kHermes", "cmdcode/x").Record(100, 10, 40, 0)
	u.RequestRecorder(GatewayCmdcode, acct, "kHermes", "cmdcode/x").Record(50, 5, 0, 0)
	u.RequestRecorder(GatewayCmdcode, acct, "kN8n", "cmdcode/y").Record(7, 1, 0, 0)
	*now = now.Add(24 * time.Hour)
	u.RequestRecorder(GatewayOpencode, nil, "kN8n", "opencode/z").Record(3, 2, 0, 0)

	// Lifetime counters keep working as before.
	if got := u.ClientKeyUsage("kHermes"); got.Requests != 2 || got.PromptTokens != 150 {
		t.Fatalf("hermes lifetime = %+v", got)
	}
	if got := u.AccountUsageFor(GatewayCmdcode, "a1"); got.Requests != 3 {
		t.Fatalf("account lifetime = %+v", got)
	}

	rows, total := u.QueryHistory("2026-11-20", "2026-11-20", usageHistoryGroup{Key: true})
	if len(rows) != 2 || rows[0].ClientKeyID != "kHermes" || rows[0].PromptTokens != 150 || rows[0].CacheReadTokens != 40 {
		t.Fatalf("day 1 rows = %+v", rows)
	}
	if total.Requests != 3 || total.PromptTokens != 157 {
		t.Fatalf("day 1 total = %+v", total)
	}

	rows, _ = u.QueryHistory("2026-11-01", "2026-11-30", usageHistoryGroup{Day: true, Key: true, Gateway: true, Model: true})
	if len(rows) != 3 {
		t.Fatalf("all rows = %+v", rows)
	}
	last := rows[2]
	if last.Day != "2026-11-21" || last.Gateway != GatewayZen || last.Model != "opencode/z" || last.ClientKeyID != "kN8n" {
		t.Fatalf("zen row = %+v (opencode gateway must normalize to zen)", last)
	}
}

func TestPlainRecordersDoNotTouchHistory(t *testing.T) {
	pinUsageClock(t, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	u := &UsageTracker{}
	u.RecorderForGateway(GatewayCmdcode, &Account{ID: "a"}, "k").Record(1, 1, 0, 0)
	u.Record(1, 1, 0, 0)
	if rows, _ := u.QueryHistory("0000-01-01", "9999-12-31", usageHistoryGroup{Key: true}); len(rows) != 0 {
		t.Fatalf("history = %+v, want empty", rows)
	}
}

func TestUsageHistoryPersistsButStaysOutOfPublicSnapshot(t *testing.T) {
	pinUsageClock(t, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	old := usageFile
	usageFile = filepath.Join(t.TempDir(), "usage.json")
	t.Cleanup(func() { usageFile = old })

	u := &UsageTracker{}
	u.RequestRecorder(GatewayCmdcode, nil, "k1", "cmdcode/m").Record(10, 2, 0, 0)
	u.RequestRecorder(GatewayCmdcode, nil, "", "cmdcode/m").Record(1, 1, 0, 0)
	if err := u.save(); err != nil {
		t.Fatal(err)
	}

	pub, _ := json.Marshal(u.Snapshot())
	if strings.Contains(string(pub), "history") || strings.Contains(string(pub), "cmdcode/m") {
		t.Fatalf("public /usage snapshot leaks history: %s", pub)
	}

	loaded := loadUsage()
	rows, total := loaded.QueryHistory("2026-11-20", "2026-11-20", usageHistoryGroup{Key: true, Model: true})
	if len(rows) != 2 || total.PromptTokens != 11 {
		t.Fatalf("reloaded rows = %+v total=%+v", rows, total)
	}
	if rows[1].ClientKeyID != noClientKey {
		t.Fatalf("keyless usage should load as %q, got %+v", noClientKey, rows[1])
	}
}

func TestUsageHistoryRetentionPrunes(t *testing.T) {
	now := pinUsageClock(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	u := &UsageTracker{}
	u.RequestRecorder(GatewayCmdcode, nil, "k", "m").Record(1, 0, 0, 0)
	*now = now.AddDate(0, 0, usageHistoryRetentionDays)
	u.RequestRecorder(GatewayCmdcode, nil, "k", "m").Record(1, 0, 0, 0)
	if _, kept := u.historySnapshot()["2026-01-01"]; !kept {
		t.Fatal("day exactly at the retention edge was pruned")
	}
	*now = now.AddDate(0, 0, 1)
	u.RequestRecorder(GatewayCmdcode, nil, "k", "m").Record(1, 0, 0, 0)
	hist := u.historySnapshot()
	if _, kept := hist["2026-01-01"]; kept {
		t.Fatal("day past retention was not pruned")
	}
	if len(hist) != 2 {
		t.Fatalf("history days = %d, want 2", len(hist))
	}

	// Loading an old file prunes too.
	u2 := &UsageTracker{}
	u2.accMu.Lock()
	u2.restoreHistoryLocked(map[string]map[string]UsageSnapshotEntry{
		"2020-01-01":   {"k|cmdcode|m": {Requests: 1}},
		"bad-day":      {"k|cmdcode|m": {Requests: 1}},
		usageDay(*now): {"k|cmdcode|m": {Requests: 2}},
	})
	u2.accMu.Unlock()
	if hist := u2.historySnapshot(); len(hist) != 1 {
		t.Fatalf("restored history = %v, want only today", hist)
	}
}

func TestBillingPeriodFirstDay(t *testing.T) {
	cases := map[string]string{
		"2026-10-21T23:38:55Z": "2026-10-22", // late start: that day belongs to the old period
		"2026-10-21T05:00:00Z": "2026-10-21",
		"2026-10-21T11:59:00Z": "2026-10-21",
	}
	for in, want := range cases {
		start, _ := time.Parse(time.RFC3339, in)
		if got := billingPeriodFirstDay(start); got != want {
			t.Errorf("billingPeriodFirstDay(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestAdminUsageHistoryEndpoint(t *testing.T) {
	srv, pool, keys, _, usage, _ := newAdminTestEnv(t)
	now := pinUsageClock(t, time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))

	hermes, err := keys.Add("hermes-agent", "ccgw-hermes-test-key", true)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := pool.Add("main", "cc-key-for-history", true)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)
	usage.SetQuota(acct.ID, &QuotaSnapshot{Plan: &QuotaPlan{CurrentPeriodStart: &start}})

	*now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC) // previous period
	usage.RequestRecorder(GatewayCmdcode, acct, hermes.ID, "cmdcode/a").Record(1000, 1, 0, 0)
	*now = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	usage.RequestRecorder(GatewayCmdcode, acct, hermes.ID, "cmdcode/a").Record(100, 1, 0, 0)
	usage.RequestRecorder(GatewayCmdcode, acct, "kgone", "cmdcode/b").Record(10, 1, 0, 0)
	*now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	resp, payload := adminRequest(t, srv, "GET", "/admin/api/usage/history?period=billing", "admin-pass-123", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, payload)
	}
	if payload["from"] != "2026-11-20" || payload["to"] != "2026-11-21" || payload["period"] != "billing" {
		t.Fatalf("window = %v..%v (%v)", payload["from"], payload["to"], payload["period"])
	}
	rows := payload["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	first := rows[0].(map[string]any)
	if first["key_name"] != "hermes-agent" || first["prompt_tokens"].(float64) != 100 {
		t.Fatalf("first row = %v", first)
	}
	if rows[1].(map[string]any)["key_name"] != "(deleted key)" {
		t.Fatalf("second row = %v", rows[1])
	}

	_, payload = adminRequest(t, srv, "GET", "/admin/api/usage/history?days=7&group=day,model", "admin-pass-123", nil)
	if payload["from"] != "2026-11-15" || len(payload["rows"].([]any)) != 3 {
		t.Fatalf("days=7 payload = %v", payload)
	}
	if _, hasKey := payload["rows"].([]any)[0].(map[string]any)["key_name"]; hasKey {
		t.Fatal("key_name present although key is not grouped")
	}

	_, payload = adminRequest(t, srv, "GET", "/admin/api/usage/history?from=2026-11-19&to=2026-11-19", "admin-pass-123", nil)
	if total := payload["total"].(map[string]any); total["prompt_tokens"].(float64) != 1000 {
		t.Fatalf("explicit range total = %v", total)
	}

	for _, bad := range []string{"?group=bogus", "?period=month", "?from=21/11/2026", "?days=0", "?days=999"} {
		resp, _ := adminRequest(t, srv, "GET", "/admin/api/usage/history"+bad, "admin-pass-123", nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, resp.StatusCode)
		}
	}
}

func TestAdminUsageHistoryBillingWithoutQuota(t *testing.T) {
	srv, _, _, _, _, _ := newAdminTestEnv(t)
	resp, _ := adminRequest(t, srv, "GET", "/admin/api/usage/history?period=billing", "admin-pass-123", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
}

func TestOpenAIUsageSniffer(t *testing.T) {
	cases := []struct {
		name                 string
		chunks               []string
		prompt, compl, cache int
	}{
		{
			name: "chat SSE with usage on the final chunk, split mid-line",
			chunks: []string{
				"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"compl",
				"etion_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":100}}}\n\n",
				"data: [DONE]\n\n",
			},
			prompt: 120, compl: 8, cache: 100,
		},
		{
			name:   "non-stream pretty JSON",
			chunks: []string{"{\n  \"id\": \"x\",\n  \"usage\": {\n    \"prompt_tokens\": 9,\n", "    \"completion_tokens\": 3\n  }\n}"},
			prompt: 9, compl: 3,
		},
		{
			name:   "responses API completed event",
			chunks: []string{"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":50,\"output_tokens\":4,\"input_tokens_details\":{\"cached_tokens\":20}}}}"},
			prompt: 50, compl: 4, cache: 20,
		},
		{
			name:   "no usage",
			chunks: []string{"data: {\"choices\":[]}\n\ndata: [DONE]\n\n"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s openAIUsageSniffer
			for _, c := range tc.chunks {
				s.Write([]byte(c))
			}
			p, c, cr := s.Usage()
			if p != tc.prompt || c != tc.compl || cr != tc.cache {
				t.Fatalf("usage = %d/%d/%d, want %d/%d/%d", p, c, cr, tc.prompt, tc.compl, tc.cache)
			}
		})
	}
}

func TestZenRelayRecordsSniffedUsageAndKeepsBytes(t *testing.T) {
	pinUsageClock(t, time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	upstream := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	u := &UsageTracker{}
	w := httptest.NewRecorder()
	handleOpenAIResponse(w, upstream, u.RequestRecorder(GatewayOpencode, &Account{ID: "z"}, "kN8n", "opencode/m"))

	if w.Body.String() != body {
		t.Fatalf("relayed body changed:\n%q", w.Body.String())
	}
	if got := u.AccountUsageFor(GatewayZen, "z"); got.PromptTokens != 30 || got.CompletionTokens != 2 {
		t.Fatalf("zen account usage = %+v", got)
	}
	rows, _ := u.QueryHistory("2026-11-21", "2026-11-21", usageHistoryGroup{Key: true, Gateway: true})
	if len(rows) != 1 || rows[0].Gateway != GatewayZen || rows[0].PromptTokens != 30 {
		t.Fatalf("zen history = %+v", rows)
	}
}

func TestHistoryTracksUpstreamAccount(t *testing.T) {
	pinUsageClock(t, time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC))
	u := &UsageTracker{}
	main, second := &Account{ID: "aMain"}, &Account{ID: "aSecond"}

	u.RequestRecorder(GatewayCmdcode, main, "kHermes", "cmdcode/x").Record(100, 1, 0, 0)
	u.RequestRecorder(GatewayCmdcode, second, "kHermes", "cmdcode/x").Record(30, 1, 0, 0)
	u.RequestRecorder(GatewayCmdcode, second, "kN8n", "cmdcode/y").Record(5, 1, 0, 0)
	// A bucket persisted before the account dimension existed (3-part key).
	u.accMu.Lock()
	u.history["2026-11-25"]["kOld|cmdcode|cmdcode/z"] = &UsageCounters{}
	u.history["2026-11-25"]["kOld|cmdcode|cmdcode/z"].add(7, 0, 0, 0)
	u.accMu.Unlock()

	rows, total := u.QueryHistory("2026-11-25", "2026-11-25", usageHistoryGroup{Account: true})
	got := map[string]int64{}
	for _, r := range rows {
		got[r.AccountID] = r.PromptTokens
	}
	if len(rows) != 3 || got["aMain"] != 100 || got["aSecond"] != 35 || got[unknownHistoryAccount] != 7 {
		t.Fatalf("by account = %+v", rows)
	}
	if total.PromptTokens != 142 {
		t.Fatalf("total = %+v", total)
	}

	// Without the account group, legacy and new buckets of one key still merge.
	rows, _ = u.QueryHistory("2026-11-25", "2026-11-25", usageHistoryGroup{Key: true})
	if len(rows) != 3 || rows[0].ClientKeyID != "kHermes" || rows[0].PromptTokens != 130 || rows[0].AccountID != "" {
		t.Fatalf("by key = %+v", rows)
	}

	names := accountDisplayName([]*AccountPool{nil}, unknownHistoryAccount)
	if names != "(unknown account)" {
		t.Fatalf("unknown name = %q", names)
	}
}
