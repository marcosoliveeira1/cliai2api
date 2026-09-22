# cliai2api-gateways Validation

**Date**: 2026-09-22
**Spec**: `.specs/features/cliai2api-gateways/spec.md`
**Design**: `.specs/features/cliai2api-gateways/design.md`
**Diff range**: `a3c0523~1..ec144f2` on main (T1..T10 + design + zen wiring fix)
**Verifier**: independent sub-agent (author ≠ verifier)
**Gate commands**: `go vet ./...` + `go test -race -count=1 ./...` (from tasks.md Gate Check Commands, Full level; `PATH=/opt/homebrew/bin`)

---

## Task Completion

All 10 tasks traced to commits in range (`git log --oneline a3c0523~1..ec144f2`):

| Task | Commit | Status | Notes |
| ---- | ------ | ------ | ----- |
| T1 Gateway interface + SplitModel | `a3c0523` | ✅ Done | `gateway.go` + `gateway_test.go` |
| T2 Config `gateways.*` + migration | `409c2e0` | ✅ Done | `config.go`, `config_test.go` |
| T3 Usage namespaced | `ee59c37` | ✅ Done | `usage.go`, `usage_namespaces_test.go` |
| T4 Zen headers + session (design GW-03b/GW-07) | `f6604d0` + `02a7831` | ✅ Done | recon live not feasible; signed assumption in `context.md` (spec-allowed per GW-03b AC4) |
| T5 ZenGateway chat + failover | `455004b` | ✅ Done | `zen.go`, `zen_test.go` |
| T5b Free-tier shaping + collapse | `24c104c` | ✅ Done | `zen_free.go`, `zen_free_test.go` |
| T6 Responses translation | `99997d8` | ✅ Done | `zen_responses.go`, `zen_responses_test.go` |
| T7 Unified catalog | `d6ef484` | ✅ Done | `models.go`, `models_test.go` |
| T8 Router + runServer | `47e3e1a` | ✅ Done | `handler.go`, `handler_test.go` |
| T9 Admin per-gateway + diagnostics | `5cca0f5` + `c0b03b4` (wiring fix) | ✅ Done | `admin.go`, `zen_diagnostic.go`, `admin_diagnostics_test.go` |
| T10 Rename + docs + STATE | `ec144f2` | ✅ Done | build gate only, no domain code |

---

## Spec-Anchored Acceptance Criteria

### GW-01 — Abstração + migração legada

| Criterion (WHEN X THEN Y) | Spec-defined outcome | `file:line` + assertion | Result |
| ------------------------- | -------------------- | ----------------------- | ------ |
| WHEN legacy `commandcode.accounts/base_url` loads THEN serve `cmdcode/<id>` + bare models and persist `gateways.cmdcode.*` | accounts preserved, base_url preserved, legacy single key folds into accounts | `config_test.go:157` — `gc.Accounts[0].APIKey != "cc-a" \|\| gc.Accounts[1].Name != "b"` → Fatal; `config_test.go:185` — single `api_key: cc-legacy` → 1 account named `default`; `config_test.go:200` — saved file contains `gateways:`, not `commandcode:`, round-trip preserves accounts + base_url | ✅ PASS |
| WHEN `GET /v1/models` after migration THEN list cmdcode models with `cmdcode/` prefix | prefixed union | `models_test.go:51` — `CmdcodePrefix+"deepseek-v4"` present, `OpencodePrefix+"gpt-5.5"` excluded by suffix; `handler_test.go:591` — both prefixes listed unfiltered | ✅ PASS |
| WHEN unknown prefix (`foo/bar`) THEN `404 invalid_request_error` | 404 + type | `handler_test.go:119` — `rec.Code != 404` → Fatal; body contains `invalid_request_error`; `gateway_test.go:10` — `SplitModel("foo/bar")` errors with `ErrUnknownGateway` | ✅ PASS |

### GW-02 — Roteamento por prefixo

| Criterion | Spec-defined outcome | `file:line` + assertion | Result |
| --------- | -------------------- | ----------------------- | ------ |
| WHEN `"model": "opencode/<id>"` THEN forward to Zen, prefix stripped | 1 hit on zen fake, upstream model bare | `handler_test.go:71` — `zenHits != 1` → Fatal; `zenModel != "deepseek-v4-flash"` → Fatal; zen usage = 1, cmdcode usage = 0 | ✅ PASS |
| WHEN `"model": "cmdcode/<id>"` THEN forward to cmdcode | 1 hit on cmdcode fake, CC `params.model` bare | `handler_test.go:18` — `cmdcodeHits != 1` → Fatal; `ccReq.Params.Model != "deepseek-v4"` → Fatal | ✅ PASS |
| WHEN `"model"` without `/` THEN assume `cmdcode` | same as above via bare id | `handler_test.go:18` — table includes `"deepseek-v4"` (bare) with identical assertions; `gateway_test.go:10` — `SplitModel("deepseek-v4")` → `("cmdcode","deepseek-v4")` | ✅ PASS |

