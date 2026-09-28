package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultZenBaseURL is the OpenCode Zen endpoint all Zen upstream calls go
// through unless overridden via SetBaseURL (tests point it at httptest fakes).
const defaultZenBaseURL = "https://opencode.ai/zen"

// zenAnonymousKey is the shared public credential the Zen free tier expects.
// Free-tier models reject real account keys with permission_denied ("can
// only be used from within OpenCode"); anonymous requests carry this key
// plus the standard CLI identity headers. Keyed accounts stay as fallback
// when the anonymous attempt fails.
const zenAnonymousKey = "public"

// anonymousAccount stands in for the public credential in logs and usage
// counters. It is never a member of any pool.
var anonymousAccount = newAccount("anonymous(public)", zenAnonymousKey, true)

// ZenClient sends requests to the OpenCode Zen upstream, rotating across the
// accounts in its pool and failing over on account-scoped errors. It mirrors
// the CCClient failover loop shape, except the 4xx semantics follow
// isNonRetryableClientResponse (issues §5): 400–499 except 401/403/429 end the
// attempt without rotating or cooling the key; 402 even clears failures via
// RecordSuccess. Failover only happens while the client response is still
// unwritten — once a stream starts, it is never replayed.
type ZenClient struct {
	pool   *AccountPool
	Base   string
	Client *http.Client

	// baseURLMu guards Base, which the admin API can update at runtime.
	baseURLMu sync.RWMutex
}

var _ Gateway = (*ZenClient)(nil)

func NewZenClientWithPool(pool *AccountPool, baseURL string) *ZenClient {
	if baseURL == "" {
		baseURL = defaultZenBaseURL
	}
	return &ZenClient{
		pool:   pool,
		Base:   baseURL,
		Client: &http.Client{Timeout: 600 * time.Second},
	}
}

func (z *ZenClient) Name() string        { return GatewayOpencode }
func (z *ZenClient) ModelPrefix() string { return OpencodePrefix }

// Pool returns the account pool, defaulting to an empty one when the client
// was built without accounts so Chat reports 503 no_accounts.
func (z *ZenClient) Pool() *AccountPool {
	if z.pool == nil {
		return NewAccountPool(nil)
	}
	return z.pool
}

func (z *ZenClient) BaseURL() string {
	z.baseURLMu.RLock()
	defer z.baseURLMu.RUnlock()
	return z.Base
}

func (z *ZenClient) SetBaseURL(url string) {
	z.baseURLMu.Lock()
	defer z.baseURLMu.Unlock()
	z.Base = url
}

