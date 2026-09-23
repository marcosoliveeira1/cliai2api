# cliai2api-gateways Tasks

## Execution Protocol (MANDATORY -- do not skip)

Implement these tasks with the `tlc-spec-driven` skill: **activate it by name and follow its Execute flow and Critical Rules.** Do not search for skill files by filesystem path. The skill is the source of truth for the full flow (per-task cycle, sub-agent delegation, adequacy review, Verifier, discrimination sensor).

**If the skill cannot be activated, STOP and tell the user — do not proceed without it.**

---

**Design**: `.specs/features/cliai2api-gateways/design.md`
**Status**: Draft (aguardando aprovação)

---

## Test Coverage Matrix

> Generated from codebase, project guidelines, and spec — confirm before Execute. Guidelines found: none — strong defaults applied (`Makefile` + `.github/workflows/ci.yml` são a única fonte de gates; sem `AGENTS.md`/`CLAUDE.md`/`CONTRIBUTING.md`).

| Code Layer | Required Test Type | Coverage Expectation | Location Pattern | Run Command |
| ---------- | ------------------ | -------------------- | ---------------- | ----------- |
| Domain / business-logic (gateway, router, zen translate, config migration, usage) | unit | All branches; 1:1 to spec ACs; every listed edge case has a test | `internal/app/*_test.go` (co-locado) | `go test ./...` |
| Route / handler (`/v1/chat/completions`, `/v1/models`, admin accounts) | integration (httptest fakes) | All routes in scope: happy + edge + error paths | `internal/app/*_test.go` (co-locado, `net/http/httptest`) | `go test -race -count=1 ./...` |
| Entity / config / schema (YAML structs) | none | — (build gate only) | — | build gate only |

## Parallelism Assessment

> Generated from codebase — confirm before Execute.

| Test Type | Parallel-Safe? | Isolation Model | Evidence |
| --------- | -------------- | --------------- | -------- |
| unit / integration (Go) | No | Globais compartilhados mutados por teste: `modelCatalog`, `configFile`, `usageFile`; nenhum teste usa `t.Parallel` | `internal/app/models_admin_test.go:10`, `admin_test.go:22-27`, `accounts_test.go:323-324`; `grep t.Parallel` vazio |

Consequência: nenhuma task leva flag `[P]`; execução e testes sempre sequenciais.

## Gate Check Commands

> Generated from codebase — confirm before Execute. Fonte: `Makefile` + `.github/workflows/ci.yml`.

| Gate Level | When to Use | Command |
| ---------- | ----------- | ------- |
| Quick | Após tasks só com unit tests | `go test ./internal/app/ -count=1` |
| Full | Após tasks com handler/admin tests | `go vet ./... && go test -race -count=1 ./...` |
| Build | Após rename/config-only ou fim de fase | `go build ./... && go vet ./... && go test -count=1 ./...` |

> Ambiente: toolchain Go **ausente** neste shell (`go: command not found`). Gates rápidos devem rodar onde houver Go (CI executa `vet` + `test -race -count=1`); nenhuma task é "done" sem o gate correspondente verde.

---

## Execution Plan

3 fases, tudo sequencial (testes não são parallel-safe → sem `[P]`, sem sub-agents; execução inline).

```
Phase 1 (Foundation):  T1 → T2 → T3
Phase 2 (Zen):         T4 → T5 → T5b → T6 → T7
Phase 3 (Integration): T8 → T9 → T10
```

---

## Task Breakdown

### T1: Gateway interface + Registry + SplitModel

**What**: Criar `internal/app/gateway.go` com interface `Gateway`, `Registry` e `SplitModel`.
**Where**: `internal/app/gateway.go` (novo) + `internal/app/gateway_test.go` (novo)
**Depends on**: None
**Reuses**: `internal/app/accounts.go` (`AccountPool`), `internal/app/config.go` (`AccountConfig`)
**Requirement**: GW-01, GW-02

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `Gateway` define `Name/ModelPrefix/Pool/BaseURL/SetBaseURL/Chat/FetchModels`
- [ ] `SplitModel("opencode/gpt-5.5")` → `("opencode","gpt-5.5")`; `"deepseek/x"` → `("cmdcode","deepseek/x")`; `"foo/bar"` → erro de prefixo desconhecido
- [ ] Gate passa: `go test ./internal/app/ -count=1`

