package app

import (
	"net/http"
	"strconv"
	"time"
)

// usageHistoryMaxDays caps ?days= to what the history can hold.
const usageHistoryMaxDays = usageHistoryRetentionDays

// GET /admin/api/usage/history
//
// Window (UTC days, inclusive), first match wins:
//   - period=billing — the current Command Code billing period, from the
//     cmdcode accounts' cached quota (plan.current_period_start)
//   - from=YYYY-MM-DD[&to=YYYY-MM-DD] — explicit range (to defaults to today)
//   - days=N — the last N days including today (default 30)
//
// group=key,model,gateway,day (any combination; default key) picks the
// dimensions kept in each row.
func handleAdminUsageHistory(pool *AccountPool, keys *ClientKeyPool, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		group, ok := parseUsageHistoryGroup(q.Get("group"))
		if !ok {
			writeAdminError(w, r, http.StatusBadRequest, "group must be a comma-separated list of key, model, gateway, day")
			return
		}

		now := usageNow()
		today := usageDay(now)
		from, to := "", today
		var periodStart *time.Time
		periodName := ""

		switch {
		case q.Get("period") != "":
			if q.Get("period") != "billing" {
				writeAdminError(w, r, http.StatusBadRequest, "period must be billing")
				return
			}
			periodStart = billingPeriodStart(pool, usage)
			if periodStart == nil {
				writeAdminError(w, r, http.StatusConflict, "no billing period known yet; refresh the account quota first")
				return
			}
			from = billingPeriodFirstDay(*periodStart)
			periodName = "billing"
		case q.Get("from") != "":
			if !validUsageDay(q.Get("from")) || (q.Get("to") != "" && !validUsageDay(q.Get("to"))) {
				writeAdminError(w, r, http.StatusBadRequest, "from/to must be YYYY-MM-DD")
				return
			}
			from = q.Get("from")
			if q.Get("to") != "" {
				to = q.Get("to")
			}
		default:
			days := 30
			if raw := q.Get("days"); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil || n < 1 || n > usageHistoryMaxDays {
					writeAdminError(w, r, http.StatusBadRequest, "days must be between 1 and 120")
					return
				}
				days = n
			}
			from = usageDay(now.AddDate(0, 0, -(days - 1)))
		}

		rows, total := usage.QueryHistory(from, to, group)
		if group.Key {
			for i := range rows {
				rows[i].KeyName = clientKeyDisplayName(keys, rows[i].ClientKeyID)
			}
		}
		resp := map[string]any{
			"from":           from,
			"to":             to,
			"group":          group.String(),
			"rows":           rows,
			"total":          total,
			"retention_days": usageHistoryRetentionDays,
		}
		if periodName != "" {
			resp["period"] = periodName
			resp["period_start"] = periodStart
		}
		writeAdminJSON(w, http.StatusOK, resp)
	}
}

func validUsageDay(s string) bool {
	_, err := time.Parse(usageDayLayout, s)
	return err == nil
}

// billingPeriodStart returns the most recent plan period start among the
// cmdcode accounts' cached quotas (with several accounts on different cycles,
// the latest start is the window every account is inside of).
func billingPeriodStart(pool *AccountPool, usage *UsageTracker) *time.Time {
	if pool == nil {
		return nil
	}
	var latest *time.Time
	for _, acct := range pool.List() {
		snap := usage.Quota(acct.ID)
		if snap == nil || snap.Plan == nil || snap.Plan.CurrentPeriodStart == nil {
			continue
		}
		start := *snap.Plan.CurrentPeriodStart
		if latest == nil || start.After(*latest) {
			latest = &start
		}
	}
	return latest
}

// billingPeriodFirstDay maps a period start to the first UTC history day that
// belongs to it. History is daily, so the start day is shared with the end of
// the previous period: it counts only when the period starts in its first
// half, otherwise the window begins the next day.
func billingPeriodFirstDay(start time.Time) string {
	return usageDay(start.UTC().Add(12 * time.Hour))
}

// clientKeyDisplayName resolves a history key ID to its current name.
func clientKeyDisplayName(keys *ClientKeyPool, id string) string {
	if id == noClientKey || id == "" {
		return "(no key)"
	}
	if keys != nil {
		if k := keys.Get(id); k != nil {
			return k.Name
		}
	}
	return "(deleted key)"
}
