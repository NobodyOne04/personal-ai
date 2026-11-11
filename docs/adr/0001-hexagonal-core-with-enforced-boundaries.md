# 0001. Hexagonal core with import boundaries enforced by a test

- Status: Proposed
- Date: 2026-11-11

> Recorded from the implemented code and ARCHITECTURE.md. The design notes say these calls were
> delegated during implementation; this record is not approved until the owner accepts it.

## Context
The platform talks to several replaceable technologies (RunPod, vLLM, PostgreSQL, Telegram, HTTP). Business rules (deployment lifecycle, idle scheduling, cost accounting) must stay testable without any of them.

## Decision
Ports and adapters: `core/domain` (stdlib only, no I/O) ← `core/application` → `core/ports`; inbound and outbound adapters depend on the core, never on each other; only `cmd/*` wires adapters and reads `config/`. The rules are enforced by `internal/archtest/boundaries_test.go`, which fails the build on a forbidden import.

## Alternatives
- Plain layered packages without enforcement: drifts silently.
- External linter (depguard): extra tool in CI for what one `go list` test does.

## Rationale
Vendor swaps (another GPU cloud, another runtime) touch one adapter. Every port has a fake in `internal/testing/fakes`, so orchestration failure paths are unit-tested.

## Consequences
- Positive: core tested without network or database; adapters are swappable.
- Negative: more packages and mapping code than a flat service; new contributors must learn the rules.

## References
ARCHITECTURE.md §1, §2, §9; `internal/archtest`.
