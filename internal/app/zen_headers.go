package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// zenCLIVersion is the opencode CLI version this gateway impersonates. It is
// the fallback User-Agent segment when the inbound caller does not provide
// one. Verified against a live mitmproxy capture of opencode 1.19.0 on
// 2026-10-13 (see .spec/issues.md §2).
const zenCLIVersion = "1.19.0"

// zenUserAgent matches the full User-Agent the official opencode CLI sends on
// Zen requests: the CLI version plus the ai-sdk and bun runtime segments. The
// free tier keys client identity off this string, so it is sent verbatim.
const zenUserAgent = "opencode/" + zenCLIVersion + " ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"

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

// canonicalSessionPattern matches OpenCode's canonical session format:
// "ses_" + 12 lowercase-hex time part + 14 base62 random part.
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// descendingTimePartPattern matches the time part the official opencode client
// mints with Identifier.descending(): the 48-bit value ~(ts_ms<<12 | counter)
// in hex. The bitwise NOT sets the low 12 bits, so a genuine time part ends in
// 0xFFF-counter. Live probing (opencode2api, 2026-10-13) shows the Zen free
// tier accepts counters 1-63 (low 12 bits 0xFC0-0xFFF) and rejects everything
// else with 403 FreeTierError *even when the ses_ shape is otherwise right* —
// so a random-hex time part is almost always refused. See .spec/issues.md.
var descendingTimePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

// CanonicalSessionID generates a fresh session in the canonical shape the Zen
// free tier accepts: ses_<12hex time part><14base62>, where the time part is a
// valid Identifier.descending() value (counter 1, low 12 bits "ffe"). A random
// 12-hex time part passes the tier's encoding check only ~1.5% of the time, so
// generation routes through canonicalizeSessionID from a random seed.
func CanonicalSessionID() string {
	return canonicalizeSessionID(hex.EncodeToString(zenRandomBytes(16)))
}

// canonicalizeSessionID returns signal unchanged when it is already an official
// descending() session (preserving an opencode client's upstream prompt-cache
// affinity). Any other identity — a foreign client session, a UUID, a legacy
// random-hex gateway session, or a bare conversation seed — is deterministically
// re-encoded into the exact descending() layout (counter 1 → low 12 hex "ffe")
// while staying stable per input, so it passes the free tier's encoding check
// where the raw shape was rejected with 403 FreeTierError.
func canonicalizeSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) &&
		descendingTimePartPattern.MatchString(signal[4:16]) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := descendingTimePart(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// descendingTimePart renders six bytes as the official client's
// Identifier.descending() layout: a timestamp shifted into the top of a 48-bit
// word, ORed with counter 1, then bitwise-NOT-ed so the low 12 hex digits read
// "ffe". The input bytes only vary the high bits, so the counter window the
// free tier requires is always satisfied.
func descendingTimePart(hash []byte) string {
	current := (uint64(hash[0])<<28 | uint64(hash[1])<<20 | uint64(hash[2])<<12 |
		uint64(hash[3])<<4 | uint64(hash[4])>>4) << 12
	current |= 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

// base62Fixed renders n as exactly width base62 digits, zero-padded on the
// left. The 10-byte (80-bit) random source always fits in 14 digits.
func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(int64(len(zenBase62)))
	out := make([]byte, width)
	rem := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, rem)
		out[i] = zenBase62[rem.Int64()]
	}
	return string(out)
}

// CanonicalMessageID generates a request-scoped message ID in the same
// suffix scheme as the session: msg_<12hex><14base62>.
func CanonicalMessageID() string {
	return "msg_" + hex.EncodeToString(zenRandomBytes(6)) + zenBase62String(14)
}

// CanonicalProjectID generates a project ID in the shape the opencode CLI
// sends: a bare 40-hex digest (the CLI hashes the worktree path; we have no
// path, so the value is random). The live 1.19.0 capture (2026-10-13) sends
// 40 hex with no "prj_" prefix, unlike the older static reference.
func CanonicalProjectID() string {
	return hex.EncodeToString(zenRandomBytes(20))
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
	} else {
		// A caller-supplied session only preserves affinity if the free tier
		// accepts its encoding; re-encode anything that is not already an
		// official descending() session (idempotent for ones that are).
		ids.SessionID = canonicalizeSessionID(ids.SessionID)
	}
	if ids.RequestID == "" {
		ids.RequestID = CanonicalMessageID()
	}
	if ids.ParentID == "" {
		ids.ParentID = zenDefaultParent
	}
	return ids
}

// zenUpstreamUserAgent picks the User-Agent sent upstream. The Zen free tier
// only serves requests that look like they come from OpenCode, so a foreign
// caller UA (curl, SDKs) is never forwarded; an OpenCode caller UA is
// preserved for sticky identity.
func zenUpstreamUserAgent(inbound string) string {
	if ua := strings.TrimSpace(inbound); ua != "" {
		if strings.HasPrefix(strings.ToLower(ua), "opencode/") {
			return ua
		}
	}
	return zenUserAgent
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
	h.Set("User-Agent", zenUpstreamUserAgent(ids.UserAgent))
}

// setZenResponsesHeaders emits the identity headers captured live from the
// opencode 1.19.0 CLI on Zen Responses requests (2026-10-13). The session
// alias on this path is x-opencode-session-id (not the chat lane's
// x-session-affinity / X-Session-Id), and the chat-only x-opencode-parent and
// prompt_cache_key header are omitted (Responses carries prompt_cache_key in
// its JSON body).
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
	h.Set("x-opencode-session-id", ids.SessionID)
	h.Set("User-Agent", zenUpstreamUserAgent(ids.UserAgent))
}
