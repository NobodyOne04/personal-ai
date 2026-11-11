# 0009. vLLM runtime with random per-deployment keys stored encrypted

- Status: Accepted
- Date: 2026-11-11
- Replaces: [0004](0004-vllm-runtime-with-derived-api-keys.md) (rejected)

> Accepted by the owner on 2026-11-11. **Not implemented yet**: the code still derives keys by
> HMAC (0004). Build this before the first real launch (`CLAUDE.md`).

## Context
Each model runs in vLLM on a RunPod pod behind RunPod's public HTTPS proxy, so every runtime
needs its own credential. 0004 derived it from one server secret and the deployment ID, so the
key was predictable from that secret and could not be rotated per deployment.

## Decision
- vLLM, speaking the OpenAI protocol, stays the runtime.
- At each launch (and restart) the server generates a random 256-bit key (`crypto/rand`) and
  passes it to the pod as `VLLM_API_KEY`.
- The key is stored in the `deployments` row, encrypted with AES-256-GCM (random nonce, the
  deployment ID as associated data) under a server key from `PERSONAL_AI_RUNTIME_KEY_SECRET`.
  A key-version column allows re-encrypting under a new server key.
- The key is decrypted only in memory when calling the runtime; it never appears in API
  responses, logs or the runtime ref.

## Alternatives
- HMAC-derived keys (0004): nothing stored, but keys are deterministic and can't be rotated
  one deployment at a time. Rejected by the owner.
- Random keys stored in plain text: a database dump would expose every live runtime.
- No key: anyone with the proxy URL could use the GPU.

## Rationale
Keys are unpredictable and independent: one leaked key exposes one deployment, and restarting a
deployment rotates its key without touching the others.

## Consequences
- Positive: per-deployment rotation; keys not derivable from any single value plus public data.
- Negative: a migration (encrypted key + key version columns) and encryption code to maintain;
  the database plus the server key together expose live keys; changing the server key means
  re-encrypting rows.
- Does **not** fix SEC-004: vLLM leaves `/invocations` and other routes unauthenticated whatever
  the key is. That needs its own fix.

## References
ADR 0004; `docs/security/SECURITY_REPORT.md` SEC-004; `adapters/outbound/inference/vllm`,
`adapters/outbound/persistence/postgres`.
