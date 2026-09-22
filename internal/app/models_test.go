package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func seedCatalogs(t *testing.T, cmdcode, opencode []ModelInfo) {
	t.Helper()
	oldBuckets := modelCatalogs
	oldUnified := modelCatalog
	modelCatalogs = map[string][]ModelInfo{}
	// Pass copies so seeds never share backing arrays with the buckets.
	if cmdcode == nil {
		setGatewayCatalog(GatewayCmdcode, nil)
	} else {
		setGatewayCatalog(GatewayCmdcode, append([]ModelInfo(nil), cmdcode...))
	}
	if opencode == nil {
		setGatewayCatalog(GatewayOpencode, nil)
	} else {
		setGatewayCatalog(GatewayOpencode, append([]ModelInfo(nil), opencode...))
	}
	t.Cleanup(func() {
		modelCatalogs = oldBuckets
		modelCatalog = oldUnified
	})
}

func decodeModels(t *testing.T, rec *httptest.ResponseRecorder) ModelList {
	t.Helper()
	var resp ModelList
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func modelIDs(resp ModelList) map[string]bool {
	out := map[string]bool{}
	for _, m := range resp.Data {
		out[m.ID] = true
	}
	return out
}

// GW-05 AC1: unified catalog carries both prefixes; exclude_models matches
// the suffix after "/" (gpt- removes opencode/gpt-5.5, keeps the rest).
func TestUnifiedModelsPrefixesAndExcludeSuffix(t *testing.T) {
	seedCatalogs(t,
		[]ModelInfo{{ID: "deepseek-v4"}},
		[]ModelInfo{{ID: "gpt-5.5"}, {ID: "kimi-k2"}},
	)
	cfg := &Config{ExcludeModels: []string{"gpt-"}}
	handler := handleModels(cfg)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := modelIDs(decodeModels(t, rec))
	for _, want := range []string{CmdcodePrefix + "deepseek-v4", OpencodePrefix + "kimi-k2"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	if got[OpencodePrefix+"gpt-5.5"] {
		t.Fatalf("opencode/gpt-5.5 must be excluded by suffix gpt-, got %v", got)
	}
}

// GW-05 AC2: a gateway without accounts contributes empty without failing
// the other gateway's listing.
func TestUnifiedModelsEmptyGateway(t *testing.T) {
	seedCatalogs(t, []ModelInfo{{ID: "deepseek-v4"}}, nil)
	rec := httptest.NewRecorder()
	handleModels(&Config{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	got := modelIDs(decodeModels(t, rec))
	if len(got) != 1 || !got[CmdcodePrefix+"deepseek-v4"] {
		t.Fatalf("unified = %v, want only cmdcode/deepseek-v4", got)
	}
}

// Per-gateway buckets never overwrite each other: refetching cmdcode keeps
// the opencode bucket intact.
func TestGatewayBucketsAreIndependent(t *testing.T) {
	seedCatalogs(t, []ModelInfo{{ID: "a"}}, []ModelInfo{{ID: "b"}})
	setGatewayCatalog(GatewayCmdcode, []ModelInfo{{ID: "a2"}})
	got := modelIDs(ModelList{Data: modelCatalog})
	for _, want := range []string{CmdcodePrefix + "a2", OpencodePrefix + "b"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	if got[CmdcodePrefix+"a"] {
		t.Fatalf("stale cmdcode/a leaked: %v", got)
	}
}

// Boot fetch failure degrades to [WARN] with the catalog left alone.
func TestFetchProviderModelsFailureKeepsCatalog(t *testing.T) {
	seedCatalogs(t, []ModelInfo{{ID: "keep"}}, []ModelInfo{{ID: "zen-keep"}})
	FetchProviderModels("http://127.0.0.1:1", "k")
	got := modelIDs(ModelList{Data: modelCatalog})
	if !got[CmdcodePrefix+"keep"] || !got[OpencodePrefix+"zen-keep"] {
		t.Fatalf("failed fetch must keep the catalog, got %v", got)
	}
}

// Zen FetchModels hits GET {base}/v1/models with Bearer Primary + CLI
// headers; without accounts it contributes empty.
func TestZenFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer zen-key-a" {
			t.Errorf("auth = %q, want Bearer zen-key-a", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-opencode-client") != "cli" || r.Header.Get("User-Agent") != zenUserAgent {
			t.Errorf("missing CLI headers: %v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{"id": "gpt-5.5", "object": "model", "owned_by": "opencode"},
				map[string]any{"id": "kimi-k2", "object": "model", "owned_by": "opencode"},
			},
		})
	}))
	defer srv.Close()

	client := NewZenClientWithPool(poolWithKeys("zen-key-a"), srv.URL)
	got := client.FetchModels()
	if len(got) != 2 || got[0].ID != "gpt-5.5" || got[1].ID != "kimi-k2" {
		t.Fatalf("FetchModels = %+v, want [gpt-5.5 kimi-k2]", got)
	}

	empty := NewZenClientWithPool(NewAccountPool(nil), srv.URL)
	if models := empty.FetchModels(); len(models) != 0 {
		t.Fatalf("no-accounts FetchModels = %+v, want empty", models)
	}

	bad := NewZenClientWithPool(poolWithKeys("zen-key-a"), "http://127.0.0.1:1")
	if models := bad.FetchModels(); len(models) != 0 {
		t.Fatalf("failed FetchModels = %+v, want empty", models)
	}
}

func TestIsModelExcludedPrefixMatch(t *testing.T) {
	cases := []struct {
		model    string
		excludes []string
		want     bool
	}{
		{"gpt-4", []string{"gpt-"}, true},
		{"claude-3-sonnet", []string{"claude-"}, true},
		{"gemini-pro", []string{"gpt-"}, false},
		{"deepseek-chat", []string{"gpt-", "claude-", "gemini-"}, false},
	}
	for _, c := range cases {
		got := isModelExcluded(c.model, c.excludes)
		if got != c.want {
			t.Fatalf("isModelExcluded(%q, %v) = %v, want %v", c.model, c.excludes, got, c.want)
		}
	}
}

func TestIsModelExcludedProviderQualified(t *testing.T) {
	cases := []struct {
		model    string
		excludes []string
		want     bool
	}{
		{"openai/gpt-4", []string{"gpt-"}, true},
		{"anthropic/claude-3", []string{"claude-"}, true},
		{"google/gemini-1.5-pro", []string{"gemini-"}, true},
		{"deepseek/deepseek-chat", []string{"gpt-"}, false},
	}
	for _, c := range cases {
		got := isModelExcluded(c.model, c.excludes)
		if got != c.want {
			t.Fatalf("isModelExcluded(%q, %v) = %v, want %v", c.model, c.excludes, got, c.want)
		}
	}
}

func TestIsModelExcludedEmptyList(t *testing.T) {
	if isModelExcluded("gpt-4", nil) {
		t.Fatal("expected false for nil excludes")
	}
	if isModelExcluded("gpt-4", []string{}) {
		t.Fatal("expected false for empty excludes")
	}
}

func TestIsModelExcludedWhitespaceEntry(t *testing.T) {
	cases := []struct {
		model    string
		excludes []string
		want     bool
	}{
		{"gpt-4", []string{"  ", "gpt-"}, true},
		{"gpt-4", []string{"", "gpt-"}, true},
	}
	for _, c := range cases {
		got := isModelExcluded(c.model, c.excludes)
		if got != c.want {
			t.Fatalf("isModelExcluded(%q, %v) = %v, want %v", c.model, c.excludes, got, c.want)
		}
	}
}
