# Diff: opencode-zen2api (nosso) vs jasonxu114514/opencode2api (referência)

Data: 2026-09-22. Base: clone `--depth 1` de `github.com/jasonxu114514/opencode2api`
em `/var/folders/.../T/opencode/opencode2api` vs `opencode-zen2api` em produção
(`api-opencode.marcosjr.site`). Análise estática do código, sem execução do referência.

Contexto do incidente que motivou: upstream devolve **403** para
`muse-spark-1.3-contributor-free` nas 3 contas (confirmado via `--debug`:
`retryable status=403` x3 → `503 all_cooling`) e **402 `Insufficient account
funds`** intermitente para outros modelos (round-robin entre contas com/sem saldo).

## 1. Gap principal — free-tier shaping (provável causa do 403)

- Nós: `internal/app/proxy.go:forwardOnce()` — passthrough byte-a-byte. Só valida
  `messages` não-vazio (`serveChat`, `chatRequest`). Nenhuma normalização.
- Deles (`internal/gateway/upstream.go`):
  - `prepareAnonymousBody()` — força `stream:true` + injeta core tools
    `bash/edit/glob/grep/read` (`anonymousCoreTools`) + `stream_options.include_usage`.
    Comentário explícito: free tier rejeita request não-agent com `403 FreeTierError`.
  - `shapeKeyBody()` — aplica o **mesmo shaping em key tiers** quando
    `catalog.IsFreeModel()` (free por nome *ou* pricing metadata).
  - `CollapseStream()` (`internal/protocol/collapse.go`) — a lane free só serve
    streaming; o gateway colapsa SSE de volta para JSON quando o cliente pediu
    `stream:false`.
- Nós não fazemos nada disso. Se o upstream passou a exigir agent-shape para
  `*-free` (o que o código deles afirma), **todo request nosso para `-free` falha
  com 403 em todas as contas por construção** — consistente com o observado
  (`glm-5`, não-free, passa; `muse-spark-*-free` falha).

## 2. Sessão/affinity (segunda causa provável do 403)

- Deles (`internal/identity/request.go:CanonicalSessionID()`): sessão no formato
  `ses_+12hex+14base62`; comentário: "Since 2026-09-16 the Zen free tier (Bearer
  public) rejects any other session shape with 403 FreeTierError".
  `DeriveRequestIDs()` deriva de `x-opencode-session/x-session-affinity/X-Session-Id/
  conversation-id/...` senão primeira mensagem user; `newUpstreamRequest()` sempre
  envia `x-opencode-client: cli`, `x-opencode-session`, `x-session-affinity`,
  `X-Session-Id`, `x-opencode-request/project/parent`, `prompt_cache_key`.
- Nós: `grep session|affinity|x-session` no `internal/` retorna zero. Nenhum header
  de sessão; `Store.NextAccount()` (`internal/app/config.go`) é round-robin puro,
  sem sticky session. Quebra cache de prompt e pode tomar 403 free-tier.

## 3. Transformação de request (maior gap funcional geral)

Deles (`internal/protocol/`), sem equivalente nosso:
- `request.go:PrepareRequest()` → `bridge.go:ConvertRequest()` via `bridgeRequest`
  intermediário (system/developer/messages/tools/tool_choice/response_format/seed/cache-keys).
- `normalizeChatToolReasoningHistory()` — injeta `reasoning_content` em turns
  assistant com `tool_calls` (vendors rejeitam sem isso).
- `normalizeAnthropicToolThinkingHistory()` — remove `signature`, troca
  `redacted_thinking`, preenche thinking vazio (só para
  `reasoningVendorHints = moonshot/kimi/deepseek/mimo`).
- `stripStaleReasoningInputs()` + `isStaleReasoningReference()` (`gateway/upstream.go`)
  — em 400 com "reasoning item … not found/expired", remove `previous_response_id` +
  itens `reasoning` e faz **um replay**.
- `responsesReasoning()/anthropicThinking()/reasoningEffort()/effortForThinkingBudget()`
  — mapeamento de thinking-level entre protocolos + `reasoning.effort` forçado
  (`config.ForcedEffort()`, `protocol.ForcedEffort()`).
