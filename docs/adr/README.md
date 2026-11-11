# Architecture Decision Records

Format and lifecycle: `docs/adr/NNNN-short-title.adr`, statuses Proposed, Accepted, Rejected, Deprecated, Superseded.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-hexagonal-core-with-enforced-boundaries.adr) | Hexagonal core with import boundaries enforced by a test | Accepted |
| [0002](0002-postgresql-with-pgx-and-embedded-migrations.adr) | PostgreSQL via pgx with an embedded migration runner | Accepted |
| [0003](0003-runpod-with-operator-declared-offerings.adr) | RunPod compute with operator-declared offerings and label-based reconciliation | Accepted |
| [0004](0004-vllm-runtime-with-derived-api-keys.adr) | vLLM runtime with per-deployment API keys derived by HMAC | Rejected, replaced by 0009 |
| [0005](0005-single-server-owns-state-clients-use-rest.adr) | One server replica owns scheduling and state; CLI and Telegram use the REST API | Accepted (temporary, tech debt) |
| [0006](0006-two-bearer-token-scopes.adr) | Two bearer-token scopes: management and inference | Accepted |
| [0007](0007-json-config-secrets-from-env.adr) | JSON config file, secrets only from the environment | Rejected, replaced by 0010 |
| [0008](0008-allowlist-runtime-images-and-remote-code.adr) | Allowlist runtime images and gate remote model code | Accepted |
| [0009](0009-vllm-runtime-with-random-encrypted-keys.adr) | vLLM runtime with random per-deployment keys stored encrypted | Accepted |
| [0010](0010-yaml-config-secrets-from-env.adr) | YAML config file, secrets only from the environment | Accepted |
