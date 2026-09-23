package app

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
)

// zenCLIVersion is the fallback User-Agent when the inbound caller does not
// provide one. Responses requests preserve the caller's User-Agent and the
// OpenCode identity values it sends.
const zenCLIVersion = "1.18.31"

// zenUserAgent matches the official opencode CLI.
const zenUserAgent = "opencode/" + zenCLIVersion

// zenDefaultClient is sent as x-opencode-client unless OPENCODE_CLIENT
// overrides it.
const zenDefaultClient = "cli"

// zenDefaultParent is sent as x-opencode-parent when the inbound request
// carries no parent ID (first message of a session).
const zenDefaultParent = "msg_system"

// ZenRequestIDs is the per-request CLI identity injected into every Zen
// upstream call. SessionID is shared by x-opencode-session,
// x-session-affinity and X-Session-Id so prompt-cache affinity holds.
type ZenRequestIDs struct {
	Client    string
	OrgID     string
	ProjectID string
	SessionID string
	RequestID string
	ParentID  string
	UserAgent string
}

// promptCacheKey scopes the upstream prompt cache to the session.
func (z ZenRequestIDs) promptCacheKey() string {
	return "zen:" + z.SessionID
}

// zenBase62 is the alphabet for the trailing segment of canonical IDs.
const zenBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func zenRandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
}

// CanonicalSessionID generates a session in the canonical shape
// ses_<12hex><14base62>. Since 2026-09-16 the Zen free tier rejects any
// other session shape with 403 (see .spec/issues.md §2).
func CanonicalSessionID() string {
	return "ses_" + hex.EncodeToString(zenRandomBytes(6)) + zenBase62String(14)
}

// CanonicalMessageID generates a request-scoped message ID in the same
// suffix scheme as the session: msg_<12hex><14base62>.
func CanonicalMessageID() string {
	return "msg_" + hex.EncodeToString(zenRandomBytes(6)) + zenBase62String(14)
}

// CanonicalProjectID generates a project ID in the canonical shape
// prj_<12hex>.
func CanonicalProjectID() string {
	return "prj_" + hex.EncodeToString(zenRandomBytes(6))
}

func zenBase62String(n int) string {
	raw := zenRandomBytes(n)
	out := make([]byte, n)
	for i, v := range raw {
		out[i] = zenBase62[int(v)%len(zenBase62)]
	}
	return string(out)
}

// zenClientName resolves the x-opencode-client identity: OPENCODE_CLIENT
// wins, default is cli.
func zenClientName() string {
	if v := strings.TrimSpace(os.Getenv("OPENCODE_CLIENT")); v != "" {
		return v
	}
	return zenDefaultClient
}

func firstZenHeader(inbound http.Header, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(inbound.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// DeriveZenRequestIDs builds the CLI identity for one upstream request.
// Inbound gateway headers win when present (sticky session across turns);
// otherwise fresh canonical IDs are generated for the first user message.
func DeriveZenRequestIDs(inbound http.Header) ZenRequestIDs {
	ids := ZenRequestIDs{
		Client:    zenClientName(),
		OrgID:     firstZenHeader(inbound, "x-opencode-org-id"),
		ProjectID: firstZenHeader(inbound, "x-opencode-project"),
		SessionID: firstZenHeader(inbound,
			"x-opencode-session", "x-session-affinity", "X-Session-Id", "conversation-id"),
		RequestID: firstZenHeader(inbound, "x-opencode-request", "x-request-id"),
		ParentID:  firstZenHeader(inbound, "x-opencode-parent"),
		UserAgent: firstZenHeader(inbound, "User-Agent"),
	}
	if ids.ProjectID == "" {
		ids.ProjectID = CanonicalProjectID()
	}
	if ids.SessionID == "" {
		ids.SessionID = CanonicalSessionID()
	}
	if ids.RequestID == "" {
		ids.RequestID = CanonicalMessageID()
	}
	if ids.ParentID == "" {
		ids.ParentID = zenDefaultParent
	}
	return ids
}

// setZenHeaders applies the headers the official opencode CLI sends. Every
// Zen upstream call goes through it so the client identity stays identical
// across chat and responses paths.
func setZenHeaders(h http.Header, apiKey string, ids ZenRequestIDs) {
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("x-opencode-client", ids.Client)
	if ids.OrgID != "" {
		h.Set("x-opencode-org-id", ids.OrgID)
	}
	h.Set("x-opencode-project", ids.ProjectID)
	h.Set("x-opencode-session", ids.SessionID)
	h.Set("x-opencode-request", ids.RequestID)
	h.Set("x-opencode-parent", ids.ParentID)
	h.Set("x-session-affinity", ids.SessionID)
	h.Set("X-Session-Id", ids.SessionID)
	h.Set("prompt_cache_key", ids.promptCacheKey())
	userAgent := ids.UserAgent
	if userAgent == "" {
		userAgent = zenUserAgent
	}
	h.Set("User-Agent", userAgent)
}

// setZenResponsesHeaders emits the identity headers observed on Zen Responses
// requests. Chat-only affinity aliases and the cache key header are omitted;
// Responses carries prompt_cache_key in its JSON body.
func setZenResponsesHeaders(h http.Header, apiKey string, ids ZenRequestIDs) {
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("Content-Type", "application/json")
	h.Set("x-opencode-client", ids.Client)
	if ids.OrgID != "" {
		h.Set("x-opencode-org-id", ids.OrgID)
	}
	h.Set("x-opencode-project", ids.ProjectID)
	h.Set("x-opencode-request", ids.RequestID)
	h.Set("x-opencode-session", ids.SessionID)
	userAgent := ids.UserAgent
	if userAgent == "" {
		userAgent = zenUserAgent
	}
	h.Set("User-Agent", userAgent)
}