- Validação estrita de tool-call ordering em `encodeChatRequest()` (pending/duplicate/
  missing-results → 400 antes do upstream).

## 4. Model discovery / pricing / tiers

- Nós: `serveModels()` — GET `/models` na primeira conta que responde 200 + filtro
  substring `exclude_models` (`filterModels`). Sem tiers, sem protocolo, sem preço.
- Deles (`internal/models/`):
  - `discovery.go:FetchModels()` por tier (zen+go) + `FetchCapabilities()` de
    `models.opencode.ai/api.json` (SDK→protocolo) + fallback docs via regex.
  - `catalog.go:Route()/keyTierOrderLocked()/protocolForLocked()` — rota por `prefer`
    (go/zen), protocolo **por tier**, `models.protocols` como override.
  - `pricing.go:PricingStore` (`models.dev/api.json`, refresh 24h) + `Decide()` —
    elegibilidade anonymous: nome contém `free` OU custo zero + não-deprecated.
    `IsFreeModel()` alimenta o shaping (§1).
  - `cache.go` — cache em disco schema v3, `LoadCache` marca `stale`, `/healthz`
    reporta `pending/empty/stale`.

## 5. Quota/saldo (402) e retry/failover

**Nada no theirs trata 402 de forma "mais inteligente" que failover** (factual):
- `upstream.go:isNonRetryableClientResponse()` = `400–499 exceto 401/403/429`.
  Logo **402 é não-retryable**: encerra o tier, retorna o body upstream
  (`copyErrorResponse` preserva status+message+`Retry-After`), e
  `observeKeyResult`/`pool.go:MarkFailure` **não penalizam** a key (402 até limpa
  falhas via `MarkSuccess`).
- **403 é retryable**: rotaciona keys, `MarkFailure` com backoff exponencial até 8x
  `failure_cooldown_seconds` + honra `Retry-After` (`pool.go:MarkFailure/parseRetryAfter`),
  e ainda tenta o próximo tier (`doUpstreamTiers`).
- Nós (`proxy.go:isRetryable`): `401/403/429/5xx` retryable — **402 cai em passthrough
  direto nos dois**, 403 rotaciona nos dois. Comportamento equivalente; a diferença é
  diagnóstico, não recuperação.

Diferenças de política que importam:
- Deles: `max_attempts` **por tier**, anonymous tenta cada proxy 1x fora do budget;
  guarda `ctx.Err()` para **não esfriar keys nunca tentadas** com budget expirado;
  `attempt_timeout_seconds` (header-wait por tentativa) vs nosso `upstreamClient`
  único de 60s (`proxy.go:15`).
- Deles: fallback cross-tier (zen 402 → tenta go); nós temos pool único, sem tiers.
- Deles: `syncProxyResult` (`refresh.go`) — só timeout/conn-refused evicta proxy +
  `RebindProxy` least-loaded; 4xx/5xx disparam check neutro (Cloudflare trace), não
  evicção. Nós: qualquer retryable troca de conta; só 429 dá cooldown fixo 60s;
  sem conceito de proxy (sem `proxies/proxyfile`, sem SOCKS).

## 6. Auth upstream / local / admin

- Upstream: nós `ZenAccount{api_key}` Bearer; eles `zen_keys/go_keys []string` +
  `anonymous:true` → Bearer `public`, pools separados `zenNodes/goNodes/anonymousPool`.
- Local: nós `api_keys[]` só-Bearer; eles `server_keys[]` aceitam `Bearer` **e**
  `x-api-key` com `ConstantTimeCompare` (`gateway.go:authenticate`).
- Admin: nós Bearer `admin_password` plaintext no YAML + lockout IP próprio
  (`limit.go:newAdminLockout`). Deles: porta separada (`webui.listen`, default 8081),
  Argon2id (`config/password.go:HashPassword`), sessão server-side HttpOnly/SameSite +
  CSRF + throttling (`admin/auth.go`), `PUT /api/account` invalida sessões,
  `POST /api/config/reveal` exige re-verificação de senha.

