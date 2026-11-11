# 0006. Two bearer-token scopes: management and inference

- Status: Proposed
- Date: 2026-11-11

> Recorded from the implemented code and ARCHITECTURE.md. The design notes say these calls were
> delegated during implementation; this record is not approved until the owner accepts it.

## Context
The API exposes cost-incurring management actions (deploy, stop, register models) and everyday chat. Clients like the Telegram bot should only get what they need.

## Decision
Two static bearer tokens from the environment: management (`/api/v1`) and inference (`/v1`). Each at least 24 characters and different; compared as SHA-256 digests in constant time. Responses never include runtime endpoints or credentials.

## Alternatives
- One token: chat clients could launch GPUs.
- OAuth/OIDC: an identity provider for a single-owner system.

## Rationale
Least privilege with no external dependency.

## Consequences
- Positive: simple, auditable.
- Negative: no per-client identity or revocation short of rotating a token; no rate limiting.

## References
ARCHITECTURE.md §6a A11, §6b (REST); `adapters/inbound/rest/handler.go`.
