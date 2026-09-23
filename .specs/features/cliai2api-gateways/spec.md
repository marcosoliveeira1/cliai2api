# cliai2api — Multi-Gateway Specification

## Problem Statement

`cmdcode2api` é um gateway OpenAI-compatível acoplado a um único upstream (Command Code, `POST /alpha/generate`). Para suportar OpenCode Zen (múltiplas keys, famílias `chat/completions` + `responses`) e futuros upstreams sem duplicar o gateway, o projeto precisa virar `cliai2api`: um gateway com abstração de providers, roteamento por prefixo de model e migração automática do config legado.

## Goals

- [ ] Um binário `cliai2api` serve chat OpenAI-compatível via 2 gateways configuráveis (`cmdcode`, `zen`), extensível a N.
- [ ] Config legado (`commandcode.*`) migra automaticamente para `gateways.*` sem perda de contas/keys/usage.
- [ ] Zen suporta N keys com round-robin + failover iguais ao cmdcode.

## Out of Scope

| Feature | Reason |
| ------- | ------ |
| Zen `/messages` (Claude/Qwen), Gemini `models/*`, `systemone` (Jev) | Fase 2; MVP cobre `chat/completions` + `responses` |
| Quota dashboard do Zen | Zen não tem `/alpha/*`; fica para fase futura |
| Reasoning-effort forçado e bridge multi-protocolo completo | Sem demanda (§13 item 10 do issues); não gateia o 403 |
| Rename do repo/imagem GHCR | Só binário + config nesta fase |
| OAuth CLI do Zen | Zen é paste-key; sem fluxo browser |

---

## Assumptions & Open Questions

| Assumption / decision | Chosen default | Rationale | Confirmed? |
| --------------------- | -------------- | --------- | ---------- |
| Roteamento por prefixo `gateway/model` | `cmdcode/<id>`, `opencode/<id>`; sem prefixo = `cmdcode` (compat) | Decidido com usuário; preserva clientes atuais | y |
| Migração automática de config | `commandcode.base_url/accounts/api_key` → `gateways.cmdcode.*` na carga, salva novo formato | Decidido com usuário; zero fricção | y |
| MVP Zen = chat + responses | `chat/completions` passthrough + `responses` com tradução SSE | Decidido com usuário | y |
| Catálogo unificado com prefixo | `GET /v1/models` retorna todos com prefixo; `exclude_models` global | Decidido com usuário | y |
| Headers CLI-only do Zen (`x-opencode-*`, `User-Agent: opencode/*`) | Gateway injeta por request (project/session/request gerados) | Requerido desde set/2026; sem isso 401/403 | n (validar no recon) |
| Failover Zen = mesmo do cmdcode (401/403/429/5xx, cooldown 429) | Reuso de `AccountPool` por gateway | Consistência + menos código | y |
| Semântica 4xx segue `isNonRetryableClientResponse` do referência | 400–499 exceto 401/403/429 = não-retryable (encerra tentativa sem esfriar key); 402 cai aqui | Evita que 402 queime o pool e mascare o 403 real (issues §5) | y |
| Uso de API key própria do usuário, sem abuso de free-tier | Documentado no README | Risco ToS | y |

**Open questions:** none — all resolved or logged above.

---

## User Stories

### P1: Abstração de gateway + migração legada ⭐ MVP

**User Story**: As a dono do gateway, I want o binário `cliai2api` carregar meu `config.yaml` antigo e continuar servindo cmdcode so that nada quebra no upgrade.

**Why P1**: Sem isso não há base para o segundo gateway.

**Acceptance Criteria**:

1. WHEN o servidor inicia com `config.yaml` legado (`commandcode.accounts`, `commandcode.base_url`) THEN ele SHALL servir `POST /v1/chat/completions` com model sem prefixo ou `cmdcode/<id>` e persistir o formato `gateways.cmdcode.*`.
2. WHEN `GET /v1/models` é chamado após migração THEN ele SHALL listar os modelos cmdcode com prefixo `cmdcode/`.
3. WHEN model tem prefixo desconhecido (`foo/bar`) THEN o sistema SHALL retornar `404 invalid_request_error`.