**Tests**: unit (tabela de prefixos: com/sem prefixo, desconhecido, vazio)
**Gate**: quick

**Commit**: `feat(gateway): add Gateway interface, registry and model prefix split`

---

### T2: Config `gateways.*` + migração legada

**What**: Estender `Config` com `gateways` (cmdcode+zen) migrando `commandcode.*` no load.
**Where**: `internal/app/config.go` (modify) + `internal/app/config_test.go` (extend)
**Depends on**: T1
**Reuses**: `loadConfig/saveConfig/writeConfigTemplate` existentes
**Requirement**: GW-01

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `loadConfig` com YAML legado popula `gateways.cmdcode` (accounts + base_url + api_key legada) sem perda
- [ ] `saveConfig` persiste `gateways:` e limpa campos legados quando migrou
- [ ] `defaultConfig` gera formato novo com `gateways.cmdcode.base_url=https://api.commandcode.ai` e `gateways.zen.base_url=https://opencode.ai/zen`
- [ ] Gate passa: `go test ./internal/app/ -count=1`

**Tests**: unit (legado→novo, novo round-trip, defaults)
**Gate**: quick

**Commit**: `feat(config): add gateways section with legacy commandcode migration`

---

### T3: Usage namespaced por gateway

**What**: Chavear contadores por `gateway:accountID` com migração lazy de entradas antigas (= cmdcode).
**Where**: `internal/app/usage.go` (modify) + `internal/app/usage_namespaces_test.go` (novo; `usage.go` hoje sem teste próprio)
**Depends on**: T2
**Reuses**: `UsageTracker.counter/Recorder/MoveAccount`, `accountID()`
**Requirement**: GW-01

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `Recorder("zen", accountID, ...)` e `Recorder("cmdcode", mesmoID, ...)` acumulam em contadores independentes
- [ ] Snapshot de `usage.json` antigo (IDs sem `:`) é lido como `cmdcode:` sem duplicar nem perder valores
- [ ] Gate passa: `go test ./internal/app/ -count=1`

**Tests**: unit (isolamento por gateway, migração lazy, MoveAccount namespaced)
**Gate**: quick

**Commit**: `feat(usage): namespace usage counters by gateway with lazy migration`

---

### T4: Headers CLI-only + sessão canônica do Zen

**What**: Criar `zen_headers.go` com `setZenHeaders` + geração de IDs `ses_/msg_` + `projectID` no formato canônico exigido pelo free-tier.
**Where**: `internal/app/zen_headers.go` (novo) + `internal/app/zen_headers_test.go` (novo)
**Depends on**: T1
**Reuses**: Algoritmo de `kode-ai/providers/opencode/headers.go` (referência documentada no context.md); formato validado no `.spec/issues.md` §2
**Requirement**: GW-03, GW-03b

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] Headers emitidos: `Authorization: Bearer`, `x-opencode-client: cli`, `x-opencode-project/session/request/parent`, `x-session-affinity`, `X-Session-Id` (mesmo valor da sessão), `prompt_cache_key`, `User-Agent: opencode/<ver>`
- [ ] Sessão no formato canônico `ses_<12hex><14base62>` (free-tier rejeita outro shape com 403 desde 2026-09-16); derivação de `x-opencode-session/x-session-affinity/X-Session-Id/conversation-id/...` senão primeira mensagem user; `client` default `cli` via `OPENCODE_CLIENT`
- [ ] Recon: captura (mitmproxy ou log `debug`) contra CLI `opencode 1.18.31` confirma forma dos headers, ou divergência registrada em `context.md` como suposição assinada
- [ ] Gate passa: `go test ./internal/app/ -count=1`

**Tests**: unit (formato `ses_…` via regex, unicidade, defaults, env override, derivação de headers inbound)
**Gate**: quick

**Commit**: `feat(zen): add CLI-compatible request headers with canonical session`

---

### T5: ZenGateway chat/completions + failover