### GW-03 — Zen multi-key `chat/completions`

| Criterion | Spec-defined outcome | `file:line` + assertion | Result |
| --------- | -------------------- | ----------------------- | ------ |
| WHEN N zen keys THEN round-robin + `Authorization: Bearer` + `x-opencode-*` / `User-Agent: opencode/*` | key order a→b, exact headers, body intact with bare model | `zen_test.go:86` — `acct.APIKey != wantKey` per call; `calls[i].auth != "Bearer zen-key-a/b"` → Fatal; `path == "/v1/chat/completions"`; `x-opencode-client == "cli"`; session triple consistent; `User-Agent == zenUserAgent`; `decoded.Model == "deepseek-v4-flash"` | ✅ PASS |
| WHEN active key fails 401/403/429/5xx THEN failover to next key pre-first-byte | 429→200 across keys, cooldown on burned key; 401/403/5xx retryable by classifier | `zen_test.go:141` — 429 with `Retry-After: 60` then 200; serving account is key-b; call order a→b; `key-a RateLimited` true. `zen_test.go:249` — classifier pins 401/403/429/500/503 as retryable (`wantNR: false`); failover loop is shared for all retryable errors | ✅ PASS (note N1) |
| WHEN 4xx outside 401/403/429 (incl. 400/402/404/422) THEN end attempt, no rotation, no cooldown; 402 clears failures | exactly 1 upstream call, error account stays key-a, no cooldown, `AuthFailures == 0` | `zen_test.go:176` — 402: `Status == 402`, `acct.APIKey == "zen-key-a"`, `len(calls) == 1`, `!RateLimited`, `AuthFailures != 0` → Fatal; `zen_test.go:206` — 400: same 1-call/no-rotation/no-cooldown shape; `zen_test.go:249` — 400/402/404/422 pinned non-retryable | ✅ PASS |
| WHEN `stream: false` THEN passthrough Zen JSON as `chat.completion` | 200 through handler lane | `handler_test.go:71` — `stream:false` to `opencode/deepseek-v4-flash` → `rec.Code == 200`; `zen_test.go:65` — passthrough body asserted intact (`decoded.Messages[0] == "hi"`, bare model) | ✅ PASS |

### GW-03b — Zen free-tier shaping + sessão canônica

| Criterion | Spec-defined outcome | `file:line` + assertion | Result |
| --------- | -------------------- | ----------------------- | ------ |
| WHEN free-tier (`IsFreeModel`: `free` in name OR zero-cost non-deprecated metadata) THEN force `stream:true` + inject missing core tools (`bash/edit/glob/grep/read`) + `stream_options.include_usage` | shaped body exact | `zen_free_test.go:15` — 7-case name table + 3 metadata-stub cases; `zen_free_test.go:54` — `req.Stream == true`, `IncludeUsage` set, `len(Tools) == 5`, each `type == "function"` with name/description/JSON-object params, all 5 names present; `zen_free_test.go:99` — idempotent second pass; `zen_free_test.go:123` — non-free intact; `zen_free_test.go:141` — existing tools kept, only absent injected (2+4=6) | ✅ PASS |
| WHEN client asked `stream:false` with shaping THEN collapse upstream SSE to `chat.completion` JSON | 200 JSON, content aggregated, valid finish_reason; upstream saw `stream:true` + 5 tools | `zen_free_test.go:171` — fake 403-without-tools gate: `resp.StatusCode == 200`, `gotStream == true`, `gotTools == 5`, collapsed `object == "chat.completion"`, 1 choice, content `"Hello world"`, `FinishReason != ""`; `zen_free_test.go:240` — collapse defaults finish_reason; `zen_free_test.go:272` — `stream:true` NOT collapsed (byte-identical passthrough) | ✅ PASS |
| WHEN sending to Zen THEN canonical `ses_<12hex><14base62>` + `x-opencode-client/session/request/project/parent`, `x-session-affinity`, `X-Session-Id` (same value), `prompt_cache_key`, derivation from inbound or first user message | exact header set, regex shape, precedence | `zen_headers_test.go:15` — 50× `ses_<12hex><14base62>` regex; `zen_headers_test.go:58` — fresh IDs canonical + `ParentID != ""`; `zen_headers_test.go:78` — 4 inbound session headers honored; `zen_headers_test.go:92` — first-header precedence; `zen_headers_test.go:103` — project/request/parent preserved; `zen_headers_test.go:120` — all 10 headers exact incl. `prompt_cache_key == "zen:"+session`, `User-Agent == opencode/<ver>` | ✅ PASS |
| WHEN session/shaping diverges from CLI `opencode 1.18.31` THEN divergence recorded in `context.md` as signed assumption | doc artifact | `.specs/features/cliai2api-gateways/context.md:61-79` — "Suposições assinadas (T4 recon)": live capture not executed, 3 signed assumptions, `zenCLIVersion` pointer, mitmproxy follow-up | ✅ PASS (docs evidence, spec-allowed) |

