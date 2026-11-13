package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveSystemOneModel(t *testing.T) {
	for _, in := range []string{"", "jev", "JEV", "jev-latest", "typesafe/jev", "typesafe-ai/jev", "cmdcode/jev", "cmdcode/typesafe/jev"} {
		got, ok := resolveSystemOneModel(in)
		if !ok || got != "typesafe/jev" {
			t.Fatalf("resolveSystemOneModel(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := resolveSystemOneModel("claude-opus-5-5"); ok {
		t.Fatal("chat model must not resolve as a System One model")
	}
}

func TestHandleSystemOneRelaysToUpstream(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"model":"typesafe/jev","answers":{"done":{"type":"noul","noul":0.91}},"usage":{"input_tokens":42,"output_tokens":0}}`)
	}))
	defer upstream.Close()

	usage := &UsageTracker{}
	h := handleSystemOne(NewCCClient("test-key", upstream.URL), usage)
	body := `{"model":"jev","state":"tests pass","questions":{"done":{"type":"noul","instructions":"is the task done?"}},"extra":1}`
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != systemOneUpstreamPath || gotAuth != "Bearer test-key" {
		t.Fatalf("upstream path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["model"] != "typesafe/jev" || gotBody["extra"] != float64(1) {
		t.Fatalf("relayed body = %#v", gotBody)
	}
	if !strings.Contains(rec.Body.String(), `"noul":0.91`) {
		t.Fatalf("response not relayed: %s", rec.Body.String())
	}
	if usage.PromptTokens.Load() != 42 {
		t.Fatalf("prompt tokens = %d, want 42", usage.PromptTokens.Load())
	}
}

func TestHandleSystemOneRejectsInvalidRequests(t *testing.T) {
	cases := map[string]string{
		"not json":       `is the task done?`,
		"no state":       `{"questions":{"q":{"type":"noul","instructions":"x"}}}`,
		"no questions":   `{"state":"s","questions":{}}`,
		"bad type":       `{"state":"s","questions":{"q":{"type":"bool","instructions":"x"}}}`,
		"choice no opts": `{"state":"s","questions":{"q":{"type":"choice","instructions":"x"}}}`,
		"score one lvl":  `{"state":"s","questions":{"q":{"type":"score","instructions":"x","criteria":["low"]}}}`,
		"chat model":     `{"model":"claude-opus-5-5","state":"s","questions":{"q":{"type":"noul","instructions":"x"}}}`,
	}
	h := handleSystemOne(NewCCClient("test-key", "http://127.0.0.1:1"), &UsageTracker{})
	for name, body := range cases {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 4xx; body=%s", name, rec.Code, rec.Body.String())
		}
	}
}