## 7. Admin API + WebUI

- Nós (`server.go:handleAdmin`): `GET/POST /admin/api/accounts|keys`,
  `PUT .../accounts/{id}|keys/{id}` (só `enabled`), `POST quotas/refresh`,
  `GET overview`, `oauth/start|status|complete|cancel`, `POST /oauth/callback`.
  WebUI = `internal/web/index.html` placeholder.
- Deles (`admin/server.go`, `debug.go`): `POST /api/auth/login|logout`,
  `GET /api/auth/session`, `GET/PUT /api/config` (view mascarada
  `SecretView{id=fingerprint,display}`), `POST /api/config/reload|reveal`,
  `PUT /api/account`, `GET /api/monitor`, `GET /api/debug/models`,
  `POST /api/debug/inference` (**Playground**: modo `auto` vs `selected` por
  fingerprint, `stream:false` forçado, 12/min/IP, `WithDiagnosticRequest` — **não
  muta cooldown/binding**, retorna `ok/http_status/request_id/route/response` +
  `key_test: usable|rejected|rate_limited|transport_error|upstream_error|request_error|
  unavailable` via `classifyKeyTest`), `GET /api/logs|/api/logs/stream` (SSE),
  WebUI completa (Playground, diagnostics, token stats, live logs).

## 8. Config

- Nós: YAML (`config.go:LoadOrBootstrap`), bootstrap auto + print de segredos no
  stderr, `Save()` com `yaml.Marshal` direto (sem tmp+rename, sem .bak), sem validação
  estrita, sem hot-reload.
- Deles: JSON com comentários, `DisallowUnknownFields` (`config/json.go`),
  `Normalize()` valida tudo, `SaveAtomic()` tmp+rename+`.bak` 0600,
  `RuntimeManager.Apply()` **valida e constrói o gateway substituto antes de trocar**
  (in-flight drena no antigo), `Reload()` do disco; só `listen/webui.*` exigem restart.

## 9. Observabilidade

- Nós: `--debug` → `debugLog`+`maskSecrets` no stderr; `GET /usage` (contadores
  persistidos); `GET /health` estático `{"status":"ok"}`.
- Deles (`internal/telemetry/`): `Middleware` (active, `Monitor.Record`, log
  `request_routed` com model/tier/key/channel/attempts/usage), `metrics.go:Monitor`
  (buckets 60min, p50/p95/p99, tokens + coverage, attempts por tier/channel/key,
  anéis 10k req/20k att), `LogHub` ring + `/api/logs/stream`, `logging.level` +
  `dump_request_bodies` (cap 64KiB, com redact), `recovery.go:Recover` (panic→500),
  `/healthz` com `ready/models/keys/proxies/issues` e 503 quando
  `pending/empty/no_healthy_proxies`.

## 10. Stream

- Nós (`serveStream`): relay byte cru 32KiB + `Flush()`, failover só pré-first-byte;
  pós-início, erro só fecha. Sem parse, sem usage.
- Deles (`protocol/stream*.go`, 681 linhas só o emitter): `ForwardStream` (mesmo
  protocolo, passthrough + `streamUsageObserver` que extrai usage sem tocar nos bytes)
  vs `TranscodeStream` (parser por protocolo + emitter para o target, `finish→tool_calls`
  quando há function_call, erro mid-stream vira evento de erro no protocolo target,
  EOF sem terminal vira `emitUnexpectedStreamError`). Usage real alimenta monitor.

## 11. Arquitetura

- Nós: monolito `internal/app` (config+store+proxy+server+quota+limit+oauth+i18n) +
  `internal/web` (embed). Simples, 1 porta, 1 tier, 1 protocolo.
- Deles: `cmd/opencode2api/main.go` só wiring (2 `http.Server` + graceful 15s);
  `gateway` (roteamento/retry/pools), `protocol` (conversão), `models` (descoberta),
  `config` (validação/persistência), `admin` (gestão), `telemetry`, `identity`,
  `httpx/jsonutil`, `buildinfo`. Separação permite hot-swap (`runtime.go`).

