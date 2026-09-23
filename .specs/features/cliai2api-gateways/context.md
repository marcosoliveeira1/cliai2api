# cliai2api-gateways Context

**Gathered:** 2026-09-22
**Spec:** `.specs/features/cliai2api-gateways/spec.md`
**Status:** Ready for design

---

## Feature Boundary

Transformar `cmdcode2api` em `cliai2api` multi-gateway (cmdcode + opencode zen, extensível), com roteamento por prefixo de model, migração automática de config e multi-key no Zen. MVP Zen = `chat/completions` + `responses`. Fora: `messages`, Gemini, systemone, quota Zen, rename de repo/imagem.

---

## Implementation Decisions

### Roteamento — prefixo no model

- `cmdcode/<id>` e `opencode/<id>`; sem `/` = `cmdcode` (compat legada).
- Prefixo removido antes de chamar o upstream; erro `404` para prefixo desconhecido.
- Sem endpoint novo, sem header/query de override.

### Config e rename — migração automática

- `commandcode.base_url/accounts/api_key` migra para `gateways.cmdcode.*` no load; salva novo formato.
- Binário passa a se chamar `cliai2api`; lê `config.yaml` antigo sem intervenção.
- Breaking change rejeitado.

### Escopo Zen MVP — chat + responses

- `chat/completions` primeiro (passthrough 1:1 + headers CLI-only).
- `responses` com tradução SSE para OpenAI (stream e non-stream).
- `messages`/Gemini/systemone explícitamente fase 2.

### Catálogo — unificado com prefixo

- `GET /v1/models` = união dos catálogos com prefixo aplicado.
- `exclude_models` segue global (casa sufixo após `/`).
- Gateway sem contas contribui vazio, sem falhar a lista.

### Agent's Discretion

- Nome exato do prefixo zen (`opencode/` vs `zen/`): usuário escolheu `opencode/` pelo padrão `opencode/<model-id>` do docs Zen — manter.
- Formato YAML de `gateways.*`: modelar espelhando `commandcode` atual (`base_url`, `accounts[]`).
- `usage.json`: estender com dimensão gateway sem migração destrutiva.

### Declined / Undiscussed Gray Areas → Assumptions

- Nenhuma área recusada; as 4 áreas foram votadas via questionário. Headers CLI-only (`x-opencode-*`) ficam como suposição a validar no recon (mitmproxy contra CLI 1.18.31).

---

## Specific References

- Docs Zen: `https://opencode.ai/docs/zen` — famílias por endpoint, `opencode/<model-id>`, `GET /zen/v1/models`.
- Recon prévio: `kode-ai/providers/opencode/headers.go` (headers `x-opencode-project/session/request/client`, `User-Agent: opencode/*`, IDs `ses_/msg_`).
- CLI local `opencode 1.18.31` disponível para captura de tráfego real.

---

## Suposições assinadas (T4 recon)

- **Data:** 2026-09-22. **Task:** T4 (GW-03, GW-03b).
- **Decisão:** captura live (mitmproxy) contra o CLI `opencode 1.18.31` não foi
  executada neste ambiente — sem CLI instalado nem proxy de interceptação
  disponível. Forma dos headers e formato `ses_<12hex><14base62>` validados
  apenas contra a referência estática `.spec/issues.md` §2 (que cita
  `internal/identity/request.go:CanonicalSessionID()` e `newUpstreamRequest()`
  do referência `jasonxu114514/opencode2api`).
- **Suposições que ficam como assinadas até recon live:**
  1. Conjunto de headers (`x-opencode-client/session/request/project/parent`,
     `x-session-affinity`, `X-Session-Id`, `prompt_cache_key`,
     `User-Agent: opencode/<ver>`) espelha o CLI real.
  2. `ses_<12hex><14base62>` é o shape canônico exigido pelo free-tier desde
     2026-09-16; `msg_<12hex><14base62>` e `prj_<12hex>` seguem o mesmo esquema.
  3. `User-Agent` carrega a versão `opencode/1.18.31` (constante `zenCLIVersion`
     em `internal/app/zen_headers.go` — atualizar quando o CLI for recapturado).
- **Follow-up:** recapturar com mitmproxy + CLI real se houver divergências de
  sessão/headers. Muse Spark free usa Responses conforme o catálogo Zen atual;
  modelos free de chat continuam no caminho de agent-shape.

---

## Deferred Ideas

- Zen `messages` (Claude/Qwen), Gemini `models/*`, `systemone` (Jev).
- Quota/billing dashboard do Zen.
- Rename repo + imagem GHCR (`ghcr.io/.../cliai2api`).
- Bring-your-own-key por modelo no Zen.
