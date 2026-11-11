# 0003. RunPod compute with operator-declared offerings and label-based reconciliation

- Status: Accepted
- Date: 2026-11-11

> Accepted by the owner on 2026-11-11.

## Context
GPU instances cost money by the hour. The platform must never provision hardware the operator did not approve and must find orphaned pods after a crash.

## Decision
RunPod REST v1 via `net/http`. Hardware offerings are declared in config (`compute.runpod.offerings`), not discovered. Idempotency: pod name `<prefix><idempotency key>`, looked up before create. Deployment labels travel as `PAI_LABEL_*` env vars; reconciliation finds orphans from those labels, so no `compute_instances` table exists.

## Alternatives
- Discover offerings from the RunPod API: could provision unapproved, expensive GPUs.
- A `compute_instances` table: deferred until billing reconciliation needs it.

## Rationale
Spending stays bounded by config; crash recovery needs no extra state.

## Consequences
- Positive: no surprise hardware; restart-safe.
- Negative: new GPU types need a config change; orphan sweep runs only at startup and on interrupted launches.

## References
ARCHITECTURE.md §6a deviation 2, §6b (RunPod, Reconciliation); `adapters/outbound/compute/runpod`.
