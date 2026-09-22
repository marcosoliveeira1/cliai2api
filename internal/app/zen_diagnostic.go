package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// T9 diagnostic surface (GW-07).
//
// Endpoint shape (chosen: simplest consistent with the existing admin API):
//
//	POST /admin/api/debug/inference
//	{"gateway":"zen","account_id":"...","model":"...","messages":[{"role":"user","content":"hi"}]}
//
// Response:
//
//	{"ok":bool,"http_status":int,"request_id":string,"route":string,
//	 "response":{...upstream echo or openai result...},
//	 "key_test":"usable|rejected|rate_limited|transport_error|upstream_error|request_error|unavailable",
//	 "gateway":string,"account_id":string,"fingerprint":string,"latency_ms":int64,"error":string}
//
// Semantics (Playground selected-key, issues §13 item 2): exactly 1 request
// to exactly 1 key — no failover, no RecordSuccess/RecordFailure, no
// cooldown or binding mutation. The pool is only read (Get), never Acquire.
// Classification mapping: 402 (and the other non-retryable 4xx: 400/404/422)
// is request_error, never rejected — the request is malformed or unfunded on
// every key, so staying put preserves the pool (isNonRetryableStatus).
// 401/403 map to rejected, 429 to rate_limited, transport failures to
// transport_error, 5xx to upstream_error, and a missing key/disabled state to
// unavailable.

// keyTestOutcome is the Playground per-key verdict for one diagnostic request.
type keyTestOutcome string

const (
	keyTestUsable         keyTestOutcome = "usable"
	keyTestRejected       keyTestOutcome = "rejected"
	keyTestRateLimited    keyTestOutcome = "rate_limited"
	keyTestTransportError keyTestOutcome = "transport_error"
	keyTestUpstreamError  keyTestOutcome = "upstream_error"
	keyTestRequestError   keyTestOutcome = "request_error"
	keyTestUnavailable    keyTestOutcome = "unavailable"
)

// classifyKeyTest maps one upstream attempt outcome to a key_test verdict.
// Note: 402 → request_error (not rejected): an insufficient-funds request
// would fail identically on every key, so it diagnoses the request, not the
// credential — document and keep this mapping stable in tests.
func classifyKeyTest(status int, transportErr bool) keyTestOutcome {
	if transportErr {
		return keyTestTransportError
	}
	switch status {
	case 0:
		return keyTestUnavailable
	case http.StatusUnauthorized, http.StatusForbidden:
		return keyTestRejected
	case http.StatusTooManyRequests:
		return keyTestRateLimited
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return keyTestUpstreamError
	}
	if isNonRetryableStatus(status) {
		return keyTestRequestError
	}
	if status >= 500 {
		return keyTestUpstreamError
	}
	return keyTestRequestError
}