**Independent Test**: Subir com config legado fixture, `curl /v1/models` mostra `cmdcode/...`, chat sem prefixo funciona, `config.yaml` reescrito contém `gateways:`.

---

### P1: Roteamento por prefixo ⭐ MVP

**User Story**: As a cliente OpenAI, I want prefixar o model (`opencode/gpt-5.5`) so that escolho o gateway por chamada.

**Why P1**: Contrato central do multi-gateway.

**Acceptance Criteria**:

1. WHEN `POST /v1/chat/completions` com `"model": "opencode/<id>"` THEN o sistema SHALL encaminhar ao gateway Zen e remover o prefixo antes do upstream.
2. WHEN `"model": "cmdcode/<id>"` THEN o sistema SHALL encaminhar ao gateway cmdcode.
3. WHEN `"model"` sem `/` THEN o sistema SHALL assumir gateway `cmdcode` (compat legada).

**Independent Test**: Dois upstreams fake (httptest), um chat pra cada prefixo, assert que cada fake recebeu exatamente 1 request com model sem prefixo.

---

### P1: Zen multi-key `chat/completions` ⭐ MVP

**User Story**: As a usuário Zen, I want cadastrar N keys e chamar `opencode/deepseek-v4-flash` so that tenho rotação e failover.

**Why P1**: Cobre DeepSeek/MiniMax/GLM/Kimi/free models com passthrough 1:1.

**Acceptance Criteria**:

1. WHEN N keys Zen configuradas e `POST /v1/chat/completions` com `opencode/<chat-id>` THEN o sistema SHALL rotacionar round-robin e injetar `Authorization: Bearer <key>` + headers `x-opencode-*` / `User-Agent: opencode/*`.
2. WHEN a key ativa falha com 401/403/429/5xx THEN o sistema SHALL fazer failover para a próxima key antes do primeiro byte (mesma semântica cmdcode).
3. WHEN o upstream retorna 4xx fora de 401/403/429 (incl. 400/402/404/422) THEN o sistema SHALL encerrar a tentativa sem rotacionar nem esfriar a key (`isNonRetryableClientResponse`; 402 até limpa falha via `MarkSuccess`).
4. WHEN `stream: false` THEN o sistema SHALL repassar o JSON do Zen como resposta OpenAI `chat.completion`.

**Independent Test**: 2 keys fake, primeira retorna 429 com `Retry-After: 1`, segunda 200; assert resposta 200 + cooldown registrado na key 1. Fake retorna 402; assert sem rotação e sem cooldown.

---

### P1: Zen free-tier shaping + sessão canônica ⭐ MVP (P0 do `.spec/issues.md` §1–§2)

**User Story**: As a usuário Zen, I want chamar modelos `*-free` pelo protocolo correto para cada família so that as rotas gratuitas de chat recebem agent-shape e Muse Spark continua usando Responses.

**Why P1**: Causa provável do 403 observado: o upstream exige agent-shape + sessão canônica para `*-free` (issues §1–§2). Sem isso, passthrough 1:1 falha por construção em todas as keys — mesmo válidas.

**Acceptance Criteria**:

1. WHEN o model é free-tier e pertence à família Chat Completions (`IsFreeModel`: nome contém `free` OU pricing metadata custo zero + não-deprecated) THEN o sistema SHALL reescrever o body: forçar `stream: true`, injetar core tools ausentes (`bash`, `edit`, `glob`, `grep`, `read`) com definições mínimas, e `stream_options.include_usage: true` — em key tiers autenticadas, igual ao referência (`shapeKeyBody`). Modelos Muse Spark usam Responses conforme o catálogo Zen, mesmo quando têm sufixo `-free`.
2. WHEN o cliente pediu `stream: false` para um model com shaping THEN o sistema SHALL colapsar o SSE upstream de volta para JSON `chat.completion` (`CollapseStream`).
3. WHEN enviando ao Zen THEN o sistema SHALL gerar sessão no formato canônico `ses_<12hex><14base62>` (rejeitar outro shape: free-tier retorna 403 desde 2026-09-16) e enviar `x-opencode-client: cli`, `x-opencode-session`, `x-session-affinity`, `X-Session-Id` (mesmo valor), `x-opencode-request/project/parent`, `prompt_cache_key` — derivando de `x-opencode-session/x-session-affinity/X-Session-Id/conversation-id/...` ou da primeira mensagem user.
4. WHEN o código de sessão/shaping diverge do CLI `opencode 1.18.31` THEN a divergência SHALL estar registrada no `context.md` como suposição assinada (recon mitmproxy).