**What**: Implementar `ZenClient` com pool próprio, passthrough OpenAI 1:1 e failover com semântica 4xx do referência.
**Where**: `internal/app/zen.go` (novo) + `internal/app/zen_test.go` (novo)
**Depends on**: T4 (headers), T1 (interface)
**Reuses**: `AccountPool` (Acquire/RecordSuccess/RecordFailure); semântica `isNonRetryableClientResponse` de `.spec/issues.md` §5 (substitui `shouldFailover` de `cc.go:261` no caminho Zen)
**Requirement**: GW-03

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `POST {base}/v1/chat/completions` repassa body OpenAI intacto + headers T4; round-robin entre N keys
- [ ] 401/403/429/5xx faz failover pré-primeiro-byte; 429 aplica cooldown (`Retry-After`, default 60s); 4xx fora de 401/403/429 (incl. 400/402/404/422) encerra sem rotacionar nem esfriar a key (402 até limpa falha)
- [ ] Zero keys → erro `503 no_accounts` com `gateway: zen`
- [ ] Gate passa: `go test -race -count=1 ./...` (fakes `httptest`: 429→200, 402 sem retry/cooldown, 400 sem retry, pool vazio)

**Tests**: integration (httptest fakes: rotação, failover 429→200, 402 sem retry, 400 sem retry, 503 sem contas)
**Gate**: full

**Commit**: `feat(zen): add ZenGateway chat passthrough with multi-key failover`

---

### T5b: Free-tier shaping + CollapseStream (P0 do issues §1)

**What**: Reescrever body de models free-tier para agent-shape antes do upstream e colapsar SSE → JSON quando o cliente pediu `stream:false`.
**Where**: `internal/app/zen_free.go` (novo: `IsFreeModel/shapeFreeBody/collapseStream`) + `internal/app/zen_free_test.go` (novo)
**Depends on**: T5 (usa o caminho `Chat` do ZenGateway)
**Reuses**: Lógica de `prepareAnonymousBody/shapeKeyBody/CollapseStream` documentada no `.spec/issues.md` §1; `handleNonStream` (`handler.go`) para saída JSON
**Requirement**: GW-03b

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `IsFreeModel(id)`: nome contém `free` (case-insensitive); stub de pricing metadata pronto para custo-zero (sem `models.dev` no MVP)
- [ ] `shapeFreeBody`: força `stream:true`, injeta tools ausentes `bash/edit/glob/grep/read` com definições mínimas, `stream_options.include_usage:true` para chat; não-free passa intacto
- [ ] `stream:false` do cliente + shaping aplicado → colapsa SSE upstream em `chat.completion` com `finish_reason` válido
- [ ] Gate passa: `go test -race -count=1 ./...` (fake que dá 403 sem tools e 200 com tools; assert 200 + body do fake com `stream:true` + 5 tools)

**Tests**: unit (IsFreeModel, shaping idempotente, não-free intacto) + integration (fake agent-shape gate, stream:false colapsado)
**Gate**: full

**Commit**: `feat(zen): shape free-tier requests to agent form with stream collapse`

---

### T6: Tradução Zen responses → OpenAI

**What**: Classificar família por model ID e traduzir SSE `responses` para chunks OpenAI.
**Where**: `internal/app/zen_responses.go` (novo) + `internal/app/zen_responses_test.go` (novo)
**Depends on**: T5b
**Reuses**: `handleStream/handleNonStream` (`handler.go`), `parseStreamEvents`, `resolveFinishReason`; shaping T5b aplica-se antes da tradução quando `IsFreeModel`
**Requirement**: GW-04

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `opencode/gpt-5.5` (responses) com `stream:true` emite `delta.content` + `finish_reason` + `data: [DONE]`; `stream:false` agrega `chat.completion`
- [ ] Upstream que fecha sem finish → `502 upstream_stream_incomplete`
- [ ] Família desconhecida cai em chat passthrough com `[WARN]` logado
- [ ] Gate passa: `go test -race -count=1 ./...`

**Tests**: integration (fake SSE responses: stream, non-stream, fechamento sem finish)
**Gate**: full

**Commit**: `feat(zen): translate responses SSE to OpenAI chunks`

---

### T7: Catálogo por gateway + `/v1/models` unificado

