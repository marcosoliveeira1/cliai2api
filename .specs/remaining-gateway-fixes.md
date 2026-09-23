# Remaining Gateway Fixes

## Status

The corrective commits through `92f0ac5` pass:

```text
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Do not deploy this revision as a complete gateway fix until the P1 items below
are resolved and independently verified.

## P1: Stream Zen Responses Incrementally

- [ ] Replace the buffered implementation in
  `internal/app/zen_responses.go:translateZenResponsesStream`.
- Current behavior calls `parseZenResponsesEvents`, which consumes the entire
  upstream response before returning any OpenAI SSE chunk.
- `handleOpenAIResponse` must flush each relayed SSE chunk when the response
  writer supports `http.Flusher`.
- Verification: an upstream fake that delays its second Responses SSE event
  must let the client receive the first translated OpenAI chunk before that
  second event is released; the output must still end with a finish chunk and
  `data: [DONE]`.

## P1: Serialize Account, Config, And Catalog Lifecycle Updates

- [ ] Prevent concurrent admin mutations from persisting stale snapshots over
  newer `config.yaml` account changes.
- Current path: `persistGatewayPools` snapshots both pools before `saveConfig`
  writes the file. Concurrent add, rename, enable, key-change, or delete
  requests can write an older snapshot last.
- [ ] Prevent a catalog fetch started before the last account is disabled or
  deleted from restoring that gateway's stale catalog after the clear.
- Verification: concurrent mutation tests under `go test -race` must prove
  that the final config and catalog match the final pool state.

## P2: Preserve Usage For In-Flight Key Changes

- [ ] Decide and implement the intended accounting behavior when an account
  key changes during an in-flight request.
- Current behavior moves existing usage to the new ID before the request's
  response handler records usage against the old ID, recreating a retired
  counter.
- Verification: start a request, rotate its key before completion, finish the
  request, then assert no retired account usage key remains in `usage.json`.

## P2: Prevent Stale Quota Writes

- [ ] Ensure an in-flight Command Code quota refresh cannot recreate quota
  state for an account that was deleted or had its key replaced.
- Verification: block a quota refresh, delete or rotate the account, release
  the refresh, and assert the retired account has no quota snapshot.

## Final Gate

- [ ] Run `go build ./... && go vet ./... && go test -race -count=1 ./...`.
- [ ] Run a fresh independent review focused on the items above.
- [ ] Deploy only after the review reports no remaining P1 findings.