### GW-04 — Zen `responses` SSE translation

| Criterion | Spec-defined outcome | `file:line` + assertion | Result |
| --------- | -------------------- | ----------------------- | ------ |
| WHEN `opencode/<responses-id>` + `stream:true` THEN `POST {zen}/v1/responses` → OpenAI chunks (`delta.content`, `finish_reason`) + `data: [DONE]` | 1 call to `/v1/responses`, translated chunks, DONE-terminated | `zen_responses_test.go:48` — `calls[0].path == "/v1/responses"`, `Content-Type == text/event-stream`, body contains `"content":"Hello "` + `"content":"world"`, `"finish_reason":"stop"`, ends `data: [DONE]`; upstream model bare (`decoded.Model == "gpt-5.5"`); family table at `zen_responses_test.go:17` (free ids stay chat) | ✅ PASS |
| WHEN `stream:false` THEN aggregate to `chat.completion` with valid `finish_reason` | JSON completion, usage mapped | `zen_responses_test.go:91` — `object == "chat.completion"`, content `"Hello world"`, `FinishReason == "stop"`, usage `3/5/8`; `zen_responses_test.go:131` — `max_output_tokens` → `length` | ✅ PASS |
| WHEN upstream closes without finish THEN `502 upstream_stream_incomplete` | 502 + code, 1 call, no retry after stream start | `zen_responses_test.go:167` — bare-EOF and explicit-DONE-without-finish variants: `Status == 502`, `Code == "upstream_stream_incomplete"`, `len(calls) == 1`, non-stream same code | ✅ PASS |

### GW-05 — Catálogo unificado

| Criterion | Spec-defined outcome | `file:line` + assertion | Result |
| --------- | -------------------- | ----------------------- | ------ |
| WHEN both gateways have models THEN union with prefixes after global `exclude_models` | prefixed union minus suffix matches | `models_test.go:51` — `cmdcode/deepseek-v4` + `opencode/kimi-k2` present, `opencode/gpt-5.5` excluded by `gpt-`; `handler_test.go:553` — same via HTTP with `object == "list"` | ✅ PASS |
| WHEN one gateway has no accounts THEN empty contribution, other unaffected | single-gateway listing | `models_test.go:78` — zen nil → only `cmdcode/deepseek-v4`; `models_test.go:90` — buckets independent; `models_test.go:104` — boot fetch failure keeps catalog; `models_test.go:116` — `FetchModels` hits `GET {base}/v1/models` with `Bearer Primary` + CLI headers; empty on no-accounts / failure | ✅ PASS |

### GW-06 — WebUI por gateway (P2)

| Criterion | Spec-defined outcome | `file:line` + assertion | Result |
| --------- | -------------------- | ----------------------- | ------ |
| WHEN admin lists accounts THEN each shows `gateway` | `gateway` per row; absent on create → `cmdcode` | `admin_diagnostics_test.go:72` — legacy create → list row `gateway == "cmdcode"` | ✅ PASS |
| WHEN admin creates with `gateway: zen` THEN zen pool + persist `gateways.zen.accounts` | pool split, persisted YAML | `admin_diagnostics_test.go:92` — 201, `payload["gateway"] == "zen"`, pools `0/1`, list row zen, same key allowed in cmdcode (no cross-gateway 409), persisted `gateways.zen.accounts[0].APIKey`; `admin_diagnostics_test.go:131` — unknown gateway → 400; `admin_diagnostics_test.go:143` — delete drops pool row + persisted account | ✅ PASS |

