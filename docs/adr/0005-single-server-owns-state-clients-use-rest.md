# 0005. One server replica owns scheduling and state; CLI and Telegram use the REST API

- Status: Proposed
- Date: 2026-11-11

> Recorded from the implemented code and ARCHITECTURE.md. The design notes say these calls were
> delegated during implementation; this record is not approved until the owner accepts it.

## Context
The scheduler, startup reconciler and in-flight request counts live in one process. Several entry points (HTTP, CLI, Telegram) need the same operations.

## Decision
`cmd/server` is the only process with database and RunPod credentials and runs exactly one replica (`Recreate` rollout). The CLI and the Telegram bot are REST clients through `internal/apiclient`, sharing wire types in `internal/apitypes`.

## Alternatives
- Telegram bot calling the core directly: a second process holding provider credentials and racing the scheduler.
- Leader election for several replicas: complexity a personal platform does not need.

## Rationale
Credentials sit in one pod; in-memory activity tracking stays correct.

## Consequences
- Positive: small blast radius for the bot; simple consistency.
- Negative: no high availability; a server restart drops in-flight streams.

## References
ARCHITECTURE.md §6a A3, A4, A5; `deployments/kubernetes/base/server.yaml`.