## 12. O que SÓ nós temos

- `OAuthFlow` (`oauth.go`) + `POST /oauth/callback` com dedupe `duplicate_account`.
- Rate-limit por client-key (`limit.go:keyRateLimiter`, 120/60s/300s, `Retry-After`) —
  eles só limitam o Playground.
- Quota display (`quota.go:QuotaSnapshot{monthly/five_hour/weekly}`, `Blocked()`,
  `isLowBalance`, `GET /admin/api/overview`, `POST /admin/api/quotas/refresh`) +
  `usage.json` persistido (`usage.go:Record/Snapshot`) + `GET /usage`. Nota:
  `fetchQuota` é **stub** que sempre retorna `Unknown:true` — display-only, nunca gateia
  (por design `PAR-12`). A `PricingStore` deles é metadata de preço, não saldo por conta.
- Contas nomeadas com enable/disable + client-keys nomeadas vs arrays de strings deles.
- `exclude_models` (substring), `/v1/models` público sem auth (deles exige auth),
  config YAML com bootstrap, i18n (`i18n.go`), deploy single-port.

## 13. Sugestões priorizadas (foco no 403-free + 402-funds)

**P0 — diagnóstico + hipótese free-shape**
1. Portar shaping free-tier + sessão canônica como experimento isolado:
   `prepareAnonymousBody`/`shapeKeyBody` (force `stream:true` + tools
   `bash/edit/glob/grep/read` + `include_usage` só para `IsFreeModel`) +
   `CollapseStream` para `stream:false` + headers `identity` (`x-opencode-session`
   canônico `ses_…`, `x-opencode-client: cli`, `prompt_cache_key`). Único item que
   **muda o veredito 403→2xx**. Testável em staging com 1 conta contra
   `muse-spark-1.3-contributor-free`.
2. Playground selected-key (`admin/debug.go:handleDebugInference` +
   `doSelectedKeyUpstream` + `WithDiagnosticRequest`): 1 request, 1 key, sem failover,
   sem mutar cooldown, com `key_test`. Hoje é impossível distinguir "todas as keys
   mortas" de "request malformado" sem degradar produção. Atenção: 402 cai em
   `request_error` no `classifyKeyTest`, não em `rejected` — documentar.
3. Log por tentativa: `recordUpstreamAttempt` (key fingerprint + proxy + status +
   `outcome`) + `copyErrorResponse` (repasse de message + `Retry-After` upstream) +
   `dump_request_bodies` opt-in. Hoje o 402/403 chega ao cliente sem rastro de qual
   conta falhou.
4. Semântica 402 vs resto: codificar `isNonRetryableClientResponse` — 402 **não**
   rotaciona nem esfria key (determinístico); 401/403/429/5xx rotacionam com backoff
   exponencial + `Retry-After`. Evita que 402 queime o pool e mascare o 403 real.

**P1 — roteamento e resiliência**
5. Descoberta por tier (`FetchModels` + `FetchCapabilities` + `Route/KeyTiers` +
   `GET /api/debug/models` com `RouteDiagnostic`): responde "o modelo existe no
   tier X?" antes de culpar saldo.
6. `attempt_timeout_seconds` separado do timeout total + guarda `ctx.Err()` (não
   esfriar keys nunca tentadas) + health de proxy só em timeout/conn-refused.
7. `/healthz` com `issues` e 503 real; hot-reload validado (`RuntimeManager.Apply`).

**P2 — resto**
8. Parser/emitter de stream (`TranscodeStream`/`ForwardStream` + usage real) quando
   existir cliente Responses/Anthropic; hoje o relay cru basta para chat puro.
9. Split de portas API/WebUI, Argon2id + sessões + CSRF, config JSON estrita —
   hardening, não incidente.
10. Não portar: `reasoning.effort` forçado e bridge multi-protocolo completo até haver
    demanda; o stub `fetchQuota` nosso já cobre display — saldo real exige endpoint de
    billing que o upstream não documenta (o theirs também não tem).
