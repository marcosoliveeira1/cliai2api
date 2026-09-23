# Jev Free via OpenCode Zen

## Context

OpenCode Zen lists `jev-1.13-free` as a free model, but it does not use the
OpenAI Chat Completions or Responses protocol. The documented endpoint is
`POST https://opencode.ai/zen/v1/systemone`. The shared gateway currently
accepts OpenAI Chat Completions requests and cannot transparently translate
them into Jev's typed question protocol.

Reference: <https://opencode.ai/docs/zen/#jev> (model catalog and Jev request
examples).

## Goal

Make Jev callable through the existing `opencode/` model namespace without
misrouting it to `/v1/chat/completions` or `/v1/responses`.

## Requirements

1. Identify Jev model IDs (`jev-*`, including `jev-1.13-free`) as a distinct
   Zen protocol family.
2. Send Jev requests to `/v1/systemone` with the configured Zen Bearer key and
   the CLI identity headers used by other Zen calls.
3. Convert an OpenAI-style user request into Jev's required `state` and
   `questions` object. The default question must have a stable ID, a supported
   type, and instructions that request a concise natural-language answer.
4. Convert the typed Jev response into the gateway's OpenAI-compatible
   `chat.completion` response, preserving upstream errors and usage when
   available.
5. Keep Jev-specific request/response handling isolated from chat and Responses
   families so failover, accounting, and API behavior remain consistent.
6. Document that `/v1/chat/completions` cannot express all System One question
   types. A future native endpoint or explicit request extension may expose
   `noul`, `choice`, and `score` without guessing from ordinary chat messages.

## Acceptance Criteria

- `opencode/jev-1.13-free` is sent to `/v1/systemone`, never either OpenAI
  endpoint.
- The upstream request contains `model`, `state`, and a valid `questions` map.
- A valid typed answer is returned as an OpenAI-compatible assistant message.
- Upstream authentication, availability, and validation errors retain their
  useful status and message through the gateway.
- Existing Zen chat, free-chat shaping, and Responses routes are unaffected.

## Out of Scope

- General-purpose translation of arbitrary OpenAI tool calls into Jev
  questions.
- Reproducing all Jev question types in the first implementation.
- Changes to the Command Code gateway.
