# 0007. JSON config file, secrets only from the environment

- Status: Proposed
- Date: 2026-11-11

> Recorded from the implemented code and ARCHITECTURE.md. The design notes say these calls were
> delegated during implementation; this record is not approved until the owner accepts it.

## Context
Configuration includes offerings, models and policies; secrets must never land in a config file or ConfigMap.

## Decision
Config is a JSON file (unknown keys rejected). Every secret comes only from `PERSONAL_AI_*` environment variables (Kubernetes Secret in deployment). JSON was chosen because the build sandbox could not fetch a YAML module; structs already carry `yaml` tags.

## Alternatives
- YAML via `yaml.v3`: the original proposal; a loader-only change.
- Secrets in the config file: would end up in the ConfigMap.

## Rationale
No third-party dependency at the time; secrets separated by construction.

## Consequences
- Positive: secrets never in config.
- Negative: JSON has no comments. The original reason (no module access) no longer holds, since `pgx` is now fetched; reconsider YAML.

## References
ARCHITECTURE.md §6a deviation 1, §6b (Secrets); `config/config.go`.
