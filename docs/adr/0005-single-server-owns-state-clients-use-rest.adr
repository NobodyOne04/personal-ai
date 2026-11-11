# 0005. One server replica owns scheduling and state; CLI and Telegram use the REST API

- Status: Accepted (temporary, technical debt)
- Date: 2026-11-11

> Accepted by the owner on 2026-11-11 as a **temporary** decision: kept because there is no time
> to build an alternative now, not because it is the target design. Revisit when availability
> matters or before running more than one server; the replacement gets a new ADR that supersedes
> this one.

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
- Technical debt: in-flight request counts live in memory and the scheduler and reconciler
  assume a single process, so scaling out needs shared state (e.g. counts in PostgreSQL) and
  leader election before a second replica can run.

## References
ARCHITECTURE.md §6a A3, A4, A5; `deployments/kubernetes/base/server.yaml`.
