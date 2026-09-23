package app

import (
	"net/http"
	"regexp"
	"testing"
)

var (
	sesPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	msgPattern = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	prjPattern = regexp.MustCompile(`^prj_[0-9a-f]{12}$`)
)

func TestCanonicalSessionIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		got := CanonicalSessionID()
		if !sesPattern.MatchString(got) {
			t.Fatalf("CanonicalSessionID() = %q, want ses_<12hex><14base62>", got)
		}
	}
}

func TestCanonicalSessionIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := CanonicalSessionID()
		if seen[id] {
			t.Fatalf("duplicate session ID %q in 100 generations", id)
		}
		seen[id] = true
	}
}

func TestCanonicalMessageAndProjectIDFormat(t *testing.T) {
	if got := CanonicalMessageID(); !msgPattern.MatchString(got) {
		t.Fatalf("CanonicalMessageID() = %q, want msg_<12hex><14base62>", got)
	}
	if got := CanonicalProjectID(); !prjPattern.MatchString(got) {
		t.Fatalf("CanonicalProjectID() = %q, want prj_<12hex>", got)
	}
}

func TestZenClientDefault(t *testing.T) {
	t.Setenv("OPENCODE_CLIENT", "")
	if got := zenClientName(); got != "cli" {
		t.Fatalf("zenClientName() = %q, want %q", got, "cli")
	}
}

func TestZenClientEnvOverride(t *testing.T) {
	t.Setenv("OPENCODE_CLIENT", "codex")
	if got := zenClientName(); got != "codex" {
		t.Fatalf("zenClientName() = %q, want %q", got, "codex")
	}
}

func TestDeriveZenRequestIDsFresh(t *testing.T) {
	t.Setenv("OPENCODE_CLIENT", "")
	ids := DeriveZenRequestIDs(http.Header{})
	if ids.Client != "cli" {
		t.Errorf("Client = %q, want %q", ids.Client, "cli")
	}
	if !sesPattern.MatchString(ids.SessionID) {
		t.Errorf("SessionID = %q, want canonical ses_ shape", ids.SessionID)
	}
	if !msgPattern.MatchString(ids.RequestID) {
		t.Errorf("RequestID = %q, want canonical msg_ shape", ids.RequestID)
	}
	if !prjPattern.MatchString(ids.ProjectID) {
		t.Errorf("ProjectID = %q, want canonical prj_ shape", ids.ProjectID)
	}
	if ids.ParentID == "" {
		t.Errorf("ParentID is empty, want a default for the first message")
	}
}

func TestDeriveZenRequestIDsFromInboundSession(t *testing.T) {
	session := CanonicalSessionID()
	for _, header := range []string{
		"x-opencode-session", "x-session-affinity", "X-Session-Id", "conversation-id",
	} {
		inbound := http.Header{}
		inbound.Set(header, session)
		ids := DeriveZenRequestIDs(inbound)
		if ids.SessionID != session {
			t.Errorf("inbound %s: SessionID = %q, want %q", header, ids.SessionID, session)
		}
	}
}

func TestDeriveZenRequestIDsPrefersFirstHeader(t *testing.T) {
	sessA := CanonicalSessionID()
	sessB := CanonicalSessionID()
	inbound := http.Header{}
	inbound.Set("x-opencode-session", sessA)
	inbound.Set("x-session-affinity", sessB)
	if ids := DeriveZenRequestIDs(inbound); ids.SessionID != sessA {
		t.Fatalf("SessionID = %q, want %q from x-opencode-session", ids.SessionID, sessA)
	}
}

func TestDeriveZenRequestIDsPreservesProjectRequestParent(t *testing.T) {
	inbound := http.Header{}
	inbound.Set("x-opencode-project", "prj_abc123def456")
	inbound.Set("x-opencode-request", "msg_abc123def456ABCDEFGHIJKLMN")
	inbound.Set("x-opencode-parent", "msg_ffffffffffff00000000000000")
	ids := DeriveZenRequestIDs(inbound)
	if ids.ProjectID != "prj_abc123def456" {
		t.Errorf("ProjectID = %q, want inbound value preserved", ids.ProjectID)
	}
	if ids.RequestID != "msg_abc123def456ABCDEFGHIJKLMN" {
		t.Errorf("RequestID = %q, want inbound value preserved", ids.RequestID)
	}
	if ids.ParentID != "msg_ffffffffffff00000000000000" {
		t.Errorf("ParentID = %q, want inbound value preserved", ids.ParentID)
	}
}

func TestSetZenHeadersEmitsCLIIdentity(t *testing.T) {
	t.Setenv("OPENCODE_CLIENT", "")
	ids := DeriveZenRequestIDs(http.Header{})
	h := http.Header{}
	setZenHeaders(h, "zen-key", ids)

	want := map[string]string{
		"Authorization":      "Bearer zen-key",
		"x-opencode-client":  "cli",
		"x-opencode-project": ids.ProjectID,
		"x-opencode-session": ids.SessionID,
		"x-opencode-request": ids.RequestID,
		"x-opencode-parent":  ids.ParentID,
		"x-session-affinity": ids.SessionID,
		"X-Session-Id":       ids.SessionID,
		"prompt_cache_key":   "zen:" + ids.SessionID,
		"User-Agent":         "opencode/" + zenCLIVersion,
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
}

func TestSetZenHeadersAffinityMirrorsSession(t *testing.T) {
	session := CanonicalSessionID()
	inbound := http.Header{}
	inbound.Set("X-Session-Id", session)
	ids := DeriveZenRequestIDs(inbound)
	h := http.Header{}
	setZenHeaders(h, "zen-key", ids)
	for _, k := range []string{"x-opencode-session", "x-session-affinity", "X-Session-Id"} {
		if h.Get(k) != session {
			t.Errorf("%s = %q, want session %q", k, h.Get(k), session)
		}
	}
	if h.Get("prompt_cache_key") == "" {
		t.Errorf("prompt_cache_key is empty, want a session-scoped value")
	}
}

func TestSetZenHeadersClientOverridePropagates(t *testing.T) {
	t.Setenv("OPENCODE_CLIENT", "codex")
	ids := DeriveZenRequestIDs(http.Header{})
	h := http.Header{}
	setZenHeaders(h, "zen-key", ids)
	if h.Get("x-opencode-client") != "codex" {
		t.Fatalf("x-opencode-client = %q, want %q", h.Get("x-opencode-client"), "codex")
	}
}

func TestSetZenResponsesHeadersPreservesOpenCodeIdentity(t *testing.T) {
	inbound := http.Header{}
	inbound.Set("x-opencode-org-id", "wrk_example")
	inbound.Set("x-opencode-project", "project-from-caller")
	inbound.Set("x-opencode-session", "session-from-caller")
	inbound.Set("x-opencode-request", "request-from-caller")
	inbound.Set("User-Agent", "opencode/1.18.32 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14")
	ids := DeriveZenRequestIDs(inbound)
	h := http.Header{}
	setZenResponsesHeaders(h, "zen-key", ids)
	for k, want := range map[string]string{
		"x-opencode-org-id": "wrk_example", "x-opencode-project": "project-from-caller",
		"x-opencode-session": "session-from-caller", "x-opencode-request": "request-from-caller",
		"User-Agent": inbound.Get("User-Agent"), "Authorization": "Bearer zen-key",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if h.Get("x-opencode-parent") != "" || h.Get("prompt_cache_key") != "" {
		t.Errorf("Responses headers include chat-only values: %+v", h)
	}
}