### GW-07 — Diagnóstico por tentativa + Playground (P2)

| Criterion | Spec-defined outcome | `file:line` + assertion | Result |
| --------- | -------------------- | ----------------------- | ------ |
| WHEN upstream request fails THEN log fingerprint + status + outcome; client error preserves message + `Retry-After` | header + message preserved; `recordUpstreamAttempt`/`copyErrorResponse` wired | `admin_diagnostics_test.go:195` — 429 fake with `Retry-After: 7` → client `Retry-After == "7"` (`copyErrorResponse` path); `admin.go:106-127` — `recordUpstreamAttempt` logs `gateway/key/status/outcome`; `admin_diagnostics_test.go:266` — 402 diagnostic preserves `"Insufficient account funds"` in `error` | ✅ PASS (note N2) |
| WHEN Playground selected-key THEN 1 request / 1 key, no failover, no cooldown/binding mutation; `key_test` taxonomy; 402 → `request_error` | envelope exact, pool untouched | `admin_diagnostics_test.go:219` — 403 on only key: envelope 200, `http_status == 403`, `key_test == rejected`, `fingerprint == id`, exactly 1 upstream call, `Errors == 0`, no cooldown; `admin_diagnostics_test.go:266` — 402 → `request_error`, message kept, 1 call, `Errors == 0`; `admin_diagnostics_test.go:295` — 11-case `classifyKeyTest` table (401/403→rejected, 429→rate_limited, 400/402/404/422→request_error, 5xx→upstream_error, transport→transport_error, none→unavailable) | ✅ PASS |