// FetchModels returns the Zen raw catalog (unprefixed; prefix is applied at
// union time). Without an enabled account it contributes empty. Upstream
// failures degrade to empty — the caller logs the [WARN].
func (z *ZenClient) FetchModels() []ModelInfo {
	pool := z.Pool()
	primary := pool.Primary()
	if primary == nil || !primary.Enabled {
		return nil
	}
	client := z.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequest("GET", z.BaseURL()+"/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+primary.APIKey)
	setZenHeaders(req.Header, primary.APIKey, DeriveZenRequestIDs(nil))
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list struct {
		Object string      `json:"object"`
		Data   []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil
	}
	return list.Data
}

// Chat posts the OpenAI body 1:1 to {base}/v1/chat/completions with the
// opencode/ prefix stripped and the CLI identity headers from T4 injected.
// The returned Account is the credential that produced the response or error.
func (z *ZenClient) Chat(ctx context.Context, req *ChatRequest) (*http.Response, *Account, error) {
	return z.ChatWithHeaders(ctx, req, nil)
}

// ChatWithHeaders preserves caller-provided Zen identity headers across
// requests so session affinity continues across conversation turns.
func (z *ZenClient) ChatWithHeaders(ctx context.Context, req *ChatRequest, inbound http.Header) (*http.Response, *Account, error) {
	out := *req
	if rest, ok := strings.CutPrefix(out.Model, OpencodePrefix); ok {
		out.Model = rest
	}
	family, known := classifyZenFamily(out.Model)
	if !known {
		log.Printf("[WARN] unknown zen model family %q, falling back to chat passthrough", out.Model)
	}
	// Free-tier lane only serves agent-shape streaming (issues §1): rewrite
	// chat requests before the upstream sees them. Muse Spark free variants
	// use the Responses API and must retain the caller's request shape instead.
	shaped := family == zenFamilyChat && shapeFreeBody(&out)
	wantCollapse := shaped && !req.Stream
	body, err := json.Marshal(&out)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal zen request: %w", err)
	}

	// One session per logical request: failover retries keep the same IDs so
	// prompt-cache affinity holds across keys.
	ids := DeriveZenRequestIDs(inbound)

	if family == zenFamilyResponses {
		// Responses free models need the same agent-shape tools the chat
		// lane gets from shapeFreeBody; the converter only forwards
		// declared tools, and the anonymous tier rejects tool-less calls.
		if IsFreeModel(out.Model) {
			ensureFreeTools(&out)
		}
		responsesBody, err := chatRequestToResponses(&out, ids)
		if err != nil {
			return nil, nil, &invalidRequestError{message: "cannot convert chat request for Zen Responses: " + err.Error()}
		}
		return z.chatResponses(ctx, &out, req.Stream, responsesBody, ids)
	}

	// Free-tier lane: the upstream only serves *-free models to the shared
	// public credential. Try it before the keyed accounts.
	if IsFreeModel(out.Model) {
		if resp, acct, err := z.chatAnonymous(ctx, body, ids, wantCollapse, bareModel(req.Model)); err == nil {
			return resp, acct, nil
		} else {
			log.Printf("[WARN] anonymous free-tier attempt for %q failed, falling back to accounts: %v", out.Model, err)
		}
	}

	pool := z.Pool()
	attempts := pool.EnabledCount()
	if attempts == 0 {
		return nil, nil, &upstreamAPIError{
			Status:  http.StatusServiceUnavailable,
			Type:    "server_error",
			Code:    "no_accounts",
			Message: "no enabled zen accounts (gateway: zen)",
		}
	}

	var lastErr error
	var lastAcct *Account
	for attempt := 0; attempt < attempts; attempt++ {
		acct := pool.Acquire()
		if acct == nil {
			// Every enabled account is cooling down from a 429.
			break
		}
		lastAcct = acct
		resp, err := z.doChat(ctx, body, acct.APIKey, ids)
		if err == nil {
			acct.RecordSuccess()
			if wantCollapse {
				collapsed, cerr := collapseStream(resp, bareModel(req.Model))
				if cerr != nil {
					return nil, acct, cerr
				}
				return collapsed, acct, nil
			}
			return resp, acct, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, acct, err
		}
		if isNonRetryableClientResponse(err) {
			// Request-scoped failure: deterministic on every key, so staying
			// put preserves the pool. 402 even clears past failures.
			var apiErr *upstreamAPIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusPaymentRequired {
				acct.RecordSuccess()
			}
			return nil, acct, err
		}
		acct.RecordFailure(err)
		lastErr = err
		log.Printf("[WARN] account %s request failed, failing over: %v", acct.Name, err)
	}
	if lastErr != nil {
		return nil, lastAcct, lastErr
	}

	wait := pool.EarliestRateLimitWait(time.Now()).Round(time.Second)
	retryAfter := ""
	message := "all zen accounts are rate limited (gateway: zen)"
	if wait > 0 {
		retryAfter = strconv.FormatInt(int64(wait.Seconds()), 10)
		message += fmt.Sprintf("; next account available in %s", wait)
	}
	return nil, nil, &upstreamAPIError{
		Status:     http.StatusTooManyRequests,
		Type:       "rate_limit_error",
		Code:       "rate_limit_exceeded",
		Message:    message,
		RetryAfter: retryAfter,
	}
}

// chatAnonymous serves one chat-lane request through the shared public
// credential the free tier requires. It mirrors the keyed success path
// (including stream collapse) without touching any pool account.
func (z *ZenClient) chatAnonymous(ctx context.Context, body []byte, ids ZenRequestIDs, wantCollapse bool, model string) (*http.Response, *Account, error) {
	resp, err := z.doChat(ctx, body, zenAnonymousKey, ids)
	if err != nil {
		return nil, anonymousAccount, err
	}
	anonymousAccount.RecordSuccess()
	if wantCollapse {
		collapsed, cerr := collapseStream(resp, model)
		if cerr != nil {
			return nil, anonymousAccount, cerr
		}
		return collapsed, anonymousAccount, nil
	}
	return resp, anonymousAccount, nil
}

// isNonRetryableClientResponse reports whether an upstream error is
// deterministic per request: 400–499 except 401/403/429 would fail identically
// on every key, so the Zen path ends the attempt instead of failing over.
// Anything else (auth, rate limit, 5xx, transport) is worth retrying.
func isNonRetryableClientResponse(err error) bool {
	var upstreamErr *upstreamAPIError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	return isNonRetryableStatus(upstreamErr.Status)
}

func isNonRetryableStatus(status int) bool {
	if status < http.StatusBadRequest || status > 499 {
		return false
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return false
	}
	return true
}