**What**: Tirar `modelCatalog` global; catálogo por gateway; `GET /v1/models` une com prefixo.
**Where**: `internal/app/models.go` (modify) + `internal/app/zen.go` (`FetchModels`) + tests (extend `models_test.go`, `models_admin_test.go`)
**Depends on**: T5
**Reuses**: `isModelExcluded` (sufixo após `/`), `FetchProviderModels`
**Requirement**: GW-05

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `GET /v1/models` retorna `cmdcode/<id>` + `opencode/<id>`; `exclude_models: ["gpt-"]` remove `opencode/gpt-5.5` e mantém o resto
- [ ] Gateway sem contas contribui vazio sem falhar a lista; falha de fetch no boot só gera `[WARN]`
- [ ] Nenhum teste legado enfraquecido (fixtures de `modelCatalog` global migradas para o novo formato)
- [ ] Gate passa: `go test -race -count=1 ./...`

**Tests**: integration (união, filtro global, gateway vazio, fetch com falha)
**Gate**: full

**Commit**: `feat(models): unify per-gateway catalogs with prefixed IDs`

---

### T8: Router no chat handler + runServer por registry

**What**: `handleChatCompletions` delega via `SplitModel`; `runServer` recebe `Registry`; erros 404/503 mapeados.
**Where**: `internal/app/handler.go`, `server.go`, `app.go` (modify) + `internal/app/handler_test.go` (extend)
**Depends on**: T3, T6, T7
**Reuses**: `handleStream/handleNonStream`, middlewares, `QuotaService` (só CCGateway)
**Requirement**: GW-02, GW-03, GW-03b, GW-04

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `opencode/<id>` → Zen (prefixo removido no upstream, shaping T5b quando free); `cmdcode/<id>` e sem prefixo → cmdcode; `foo/bar` → `404`; gateway sem contas → `503` com `gateway:` no corpo
- [ ] `usage.save()` e `RecorderFor` usam account namespaced; logs incluem `gateway=`
- [ ] Gate passa: `go vet ./... && go test -race -count=1 ./...` (dois upstreams fake, assert 1 request cada com model sem prefixo)

**Tests**: integration (roteamento 3 casos + erros 404/503, upstream fake por gateway)
**Gate**: full

**Commit**: `feat(router): route chat completions by model prefix via registry`

---

### T9: Admin com dimensão gateway + diagnóstico por tentativa (P2)

**What**: CRUD de contas com campo `gateway`; WebUI exibe coluna `gateway`; log por tentativa com fingerprint + `copyErrorResponse`; Playground selected-key sem mutar pool.
**Where**: `internal/app/admin.go` (modify) + `internal/web/index.html` (coluna+select) + `internal/app/admin_test.go` (extend)
**Depends on**: T8
**Reuses**: `AccountPool` por gateway, `MoveAccount/DropAccount` namespaced
**Requirement**: GW-06, GW-07

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `GET /admin/api/accounts` inclui `gateway` por conta; `POST` com `{"gateway":"zen",...}` cai no pool zen e persiste em `gateways.zen.accounts`; ausente = `cmdcode` (retrocompat)
- [ ] WebUI lista `gateway` e permite escolher no create
- [ ] Falha upstream loga key fingerprint + status + `outcome` por tentativa; erro ao cliente preserva message + `Retry-After` do upstream
- [ ] Playground selected-key: 1 request, 1 key, sem failover, sem mutar cooldown/binding; `key_test` com 402 → `request_error` (documentado)
- [ ] Gate passa: `go test -race -count=1 ./...`

**Tests**: integration (create/list por gateway, default cmdcode, delete limpa usage namespaced, selected-key sem cooldown, 402 → request_error)
**Gate**: full

**Commit**: `feat(admin): scope accounts API and WebUI by gateway with key diagnostics`

---

### T10: Rename binário + docs + memória do projeto

**What**: `cmd/cmdcode2api` → `cmd/cliai2api`, `Makefile`/CI/build, README e `STATE.md` com ADs.
**Where**: `cmd/cliai2api/`, `Makefile`, `.github/workflows/ci.yml`, `README.md`, `.specs/STATE.md` (novo)
**Depends on**: T9
**Reuses**: `app.go` (strings de ajuda/nome)
**Requirement**: GW-01

**Tools**:

- MCP: NONE
- Skill: NONE

**Done when**:

- [ ] `go build ./...` gera `cliai2api`; `cliai2api --version` imprime novo nome; CI compila o novo path
- [ ] README documenta gateways, prefixos, multi-key zen e migração
- [ ] `.specs/STATE.md` criado com AD-001 (prefixo como contrato), AD-002 (`AccountPool`-por-gateway), AD-003 (namespace `gateway:id`), e status das requirements
- [ ] Gate passa: `go build ./... && go vet ./... && go test -count=1 ./...`

