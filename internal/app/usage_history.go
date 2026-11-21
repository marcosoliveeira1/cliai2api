package app

import (
	"sort"
	"strings"
	"time"
)

// Usage history: per-UTC-day counters split by client key, gateway and model.
//
// The lifetime counters in UsageTracker answer "how much has key X used since
// it was created", but not "who burned this month's credits": a billing period
// is a time window, and the upstream (Command Code /alpha/usage/summary) only
// reports the account-wide total. Daily buckets keyed by client key + model let
// the admin API sum any window and attribute it.

// usageHistoryRetentionDays bounds how many UTC days persist in usage.json.
// Command Code bills monthly, so ~4 months keeps the current and the previous
// periods with room to spare while the file stays small (one entry per active
// key/model per day).
const usageHistoryRetentionDays = 120

// usageNow is a var so tests can pin the clock.
var usageNow = time.Now

const usageDayLayout = "2006-01-02"

func usageDay(t time.Time) string { return t.UTC().Format(usageDayLayout) }

// noClientKey marks usage recorded without an authenticated client key
// (e.g. internal calls); it can never collide with a real "k…" key ID.
const noClientKey = "-"

// usageHistoryKey joins the history dimensions. "|" never appears in client
// key IDs or gateway names; it is replaced in model IDs defensively so the key
// always splits back into exactly three parts.
func usageHistoryKey(clientKeyID, gateway, model string) string {
	if clientKeyID == "" {
		clientKeyID = noClientKey
	}
	return clientKeyID + "|" + gateway + "|" + strings.ReplaceAll(model, "|", "/")
}

