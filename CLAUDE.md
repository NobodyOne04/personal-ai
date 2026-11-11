# personal-ai

Self-hosted LLM platform: deploys models with vLLM on RunPod GPUs on demand, serves an
OpenAI-compatible API, with a CLI and a Telegram bot. Go, PostgreSQL, Kubernetes.

## Before the first real launch
Not yet deployed against real RunPod/production. When the owner wants to launch, stop and go
through this list first; don't deploy until each item is done or the owner explicitly waives it.
1. **SEC-004 (Medium, open)**: vLLM serves `/invocations` (chat) without the API key on the pod's
   public RunPod URL. Implement a fix first; options in `docs/security/SECURITY_REPORT.md`
   (recommended: entrypoint that guards every route). Needs the owner's choice and an ADR.
2. **SEC-002**: production `secrets.env` DB URL must use `sslmode=verify-full` (plus the
   provider's CA in `sslrootcert` if it isn't publicly trusted); confirm the server connects.
3. **SEC-006**: after the first deploy, check the NetworkPolicies and rate limit: bot reaches the
   server, server reaches PostgreSQL and RunPod, ingress still streams.
4. **ADR 0009** (random per-deployment runtime keys, stored encrypted) is accepted but not built:
   implement it.
5. Config: models with a custom `image`, `trust_remote_code` or `extra_args` need
   `runtime.allowed_images` / `allow_trust_remote_code` / `allow_extra_args`.
6. Start with one small model and a low `scheduling.max_active`, and watch the logs (ARCHITECTURE.md §10).
Update the security report statuses as items are verified, then remove this section.

## Commands
- `make check`: gofmt, `go vet`, `go test -race`, gitleaks. CI runs it on every PR.
- `make test-integration`: also PostgreSQL tests and a full server boot (Docker, port 55432).
- `make build`: `bin/server`, `bin/telegram`, `bin/personal-ai`.
- `make run-server` / `make run-telegram`: run locally; secrets come from your environment.
- `make k8s-secrets|k8s-render|k8s-apply OVERLAY=local|production`: Kubernetes.
- `make help`: everything else.

## Layout
- `core/domain`, `core/application`, `core/ports`: business rules; no I/O, stdlib only.
- `adapters/inbound/{rest,telegram,cli}`, `adapters/outbound/{compute/runpod,inference/vllm,persistence/postgres,messaging}`.
- `cmd/{server,telegram,cli}`: wiring only. `config/`: YAML config (ADR 0010) + `PERSONAL_AI_*` env secrets.
- `internal/archtest`: fails the build on forbidden imports. `internal/testing/fakes`: a fake per port.
- `deployments/kubernetes`: base + `local`/`production` overlays.
- `ARCHITECTURE.md`: full design. `docs/adr/`: decision records. `docs/security/SECURITY_REPORT.md`: security baseline.

## Conventions
- Import rules in ARCHITECTURE.md §1 are enforced by `internal/archtest`; the core never imports adapters, `cmd` or `config`.
- Secrets only from env, never in config files or logs; responses never expose runtime endpoints or keys.
- Significant architecture changes get an ADR in `docs/adr/` (`adr-manager`); only the owner accepts one.
- The security report is updated in place after each audit (`security-audit`); never drop old findings.
- Exactly one server replica: it owns the scheduler, reconciler and in-memory activity counts.
  Temporary, known tech debt (ADR 0005); never scale it out without a superseding ADR.
- Changes go through a branch and PR (`github-flow`) with a `CHANGELOG.md` line.

## Gotchas
- Runtime images must be in `runtime.allowed_images` (or be `runtime.image`); a deployment's
  `runtime_config` cannot change `image`, `trust_remote_code` or `extra_args`; `extra_args` needs
  `runtime.allow_extra_args` (ADR 0008). Never filter vLLM flags with a deny list: its parser
  accepts abbreviations and underscores.
- Base images and actions are pinned by digest/SHA; let Dependabot bump them, don't go back to tags.