**Tests**: none (build gate only)
**Gate**: build

**Commit**: `chore(rename): ship cliai2api binary with docs and project memory`

---

## Parallel Execution Map

Sem `[P]`: testes não são parallel-safe (globais compartilhados), logo todas as tasks são sequenciais.

```
Phase 1 (Foundation, Sequential):
  T1 ──→ T2 ──→ T3

Phase 2 (Zen, Sequential):
  T3 ──→ T4 ──→ T5 ──→ T5b ──→ T6
  T5 ──→ T7

Phase 3 (Integration, Sequential):
  T3,T6,T7 ──→ T8 ──→ T9 ──→ T10
```

## Task Granularity Check

| Task | Scope | Status |
| ---- | ----- | ------ |
| T1: Gateway interface + SplitModel | 1 arquivo novo + teste | ✅ Granular |
| T2: Config gateways + migração | 1 arquivo modificado + teste | ✅ Granular |
| T3: Usage namespace | 1 arquivo modificado + teste | ✅ Granular |
| T4: Zen headers | 1 arquivo novo + teste | ✅ Granular |
| T5: ZenGateway chat + failover | 1 arquivo novo + teste | ✅ Granular |
| T5b: Free-tier shaping + collapse | 1 arquivo novo + teste | ✅ Granular |
| T6: Responses translate | 1 arquivo novo + teste | ✅ Granular |
| T7: Catálogo unificado | 1 arquivo modificado + teste | ✅ Granular |
| T8: Router + runServer | handler+server+wiring, coeso | ⚠️ OK (2 arquivos, um conceito: roteamento) |
| T9: Admin por gateway | admin+WebUI, um conceito | ⚠️ OK (API+UI da mesma dimensão) |
| T10: Rename + docs + STATE | sem código de domínio | ✅ Granular (build gate) |

## Diagram-Definition Cross-Check

| Task | Depends On (task body) | Diagram Shows | Status |
| ---- | ---------------------- | ------------- | ------ |
| T1 | None | início Phase 1 | ✅ Match |
| T2 | T1 | T1 → T2 | ✅ Match |
| T3 | T2 | T2 → T3 | ✅ Match |
| T4 | T1 | T3 → T4 (T1 já completo; seta via Phase 1) | ✅ Match |
| T5 | T4, T1 | T4 → T5 | ✅ Match |
| T5b | T5 | T5 → T5b | ✅ Match |
| T6 | T5b | T5b → T6 | ✅ Match |
| T7 | T5 | T5 → T7 | ✅ Match |
| T8 | T3, T6, T7 | T3,T6,T7 → T8 | ✅ Match |
| T9 | T8 | T8 → T9 | ✅ Match |
| T10 | T9 | T9 → T10 | ✅ Match |

## Test Co-location Validation

| Task | Code Layer Created/Modified | Matrix Requires | Task Says | Status |
| ---- | --------------------------- | --------------- | --------- | ------ |
| T1 gateway.go | Domain | unit | unit | ✅ OK |
| T2 config.go | Domain | unit | unit | ✅ OK |
| T3 usage.go | Domain | unit | unit | ✅ OK |
| T4 zen_headers.go | Domain | unit | unit | ✅ OK |
| T5 zen.go | Domain + handler path | unit + integration | integration (fakes; cobre ACs+edges) | ✅ OK |
| T5b zen_free.go | Domain + handler path | unit + integration | unit + integration | ✅ OK |
| T6 zen_responses.go | Domain + handler path | unit + integration | integration | ✅ OK |
| T7 models.go/handler | Domain + route | unit + integration | integration | ✅ OK |
| T8 handler/server | Route | integration | integration | ✅ OK |
| T9 admin/WebUI | Route | integration | integration | ✅ OK |
| T10 rename/docs | Entity/config | none | none | ✅ OK |

**Requirement coverage:** 8 total, 8 mapped (GW-01: T1,T2,T3,T10; GW-02: T1,T8; GW-03: T4,T5,T8; GW-03b: T4,T5b,T8; GW-04: T6,T8; GW-05: T7; GW-06: T9; GW-07: T9), 0 unmapped.