// bareModel strips the opencode/ prefix for upstream and display use.
func bareModel(model string) string {
	if rest, ok := strings.CutPrefix(model, OpencodePrefix); ok {
		return rest
	}
	return model
}

func (z *ZenClient) doChat(ctx context.Context, body []byte, apiKey string, ids ZenRequestIDs) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", z.BaseURL()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setZenHeaders(httpReq.Header, apiKey, ids)

	resp, err := z.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		return nil, normalizeUpstreamError(resp.StatusCode, errBody, resp.Header)
	}
	return resp, nil
}

// chatResponses runs the responses lane: POST {base}/v1/responses with
// failover, then translates the SSE into OpenAI chunks (stream) or folds it
// into chat.completion JSON (non-stream). Translation happens after failover
// so a 502 incomplete never rotates keys.
func (z *ZenClient) chatResponses(ctx context.Context, out *ChatRequest, wantStream bool, body []byte, ids ZenRequestIDs) (*http.Response, *Account, error) {
	pool := z.Pool()
	attempts := pool.EnabledCount()
	if attempts == 0 {
		return nil, nil, &upstreamAPIError{
			Status:  http.StatusServiceUnavailable,
			Type:    "server_error",
			Code:    "no_accounts",
			Message: "no enabled zen accounts (gateway: zen)",
		}
	}

	model := out.Model
	// Free-tier lane first: the upstream only serves *-free models to the
	// shared public credential. Keyed accounts stay as fallback.
	if IsFreeModel(model) {
		if resp, acct, err := z.chatResponsesAnonymous(ctx, wantStream, body, ids, model); err == nil {
			return resp, acct, nil
		} else {
			log.Printf("[WARN] anonymous free-tier attempt for %q failed, falling back to accounts: %v", model, err)
		}
	}
	var lastErr error
	var lastAcct *Account
	for attempt := 0; attempt < attempts; attempt++ {
		acct := pool.Acquire()
		if acct == nil {
			break
		}
		lastAcct = acct
		resp, err := z.doResponses(ctx, body, acct.APIKey, ids)
		if err == nil {
			acct.RecordSuccess()
			if wantStream {
				translated, terr := translateZenResponsesStream(resp, model)
				if terr != nil {
					return nil, acct, terr
				}
				return translated, acct, nil
			}
			aggregated, aerr := aggregateZenResponses(resp, model)
			if aerr != nil {
				return nil, acct, aerr
			}
			return aggregated, acct, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, acct, err
		}
		if isNonRetryableClientResponse(err) {
			var apiErr *upstreamAPIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusPaymentRequired {
				acct.RecordSuccess()
			}
			return nil, acct, err
		}
		acct.RecordFailure(err)
		lastErr = err
		log.Printf("[WARN] account %s request failed, failing over: %v", acct.Name, err)
	}
	if lastErr != nil {
		return nil, lastAcct, lastErr
	}

	wait := pool.EarliestRateLimitWait(time.Now()).Round(time.Second)
	retryAfter := ""
	message := "all zen accounts are rate limited (gateway: zen)"
	if wait > 0 {
		retryAfter = strconv.FormatInt(int64(wait.Seconds()), 10)
		message += fmt.Sprintf("; next account available in %s", wait)
	}
	return nil, nil, &upstreamAPIError{
		Status:     http.StatusTooManyRequests,
		Type:       "rate_limit_error",
		Code:       "rate_limit_exceeded",
		Message:    message,
		RetryAfter: retryAfter,
	}
}

// chatResponsesAnonymous serves one responses-lane request through the
// shared public credential the free tier requires, translating the SSE the
// same way the keyed path does.
func (z *ZenClient) chatResponsesAnonymous(ctx context.Context, wantStream bool, body []byte, ids ZenRequestIDs, model string) (*http.Response, *Account, error) {
	resp, err := z.doResponses(ctx, body, zenAnonymousKey, ids)
	if err != nil {
		return nil, anonymousAccount, err
	}
	anonymousAccount.RecordSuccess()
	if wantStream {
		translated, terr := translateZenResponsesStream(resp, model)
		if terr != nil {
			return nil, anonymousAccount, terr
		}
		return translated, anonymousAccount, nil
	}
	aggregated, aerr := aggregateZenResponses(resp, model)
	if aerr != nil {
		return nil, anonymousAccount, aerr
	}
	return aggregated, anonymousAccount, nil
}

func (z *ZenClient) doResponses(ctx context.Context, body []byte, apiKey string, ids ZenRequestIDs) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", z.BaseURL()+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setZenResponsesHeaders(httpReq.Header, apiKey, ids)

	resp, err := z.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		return nil, normalizeUpstreamError(resp.StatusCode, errBody, resp.Header)
	}
	return resp, nil
}
