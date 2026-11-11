# personal-ai

Self-hosted LLM platform: deploys models with vLLM on RunPod GPUs on demand, serves an
OpenAI-compatible API, with a CLI and a Telegram bot. Go, PostgreSQL, Kubernetes.

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
- `cmd/{server,telegram,cli}`: wiring only. `config/`: JSON config + `PERSONAL_AI_*` env secrets.
- `internal/archtest`: fails the build on forbidden imports. `internal/testing/fakes`: a fake per port.
- `deployments/kubernetes`: base + `local`/`production` overlays.
- `ARCHITECTURE.md`: full design. `docs/adr/`: decision records. `docs/security/SECURITY_REPORT.md`: security baseline.

## Conventions
- Import rules in ARCHITECTURE.md §1 are enforced by `internal/archtest`; the core never imports adapters, `cmd` or `config`.
- Secrets only from env, never in config files or logs; responses never expose runtime endpoints or keys.
- Significant architecture changes get an ADR in `docs/adr/` (`adr-manager`); only the owner accepts one.
- The security report is updated in place after each audit (`security-audit`); never drop old findings.
- Exactly one server replica: it owns the scheduler, reconciler and in-memory activity counts.
- Changes go through a branch and PR (`github-flow`) with a `CHANGELOG.md` line.

## Gotchas
- `bin/`, `Untitled.ipynb` and `test.txt` are tracked by mistake (SEC-003); don't add more build output.