// zenDiagnosticRequest runs one selected-key probe against baseURL without
// touching the pool: no Acquire, no RecordSuccess/RecordFailure, no cooldown.
// It posts the OpenAI body 1:1 to the chat lane (model without prefix) with
// the CLI identity headers, mirroring ZenClient.doChat minus rotation — the
// chat lane is the passthrough-safe default for unknown models, matching
// classifyZenFamily's fallback. The returned map is the JSON response body.
func zenDiagnosticRequest(ctx context.Context, baseURL, apiKey, model string, messages []Message) (status int, respBody map[string]any, retryAfter string, requestID string, latency time.Duration, err error) {
	out := &ChatRequest{Model: model, Messages: messages}
	shapeFreeBody(out)
	body, merr := json.Marshal(out)
	if merr != nil {
		return 0, nil, "", "", 0, fmt.Errorf("marshal diagnostic request: %w", merr)
	}
	ids := DeriveZenRequestIDs(nil)
	httpReq, merr := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(baseURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if merr != nil {
		return 0, nil, "", "", 0, fmt.Errorf("create request: %w", merr)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setZenHeaders(httpReq.Header, apiKey, ids)

	client := &http.Client{Timeout: 60 * time.Second}
	start := time.Now()
	resp, derr := client.Do(httpReq)
	latency = time.Since(start)
	if derr != nil {
		return 0, nil, "", ids.RequestID, latency, fmt.Errorf("send request: %w", derr)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	retryAfter = resp.Header.Get("Retry-After")
	requestID = resp.Header.Get("x-request-id")
	if requestID == "" {
		requestID = resp.Header.Get("lb-request-id")
	}
	if requestID == "" {
		requestID = ids.RequestID
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	if decoded == nil {
		decoded = map[string]any{"raw": strings.TrimSpace(string(raw))}
	}
	if resp.StatusCode != http.StatusOK {
		apiErr := normalizeUpstreamError(resp.StatusCode, raw, resp.Header)
		return resp.StatusCode, decoded, retryAfter, requestID, latency,
			&diagnosticUpstreamError{status: resp.StatusCode, apiErr: apiErr}
	}
	return resp.StatusCode, decoded, retryAfter, requestID, latency, nil
}

// diagnosticUpstreamError carries a non-2xx diagnostic attempt without
// touching pool state.
type diagnosticUpstreamError struct {
	status int
	apiErr *upstreamAPIError
}

func (e *diagnosticUpstreamError) Error() string {
	if e.apiErr != nil {
		return e.apiErr.Error()
	}
	return fmt.Sprintf("diagnostic upstream error %d", e.status)
}

// handleAdminDebugInference is the Playground selected-key endpoint: 1 request,
// 1 key, no failover, no pool mutation. Cmdcode routes to the existing key
// probe for compat; zen takes the diagnostic chat lane.
func handleAdminDebugInference(pool *AccountPool, zenPool *AccountPool, cc *CCClient, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Gateway   string    `json:"gateway"`
			AccountID string    `json:"account_id"`
			Model     string    `json:"model"`
			Messages  []Message `json:"messages"`
		}
		if err := decodeJSONBody(w, r, &body); err != nil {
			writeAdminError(w, r, 400, err.Error())
			return
		}
		gateway, ok := normalizeAdminGateway(body.Gateway)
		if body.Gateway == "" {
			gateway, ok = gwNameFromAccount(pool, zenPool, body.AccountID)
		}
		if !ok {
			writeAdminError(w, r, 400, "unknown gateway")
			return
		}
		if strings.TrimSpace(body.AccountID) == "" {
			writeAdminError(w, r, 400, "account_id is required")
			return
		}
		if strings.TrimSpace(body.Model) == "" {
			writeAdminError(w, r, 400, "model is required")
			return
		}
		gwName, acct, _ := adminPoolsForID(pool, zenPool, body.AccountID)
		if acct == nil {
			writeAdminJSON(w, 200, map[string]any{
				"ok": false, "http_status": 0, "request_id": "", "route": gateway,
				"response": nil, "key_test": string(keyTestUnavailable),
				"gateway": gateway, "account_id": body.AccountID, "fingerprint": "",
				"latency_ms": int64(0), "error": "account not found",
			})
			return
		}
		if !acct.Enabled {
			writeAdminJSON(w, 200, map[string]any{
				"ok": false, "http_status": 0, "request_id": "", "route": gwName,
				"response": nil, "key_test": string(keyTestUnavailable),
				"gateway": gwName, "account_id": acct.ID, "fingerprint": acct.ID,
				"latency_ms": int64(0), "error": "account disabled",
			})
			return
		}

		model := strings.TrimSpace(body.Model)
		if strings.Contains(model, "/") {
			rest, cutGateway, err := SplitModel(model)
			if err != nil {
				writeAdminError(w, r, 400, "unknown model prefix")
				return
			}
			if cutGateway != gwName {
				writeAdminError(w, r, 400, "model gateway does not match account gateway")
				return
			}
			model = rest
		}
		messages := body.Messages
		if len(messages) == 0 {
			messages = []Message{{Role: "user", Content: TextContent("ping")}}
		}

		if gwName == GatewayCmdcode {
			base := ""
			if cc != nil {
				base = cc.BaseURLValue()
			}
			if base == "" && cfg != nil {
				base = cfg.UpstreamBaseURL()
			}
			result := testAccountKey(base, acct.APIKey)
			recordUpstreamAttempt(gwName, acct.ID, result.Status, string(diagnosticResultToKeyTest(result)), nil)
			var keyTest keyTestOutcome
			var status int
			var errMsg string
			switch {
			case result.OK:
				keyTest, status = keyTestUsable, http.StatusOK
			case result.Error != "" && result.Status == 0:
				keyTest, errMsg = keyTestTransportError, result.Error
			default:
				keyTest, status, errMsg = classifyKeyTest(result.Status, false), result.Status, result.Error
			}
			writeAdminJSON(w, 200, map[string]any{
				"ok": result.OK, "http_status": status, "request_id": "", "route": gwName,
				"response": map[string]any{"models": result.Models},
				"key_test": string(keyTest),
				"gateway":  gwName, "account_id": acct.ID, "fingerprint": acct.ID,
				"latency_ms": result.LatencyMS, "error": errMsg,
			})
			return
		}

		base := ""
		if cfg != nil {
			base = cfg.GatewayBaseURL(GatewayZen)
		}
		if base == "" {
			base = defaultZenBaseURL
		}
		status, respBody, retryAfter, requestID, latency, derr := zenDiagnosticRequest(r.Context(), base, acct.APIKey, model, messages)
		latencyMS := latency.Milliseconds()
		if derr == nil {
			recordUpstreamAttempt(gwName, acct.ID, status, string(keyTestUsable), nil)
			writeAdminJSON(w, 200, map[string]any{
				"ok": true, "http_status": status, "request_id": requestID, "route": gwName,
				"response": respBody, "key_test": string(keyTestUsable),
				"gateway": gwName, "account_id": acct.ID, "fingerprint": acct.ID,
				"latency_ms": latencyMS, "error": "",
			})
			return
		}
		var diagErr *diagnosticUpstreamError
		var transport bool
		var apiErr *upstreamAPIError
		if isDiagErr(derr, &diagErr) {
			apiErr = diagErr.apiErr
		} else {
			transport = true
		}
		keyTest := classifyKeyTest(status, transport)
		var errMsg string
		if apiErr != nil {
			errMsg = apiErr.Message
			if retryAfter != "" && !strings.Contains(errMsg, retryAfter) {
				errMsg = strings.TrimSpace(errMsg + " (Retry-After: " + retryAfter + ")")
			}
		}
		if transport {
			errMsg = derr.Error()
		}
		// Client error shape preserves the upstream message + Retry-After —
		// the diagnostic body mirrors copyErrorResponse semantics for the
		// Playground surface.
		outcome := string(keyTest)
		recordUpstreamAttempt(gwName, acct.ID, status, outcome, derr)
		payload := map[string]any{
			"ok": false, "http_status": status, "request_id": requestID, "route": gwName,
			"response": respBody, "key_test": outcome,
			"gateway": gwName, "account_id": acct.ID, "fingerprint": acct.ID,
			"latency_ms": latencyMS, "error": errMsg,
		}
		if retryAfter != "" {
			payload["retry_after"] = retryAfter
		}
		writeAdminJSON(w, 200, payload)
	}
}

// diagnosticResultToKeyTest maps the legacy cmdcode key probe to key_test
// for the cmdcode diagnostic branch. A probe failure without status is a
// transport problem; otherwise the shared classifier decides (402 →
// request_error via isNonRetryableStatus).
func diagnosticResultToKeyTest(result accountTestResult) keyTestOutcome {
	if result.OK {
		return keyTestUsable
	}
	if result.Status == 0 {
		return keyTestTransportError
	}
	return classifyKeyTest(result.Status, false)
}

// isDiagErr unwraps a diagnostic error.
func isDiagErr(err error, target **diagnosticUpstreamError) bool {
	if de, ok := err.(*diagnosticUpstreamError); ok && de != nil {
		*target = de
		return true
	}
	return false
}
