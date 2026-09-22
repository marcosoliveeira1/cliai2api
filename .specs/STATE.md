# STATE

## Decisions

### AD-001
- **Decision**: Model prefix (`cmdcode/`, `opencode/`) is the routing contract; no new endpoint, header, or query override.
- **Reason**: Prefix travels inside the existing OpenAI `model` field, so clients need no new config and routing stays testable per request.
- **Trade-off**: Model IDs grow a gateway qualifier; clients must use prefixed IDs from `/v1/models`.
- **Scope**: cliai2api-gateways (handler routing, models catalog, admin errors).
- **Date**: 2026-09-22
- **Status**: active

### AD-002
- **Decision**: One `AccountPool` instance per gateway (`cmdcode`, `zen`); Zen reuses the cmdcode failover/cooldown semantics.
- **Reason**: Pool reuse gives round-robin + 429 cooldown + pre-first-byte failover for free, keeps pools independent, and bounds the change to wiring.
- **Trade-off**: Zen inherits pool-coupled behavior (cooldown windows, `MarkSuccess` on 402); divergence needs explicit overrides.
- **Scope**: cliai2api-gateways (accounts, zen client, admin CRUD, quota).
- **Date**: 2026-09-22
- **Status**: active

### AD-003
- **Decision**: `usage.json` counters are namespaced `gateway:accountID` with lazy migration (entries without `:` read as `cmdcode:`).
- **Reason**: Same key may exist in both gateways; namespacing prevents counter collision without a destructive migration.
- **Trade-off**: Old tooling reading raw `usage.json` IDs sees new `gateway:` keys after upgrade.
- **Scope**: cliai2api-gateways (usage tracking, admin delete/move, `/usage` output).
- **Date**: 2026-09-22
- **Status**: active

## Handoff

- **Feature**: cliai2api-gateways / `.specs/features/cliai2api-gateways/`
- **Phase / Task**: Phase 3 / T10 — Rename binário + docs + memória do projeto
- **Completed**: T1, T2, T3, T4, T5, T5b, T6, T7, T8, T9, T10
- **In-progress**: none
- **Next step**: none — feature complete; run staging validation per spec success criteria.
- **Blockers**: none
- **Uncommitted files**: none
- **Branch**: main

## Requirements

| Requirement ID | Story | Status |
| -------------- | ----- | ------ |
| GW-01 | Abstração + migração (incl. binário `cliai2api`) | done (T1, T2, T3, T10) |
| GW-02 | Roteamento por prefixo | done (T1, T8) |
| GW-03 | Zen chat multi-key | done (T4, T5, T8) |
| GW-03b | Zen free-tier shaping + sessão canônica | done (T4, T5b, T8) |
| GW-04 | Zen responses SSE | done (T6, T8) |
| GW-05 | Catálogo unificado | done (T7) |
| GW-06 | WebUI por gateway | done (T9) |
| GW-07 | Diagnóstico por tentativa + Playground selected-key | done (T9) |
