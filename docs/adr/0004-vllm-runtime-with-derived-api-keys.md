# 0004. vLLM runtime with per-deployment API keys derived by HMAC

- Status: Proposed
- Date: 2026-11-11

> Recorded from the implemented code and ARCHITECTURE.md. The design notes say these calls were
> delegated during implementation; this record is not approved until the owner accepts it.

## Context
Each model runs in vLLM on a RunPod pod reachable through RunPod's public HTTPS proxy, so the runtime needs a credential, and storing one per deployment adds secret storage.

## Decision
vLLM speaking the OpenAI protocol. The per-deployment key is HMAC-SHA256(`PERSONAL_AI_RUNTIME_KEY_SECRET`, deployment ID), passed to the pod as `VLLM_API_KEY` and recomputed by the server on each call. No runtime credential is stored.

## Alternatives
- Random key per deployment stored in PostgreSQL: needs encryption at rest and rotation.
- No key, rely on the proxy URL being unguessable: anyone with the URL could use the GPU.

## Rationale
Zero stored secrets for runtimes; rotating the one server secret invalidates every runtime key.

## Consequences
- Positive: nothing to leak from the database.
- Negative: rotating the secret requires restarting running deployments; vLLM's key covers `/v1` only, other vLLM routes stay open on the proxy.

## References
ARCHITECTURE.md §6a A2, §6b (vLLM / A2); `adapters/outbound/inference/vllm`.
