# 0002. PostgreSQL via pgx with an embedded migration runner

- Status: Accepted
- Date: 2026-11-11

> Accepted by the owner on 2026-11-11.

## Context
Deployments, the model catalog, preferences and usage must survive restarts, and lifecycle updates race with concurrent requests.

## Decision
PostgreSQL accessed through `pgx/v5`. Migrations are SQL files embedded in the binary and applied at startup by `postgres.Migrate` (advisory lock, one transaction per file). Deployment updates use optimistic locking; `RecordActivity` updates `last_activity_at` outside that lock.

## Alternatives
- `golang-migrate`: another dependency for a few dozen lines.
- SQLite: simpler, but no shared server for the Kubernetes deployment and weaker concurrency.

## Rationale
One dependency, migrations ship with the binary, and concurrent launch/stop cannot overwrite each other.

## Consequences
- Positive: no separate migration step at deploy time.
- Negative: the server needs DDL rights on startup; rollbacks are manual SQL.

## References
ARCHITECTURE.md §6b (Persistence, Orchestration), §7; `adapters/outbound/persistence/postgres`.
