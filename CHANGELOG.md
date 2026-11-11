# Changelog

## Unreleased

### Changed
- ADRs 0001 (hexagonal core), 0003 (RunPod offerings) and 0006 (two token scopes) accepted
- ADR 0007 (JSON config) rejected; ADR 0010 accepted: YAML config, secrets still env-only (not built yet)
- ADR 0005 (single server replica) accepted as temporary technical debt
- ADR 0004 (HMAC-derived runtime keys) rejected; ADR 0009 accepted: random per-deployment keys stored encrypted (not built yet)
- ADR 0002 (PostgreSQL via pgx, embedded migrations) accepted
- `CLAUDE.md` lists what must be done before the first real launch (SEC-004 fix, DB TLS, network policy checks, ADR review)

### Security
- Runtime images limited to an allowlist, remote model code and free-form vLLM flags off by default, deployments can no longer override image or code options (SEC-001)
- Production database URL verifies the server certificate (SEC-002)
- Build binaries and scratch files no longer tracked (SEC-003)
- vLLM image, base images and CI actions pinned, Dependabot keeps them current (SEC-005)
- Ingress rate limit and NetworkPolicies for the server and bot (SEC-006)

### Added
- Architecture decision records in `docs/adr/` (0001 to 0007, Proposed), first security audit in `docs/security/SECURITY_REPORT.md`, `CLAUDE.md` and this changelog