**Independent Test**: Upstream fake que exige agent-shape (403 sem tools, 200 com tools); assert que um modelo free de chat como `nemotron-3-ultra-free` com `stream:false` retorna JSON e o fake recebeu `stream:true` + 5 core tools. Assert formato `ses_…` via regex em teste unit. Para Muse Spark free, assert rota `/v1/responses` e payload Responses (`input`, não `messages`).

---

### P1: Zen `responses` com tradução SSE ⭐ MVP

**User Story**: As a usuário Zen, I want chamar modelos GPT/Grok/Muse (família Responses, inclusive Muse Spark free) via `/v1/chat/completions` so that uso esses modelos pelo gateway.

**Why P1**: Sem isso metade do catálogo pago fica inacessível.

**Acceptance Criteria**:

1. WHEN `POST /v1/chat/completions` com `opencode/<responses-id>` e `stream: true` THEN o sistema SHALL converter o pedido Chat Completions para o formato Responses, chamar `POST {zen_base}/v1/responses`, traduzir texto e chamadas de função em chunks OpenAI e terminar com `data: [DONE]`.
2. WHEN `stream: false` THEN o sistema SHALL agregar a resposta `responses` em um `chat.completion` com `finish_reason` válido.
3. WHEN o upstream `responses` fecha sem finish THEN o sistema SHALL retornar `502 upstream_stream_incomplete` (mesma semântica atual).

**Independent Test**: Upstream fake SSE `responses`, assert stream OpenAI contém texto + `[DONE]`; caso sem finish assert 502.

---

### P1: Catálogo unificado ⭐ MVP

**User Story**: As a cliente, I want `GET /v1/models` listar cmdcode + zen com prefixo so that descubro modelos sem configurar nada.

**Why P1**: Descoberta é parte do contrato OpenAI.

**Acceptance Criteria**:

1. WHEN ambos gateways têm modelos THEN `GET /v1/models` SHALL retornar `cmdcode/<id>` + `opencode/<id>` após `exclude_models` global.
2. WHEN um gateway está sem contas THEN ele SHALL contribuir com lista vazia sem derrubar o outro.

**Independent Test**: Catalog fixtures dos dois gateways, assert prefixos + filtro `exclude_models: ["gpt-"]` remove `opencode/gpt-5.5` e mantém resto.

---

### P2: WebUI por gateway

**User Story**: As a operador, I want gerenciar contas/keys do Zen na WebUI separado do cmdcode so that opero sem editar YAML.

**Why P2**: Paridade operacional; não bloqueia API.

**Acceptance Criteria**:

1. WHEN admin lista contas THEN cada conta SHALL exibir seu `gateway` (`cmdcode`|`zen`).
2. WHEN admin cria conta informando `gateway: zen` THEN ela SHALL entrar no pool Zen e persistir em `gateways.zen.accounts`.

**Independent Test**: `POST /admin/api/accounts {"gateway":"zen",...}` + `GET` mostra `gateway: zen`.

---

### P2: Diagnóstico por tentativa + Playground selected-key

**User Story**: As a operador, I want distinguir "todas as keys mortas" de "request malformado" sem degradar produção so that debugo 402/403 sem queimar o pool.

**Why P2**: Hoje o 402/403 chega ao cliente sem rastro de qual conta falhou (issues §13 itens 2–3).

