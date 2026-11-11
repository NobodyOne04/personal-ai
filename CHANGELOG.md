# Changelog

## Unreleased

### Security
- Runtime images limited to an allowlist, remote model code off by default, deployments can no longer override image or code options (SEC-001)
- Production database URL verifies the server certificate (SEC-002)
- Build binaries and scratch files no longer tracked (SEC-003)
- vLLM image, base images and CI actions pinned, Dependabot keeps them current (SEC-005)
- Ingress rate limit and NetworkPolicies for the server and bot (SEC-006)

### Added
- Architecture decision records in `docs/adr/` (0001 to 0007, Proposed), first security audit in `docs/security/SECURITY_REPORT.md`, `CLAUDE.md` and this changelog