func usageHistorySplit(key string) (clientKeyID, gateway, model string) {
	parts := strings.SplitN(key, "|", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

// historyGateway normalizes gateway names the same way account counters do
// (the legacy "opencode" name is the zen gateway).
func historyGateway(gateway string) string {
	switch gateway {
	case "":
		return GatewayCmdcode
	case GatewayOpencode:
		return GatewayZen
	}
	return gateway
}

// recordHistory adds one request to today's bucket for the given dimensions.
func (u *UsageTracker) recordHistory(clientKeyID, gateway, model string, prompt, completion, cacheRead, cacheWrite int) {
	now := usageNow()
	day := usageDay(now)
	key := usageHistoryKey(clientKeyID, historyGateway(gateway), model)
	u.accMu.Lock()
	defer u.accMu.Unlock()
	if u.history == nil {
		u.history = make(map[string]map[string]*UsageCounters)
	}
	bucket := u.history[day]
	if bucket == nil {
		bucket = make(map[string]*UsageCounters)
		u.history[day] = bucket
		// A new day is the only moment old days can fall out of retention.
		u.pruneHistoryLocked(now)
	}
	c := bucket[key]
	if c == nil {
		c = &UsageCounters{}
		bucket[key] = c
	}
	c.add(prompt, completion, cacheRead, cacheWrite)
}

func (u *UsageTracker) pruneHistoryLocked(now time.Time) {
	cutoff := usageDay(now.AddDate(0, 0, -usageHistoryRetentionDays))
	for day := range u.history {
		if day < cutoff {
			delete(u.history, day)
		}
	}
}

// historySnapshot copies the history for persistence.
func (u *UsageTracker) historySnapshot() map[string]map[string]UsageSnapshotEntry {
	u.accMu.Lock()
	defer u.accMu.Unlock()
	if len(u.history) == 0 {
		return nil
	}
	out := make(map[string]map[string]UsageSnapshotEntry, len(u.history))
	for day, bucket := range u.history {
		entries := make(map[string]UsageSnapshotEntry, len(bucket))
		for key, c := range bucket {
			entries[key] = c.snapshot()
		}
		out[day] = entries
	}
	return out
}

// restoreHistoryLocked loads persisted history; callers hold accMu.
func (u *UsageTracker) restoreHistoryLocked(history map[string]map[string]UsageSnapshotEntry) {
	if len(history) == 0 {
		return
	}
	if u.history == nil {
		u.history = make(map[string]map[string]*UsageCounters)
	}
	for day, entries := range history {
		if _, err := time.Parse(usageDayLayout, day); err != nil {
			continue
		}
		bucket := make(map[string]*UsageCounters, len(entries))
		for key, entry := range entries {
			c := &UsageCounters{}
			c.restore(entry)
			bucket[key] = c
		}
		u.history[day] = bucket
	}
	u.pruneHistoryLocked(usageNow())
}

// UsageHistoryRow is one aggregated slice of the history. Dimensions that are
// not part of the grouping are left empty.
type UsageHistoryRow struct {
	Day         string `json:"day,omitempty"`
	ClientKeyID string `json:"key_id,omitempty"`
	KeyName     string `json:"key_name,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Model       string `json:"model,omitempty"`
	UsageSnapshotEntry
}

// usageHistoryGroup selects which dimensions a query keeps.
type usageHistoryGroup struct {
	Day, Key, Gateway, Model bool
}

func parseUsageHistoryGroup(raw string) (usageHistoryGroup, bool) {
	var g usageHistoryGroup
	if strings.TrimSpace(raw) == "" {
		g.Key = true
		return g, true
	}
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(strings.ToLower(part)) {
		case "day":
			g.Day = true
		case "key":
			g.Key = true
		case "gateway":
			g.Gateway = true
		case "model":
			g.Model = true
		case "":
		default:
			return g, false
		}
	}
	return g, true
}

func (g usageHistoryGroup) String() string {
	var parts []string
	if g.Day {
		parts = append(parts, "day")
	}
	if g.Key {
		parts = append(parts, "key")
	}
	if g.Gateway {
		parts = append(parts, "gateway")
	}
	if g.Model {
		parts = append(parts, "model")
	}
	return strings.Join(parts, ",")
}

// QueryHistory sums the UTC days in [from, to] (inclusive, YYYY-MM-DD) by the
// requested dimensions. Rows come sorted by day, then by tokens descending.
func (u *UsageTracker) QueryHistory(from, to string, group usageHistoryGroup) ([]UsageHistoryRow, UsageSnapshotEntry) {
	type rowKey struct{ day, key, gateway, model string }
	sums := map[rowKey]*UsageSnapshotEntry{}
	var total UsageSnapshotEntry

	u.accMu.Lock()
	for day, bucket := range u.history {
		if day < from || day > to {
			continue
		}
		for hk, c := range bucket {
			keyID, gateway, model := usageHistorySplit(hk)
			rk := rowKey{}
			if group.Day {
				rk.day = day
			}
			if group.Key {
				rk.key = keyID
			}
			if group.Gateway {
				rk.gateway = gateway
			}
			if group.Model {
				rk.model = model
			}
			e := c.snapshot()
			acc := sums[rk]
			if acc == nil {
				acc = &UsageSnapshotEntry{}
				sums[rk] = acc
			}
			acc.addEntry(e)
			total.addEntry(e)
		}
	}
	u.accMu.Unlock()

	rows := make([]UsageHistoryRow, 0, len(sums))
	for rk, e := range sums {
		rows = append(rows, UsageHistoryRow{Day: rk.day, ClientKeyID: rk.key, Gateway: rk.gateway, Model: rk.model, UsageSnapshotEntry: *e})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Day != rows[j].Day {
			return rows[i].Day < rows[j].Day
		}
		ti := rows[i].PromptTokens + rows[i].CompletionTokens
		tj := rows[j].PromptTokens + rows[j].CompletionTokens
		if ti != tj {
			return ti > tj
		}
		if rows[i].ClientKeyID != rows[j].ClientKeyID {
			return rows[i].ClientKeyID < rows[j].ClientKeyID
		}
		return rows[i].Model < rows[j].Model
	})
	return rows, total
}

func (e *UsageSnapshotEntry) addEntry(o UsageSnapshotEntry) {
	e.Requests += o.Requests
	e.PromptTokens += o.PromptTokens
	e.CompletionTokens += o.CompletionTokens
	e.CacheReadTokens += o.CacheReadTokens
	e.CacheWriteTokens += o.CacheWriteTokens
}