**Acceptance Criteria**:

1. WHEN um request falha no upstream THEN o log SHALL conter key fingerprint + proxy + status + `outcome` por tentativa, e a resposta ao cliente SHALL preservar message + `Retry-After` do upstream (`copyErrorResponse`).
2. WHEN admin chama o Playground com key selecionada THEN o sistema SHALL fazer 1 request, 1 key, sem failover, sem mutar cooldown/binding, retornando `ok/http_status/request_id/route/response` + `key_test: usable|rejected|rate_limited|transport_error|upstream_error|request_error|unavailable` (402 cai em `request_error`, não `rejected` — documentar).

**Independent Test**: Fake 403 na key A + Playground selected A; assert `key_test: rejected` e pool intacto (A sem cooldown adicional).

---

## Edge Cases

- WHEN `gateways.zen` vazio e model `opencode/*` THEN sistema SHALL retornar `503 no_accounts` com `gateway: zen` no erro.
- WHEN key duplicada dentro do mesmo gateway THEN SHALL rejeitar `409 duplicate` (IDs derivam da key, como hoje).
- WHEN mesma key em gateways diferentes THEN SHALL permitir (pools independentes).
- WHEN `exclude_models` casa sufixo após `/` THEN SHALL filtrar (`gpt-` remove `opencode/gpt-5.5`).

---

## Requirement Traceability

| Requirement ID | Story | Phase | Status |
| -------------- | ----- | ----- | ------ |
| GW-01 | P1: Abstração + migração | Design | Pending |
| GW-02 | P1: Roteamento por prefixo | Design | Pending |
| GW-03 | P1: Zen chat multi-key | Design | Pending |
| GW-03b | P1: Zen free-tier shaping + sessão canônica (issues §1–§2) | Design | Pending |
| GW-04 | P1: Zen responses SSE | Design | Pending |
| GW-05 | P1: Catálogo unificado | Design | Pending |
| GW-06 | P2: WebUI por gateway | Design | Pending |
| GW-07 | P2: Diagnóstico por tentativa + Playground selected-key (issues §13.2–3) | Design | Pending |

**Coverage:** 8 total, 0 mapped to tasks, 8 unmapped.

---

## Success Criteria

- [ ] Upgrade com config legado: chat sem prefixo funciona + `config.yaml` migrado, zero intervenção.
- [ ] `opencode/deepseek-v4-flash` (chat) e `opencode/gpt-5.5` (responses) funcionam stream + non-stream via gateway.
- [ ] `opencode/muse-spark-1.3-contributor-free` com `stream:false` usa `/v1/responses`, converte o payload e retorna JSON `chat.completion`.
- [ ] 402 não rotaciona nem esfria key; 403 rotaciona com backoff + `Retry-After`.
- [ ] Failover Zen validado em teste com 429 → próxima key.
- [ ] `go test ./...` verde; nenhum teste legado enfraquecido.

## Implicit-Requirement Dimensions Sweep

- Input validation & bounds → GW-02/GW-05 (prefixo desconhecido 404, exclude global).
- Failure / partial-failure → GW-04 (stream incompleto 502), edge 503 sem contas.
- Idempotency / retry / duplicate → GW-03 (failover pré-primeiro-byte; sem replay de stream iniciado; 402 sem retry/cooldown), dup 409.
- Auth boundaries & rate limits → GW-03 (cooldown 429 por key, `Retry-After` repassado), GW-03b (free-tier exige agent-shape + sessão canônica).
- Concurrency / ordering → `AccountPool` por gateway, round-robin thread-safe (reuso).
- Data lifecycle / expiry → migração persiste novo formato; `usage.json` com contadores por gateway+conta.
- Observability → logs com `gateway=` em cada upstream call; contadores de erro por pool.
- External-dependency failure → 502/503 mapeados por gateway sem derrubar o outro (GW-05).
- State-transition integrity → N/A porque MVP não adiciona transições de estado além de enabled/cooldown já existentes.