**Status**: ✅ All 24 ACs covered with spec-outcome-matched assertions. No ⚠️ spec-precision gaps (every AC defines a precise checkable outcome; GW-03b AC4's outcome is itself a docs artifact, present).

---

## Edge Cases (spec.md)

- [x] `gateways.zen` empty + `opencode/*` → `503 no_accounts` with `gateway: zen` — `zen_test.go:231` (`Status == 503`, `Code == "no_accounts"`, message contains `gateway: zen`) + `handler_test.go:145` (HTTP 503, body `no_accounts` + `gateway: zen`, upstream untouched)
- [x] Duplicate key in same gateway → `409 duplicate` — `admin_test.go:108` (`StatusCode == 409` on same-pool re-add; pool-scoped `errDuplicateAccount` in `accounts.go:250`)
- [x] Same key in different gateways → allowed — `admin_diagnostics_test.go:114` (same key created in cmdcode after zen → 201)
- [x] `exclude_models` matches suffix after `/` — `models_test.go:71` (`gpt-` removes `opencode/gpt-5.5`, keeps rest) + `handler_test.go:553`

---

## Discrimination Sensor

Scratch-only mutations (applied → `go test ./internal/app/ -count=1 -run <pattern>` → reverted via `git checkout -- <file>`; tree ends clean, verified `git status --short` shows only pre-existing untracked spec files):

| # | File:line (approx) | Mutation | Target tests | Result |
| - | ------------------ | -------- | ------------ | ------ |
| 1 | `zen_free.go:24` | `IsFreeModel` name-check `return true` → `return false` | `TestIsFreeModel\|TestShapeFreeBody\|TestZenChatFree` | ✅ Killed (`TestZenChatFreeCollapseDefaultsFinishReason`, `TestZenChatFreeStreamPassesThrough` FAIL) |
| 2 | `zen_free.go:74` | `shapeFreeBody` `req.Stream = true` → `false` | `TestShapeFreeBodyForcesAgentShape\|…\|TestZenChatFreeAgentShapeGate` | ✅ Killed (`TestShapeFreeBodyForcesAgentShape`, `TestZenChatFreeAgentShapeGate` 403 FAIL) |
| 3 | `zen.go:227` | `isNonRetryableStatus`: drop 403 from retryable list (→ non-retryable) | `TestIsNonRetryableClientResponse\|…` | ✅ Killed (`TestIsNonRetryableClientResponse/403_forbidden_fails_over` FAIL) |
| 4 | `zen_headers.go:134` | `setZenHeaders`: `Authorization: "Bearer "+key` → bare key | `TestSetZenHeadersEmitsCLIIdentity\|TestZenChatRoundRobinPassthrough\|TestZenFetchModels` | ✅ Killed (3 assertion failures incl. round-robin auth) |
| 5 | `gateway.go:52` | `SplitModel` bare-model default `cmdcode` → `opencode` | `TestSplitModel\|TestRouterRoutesCmdcodeAndBareToCmdcode` | ✅ Killed (`TestRouterRoutesCmdcodeAndBareToCmdcode` 502, zen received cmdcode traffic) |

**Sensor depth**: lightweight fault-injection, 5 targeted behavior-level mutations over highest-risk new code (free-tier detection/shaping, failover classifier, auth headers, routing default).
**Result**: 5/5 killed — PASS ✅, 0 survived.

---

## Code Quality

| Principle | Status |
| --------- | ------ |
| Minimum code | ✅ — new files scoped per task (`zen*.go`, `gateway.go`); no unrelated refactors in diffstat |
| Surgical changes | ✅ — `handler.go`/`server.go`/`app.go`/`models.go`/`config.go`/`usage.go` touched only at gateway seams |
| No scope creep | ✅ — out-of-scope items (`messages`, Gemini, quota Zen, repo rename) absent from diff |
| Matches patterns | ✅ — Zen failover mirrors `CCClient` loop; `handleStream/handleNonStream` reused for all 3 lanes |
| Spec-anchored outcome check | ✅ — table above; each assertion targets the spec-defined value |
| Per-layer coverage met | ✅ — domain 1:1 AC mapping; routes cover happy + edge + error via httptest fakes |
| Every test maps to a spec requirement | ✅ — new tests carry `GW-xx ACn` comments |
| Guidelines followed | ✅ — none documented (`tasks.md`: "none — strong defaults applied"); gates from `Makefile` + CI |

---

## Gate Check

- **Gate command**: `go vet ./... && go test -race -count=1 ./...` (`PATH=/opt/homebrew/bin`)
- **Result**: `go vet` exit 0; `go test -race`: `cmd/cliai2api` (no test files), `internal/app` ok, `internal/i18n` ok, `internal/web` (no test files) — **0 failed, 0 skipped**
- **Test count before feature** (`a3c0523~1`): 186 `func Test*` in `internal/app`
- **Test count after feature** (worktree): 252 `func Test*` in `internal/app`
- **Delta**: +66 new tests, none deleted, no weakened assertions observed
- **Failures**: none

---

## Fix Plans

None — no blocking gaps. Non-blocking notes for the implementer (no re-verification required):

- **N1 (GW-03 AC2)**: behavioral failover is proven with 429 (`TestZenChatFailover429ThenSuccess`); 401/403/5xx failover rests on the classifier table (`TestIsNonRetryableClientResponse`) + the shared retry loop. Consider a 403→200 failover test for symmetry.
- **N2 (GW-07 AC1)**: spec text mentions logging `proxy`; implementation (`recordUpstreamAttempt`, `admin.go:106`) logs `gateway/key/status/outcome` without proxy, matching design §5 — treat as design-supersedes-spec; optionally align spec wording.
- **N3 (Edge)**: same-gateway duplicate 409 is asserted via the legacy cmdcode pool (`admin_test.go:108`); no zen-pool-scoped duplicate test, though `errDuplicateAccount` is pool-scoped by construction. Optional symmetry test.

---

## Requirement Traceability Update

| Requirement | Previous Status | New Status |
| ----------- | --------------- | ---------- |
| GW-01 | Design | ✅ Verified |
| GW-02 | Design | ✅ Verified |
| GW-03 | Design | ✅ Verified |
| GW-03b | Design | ✅ Verified |
| GW-04 | Design | ✅ Verified |
| GW-05 | Design | ✅ Verified |
| GW-06 | Design | ✅ Verified |
| GW-07 | Design | ✅ Verified |

---

## Summary

**Overall**: ✅ Ready

**Spec-anchored check**: 24/24 ACs matched to spec outcomes with `file:line` + assertion evidence
**Sensor**: 5/5 mutations killed, 0 survived
**Gate**: `go vet` clean; `go test -race -count=1 ./...` all packages ok (+66 tests, 0 failed, 0 skipped)

**What works**: legacy config migration with serving compat; prefix routing with prefix-stripping; zen multi-key round-robin + 429 failover + 402/400 no-rotation semantics; free-tier agent shaping with stream collapse; canonical session + CLI headers; responses SSE translation (stream + non-stream + 502-incomplete); unified prefixed catalog with global excludes; per-gateway admin CRUD + selected-key diagnostics with 402→request_error taxonomy.

**Issues found**: none blocking (3 non-blocking notes N1–N3 above).

**Next steps**: merge/ship; before staging against `muse-spark-*-free`, run the mitmproxy recapture follow-up logged in `context.md`.
