# Security Report

Persistent security baseline for personal-ai. Updated in place after every audit; findings keep
their IDs and stay listed after they are resolved.

## Latest audit

- **Date**: 2026-11-11 (remediation re-check; first audit the same day)
- **Commit**: branch `bug/security-findings` on top of `55b5c34`
- **Scope**: full: Go application (REST, OpenAI-compatible API, Telegram bot, CLI client), RunPod
  and vLLM adapters, PostgreSQL adapter, config and secrets, Dockerfile, Kubernetes manifests,
  GitHub Actions CI, files tracked in git.
- **Overall assessment**: solid for a single-owner platform. No secrets in history, no injection
  or auth bypass in this codebase. The open risk is SEC-004: vLLM serves an unauthenticated
  inference route on the pod's public proxy URL. SEC-001, SEC-003 and SEC-005 are fixed;
  SEC-002 and SEC-006 are fixed in the repo and need checking on the real deployment.
- **Methodology and tools**: manual code and config review; `gitleaks` 8.x (git history and
  working tree, no leaks); `go vet` (clean); `govulncheck` (no known vulnerabilities in called
  code); vLLM v0.33.0 source read for SEC-004; `kubectl kustomize` render of both overlays.
- **Strengths observed**: bearer tokens compared as SHA-256 digests in constant time, separate
  management and inference scopes; secrets only from the environment; responses never include
  runtime endpoints or keys; log redaction of credential keys; Telegram allowlist (unknown users
  ignored); distroless non-root image, read-only root filesystem, dropped capabilities, no service
  account token; NetworkPolicy on PostgreSQL; request body limits; CI with `contents: read` and
  gitleaks on every PR.
- **Limitations, not verified**:
  - `govulncheck` ran with the local Go 1.27 toolchain; the image builds with Go 1.25, whose
    standard library was not checked separately. `osv-scanner`, `trivy` and `semgrep` not run.
  - `staticcheck` is built with an older Go than `go.mod` requires and could not run.
  - No dynamic testing; RunPod and vLLM behaviour inferred from code and their public docs.
  - Not read in depth: `adapters/inbound/cli`, SQL migrations, `internal/apiclient`.

## Findings

### SEC-001: Management API can launch any container image with the Hugging Face token

- **Severity**: Medium · **Confidence**: High · **Status**: Resolved
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: `adapters/outbound/inference/vllm/vllm.go` (`parseSettings`, `Prepare`),
  `POST /api/v1/models`, `PUT /api/v1/models/{id}`, `POST /api/v1/deployments` (`runtime_config`)
- **Evidence**: the model `runtime` map and the per-deployment `runtime_config` overrides accept
  `image`, `trust_remote_code` and `extra_args` without any allowlist. The pod environment always
  carries `VLLM_API_KEY` and, when configured, `HF_TOKEN` (`vllm.go`, `Prepare`).
- **Attack scenario**: someone who obtains the management token (leaked CLI config, a compromised
  machine running the CLI or bot) registers a model whose `image` is their own container, or
  enables `trust_remote_code` on a malicious Hugging Face repo, then deploys it. Their code runs
  on a RunPod GPU billed to the owner and can read `HF_TOKEN` from its environment.
- **Impact**: Hugging Face token theft; GPU spend up to `scheduling.max_active` on the declared
  offerings. Offerings in config cap the hardware type, not the code that runs on it.
- **Remediation**: keep an allowlist of runtime images in config and reject others when a model is
  registered or deployed; refuse `image`, `trust_remote_code` and `extra_args` in per-deployment
  `runtime_config`; pass `HF_TOKEN` only to models that need gated weights.
- **References**: CWE-829, CWE-94, OWASP ASVS V14.2.
- **Resolution** (2026-11-11): `runtime.allowed_images` (the default image is always allowed)
  and `runtime.allow_trust_remote_code` (default off) in config; `trust_remote_code` and
  `--trust-remote-code` in `extra_args` are refused unless enabled; a deployment's
  `runtime_config` can no longer set `image`, `trust_remote_code` or `extra_args`. Verified by
  `TestPrepareRestrictsWhatCodeRuns`. ADR 0008.

### SEC-002: Production database connection encrypts without verifying the server

- **Severity**: Low · **Confidence**: High · **Status**: In Progress
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: `deployments/kubernetes/overlays/production/secrets.env.example`
- **Evidence**: the template uses `sslmode=require`, which encrypts but accepts any server
  certificate.
- **Attack scenario**: an attacker on the network path between the cluster and the managed
  database impersonates it and reads or alters traffic, including credentials.
- **Impact**: database credential and data exposure; needs a network position.
- **Remediation**: `sslmode=verify-full` with `sslrootcert` pointing at the provider's CA.
- **References**: CWE-295.
- **Resolution** (2026-11-11): template now uses `sslmode=verify-full&sslrootcert=system`
  (supported by pgx v5.12). Still to do by the owner: update the real production `secrets.env`
  and confirm the server connects; providers with a private CA need their CA bundle instead.

### SEC-003: Build binaries and scratch files tracked in git

- **Severity**: Low · **Confidence**: High · **Status**: Resolved
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: `bin/server`, `bin/telegram`, `bin/personal-ai`, `Untitled.ipynb`,
  `.ipynb_checkpoints/`, `test.txt`
- **Evidence**: committed in `2c16aa5` and `e0e4000`; `.gitignore` does not cover `bin/`.
- **Attack scenario**: anyone cloning the repo can run binaries that cannot be reviewed or
  reproduced from source; a tampered binary in a future commit would go unnoticed in review.
- **Impact**: supply-chain trust; repo bloat. No secrets found in them (gitleaks).
- **Remediation**: `git rm --cached` these files, add `bin/`, `*.ipynb`, `.ipynb_checkpoints/` to
  `.gitignore`. Build with `make build` or the Dockerfile instead.
- **References**: CWE-506 (class), SLSA source integrity.
- **Resolution** (2026-11-11): files removed from the index (kept on disk) and added to
  `.gitignore`. They remain in history before this change; no secrets in them (gitleaks).

### SEC-004: vLLM serves unauthenticated inference on the pod's public proxy URL

- **Severity**: Medium (raised from Informational on 2026-11-11) · **Confidence**: High · **Status**: Open
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: vLLM pods behind the RunPod HTTPS proxy (ADR 0004)
- **Evidence**: vLLM v0.33.0 `vllm/entrypoints/serve/middleware/authenticate.py` checks the API
  key only for paths starting with `/v1`, `/v2`, `/inference`, `/cohere`.
  `vllm/entrypoints/launchers/api_server/routers.py` always registers the SageMaker routes, and
  `POST /invocations` dispatches to chat completions. `/metrics`, `/version`, `/docs` and
  `/tokenizer_info` are also open.
- **Attack scenario**: someone who learns a pod's proxy URL (`https://<pod-id>-8000.proxy.runpod.net`;
  pod IDs are random but not secret: RunPod console, logs, shared screenshots) sends chat
  requests to `/invocations` with no key, using the GPU and keeping it busy, so idle shutdown
  never triggers and billing continues.
- **Impact**: free use of the owner's GPU and running cost until `max_active`/manual stop; model
  metadata disclosure. Not verified against a live pod.
- **Remediation options** (architecture change, needs a decision, see `architecture-review`):
  1. Start vLLM through a small Python entrypoint that sets the middleware's guarded prefix to
     `/` before serving (the adapter already sends the key on every call, `/health` included).
     Smallest change; tied to vLLM internals, re-check on each vLLM upgrade.
  2. An authenticating reverse proxy in the pod in front of vLLM (custom image). Robust; adds an
     image to build and maintain.
  3. Reach pods over a private network (RunPod global networking / VPN) instead of the public
     proxy. Most robust; the server would need to join that network.
- **References**: CWE-306, OWASP API2:2023.

### SEC-005: Images and actions pinned by tag, not digest

- **Severity**: Informational · **Confidence**: High · **Status**: Resolved
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: `Dockerfile` (`golang:1.25-alpine`, `distroless/static-debian12:nonroot`),
  `components/postgres` (`postgres:16-alpine`), `config.example.json`
  (`vllm/vllm-openai:latest`), `.github/workflows/ci.yml` (`actions/checkout@v4`,
  `actions/setup-go@v5`)
- **Impact**: a moved tag changes what builds or runs without a code change. `:latest` on the
  vLLM image is the most exposed: every deployment can pull a different runtime.
- **Remediation**: pin the vLLM image to a version (or digest); pin actions and base images by
  digest with Dependabot/Renovate to update them.
- **References**: OpenSSF Scorecard "Pinned-Dependencies".
- **Resolution** (2026-11-11): vLLM image `v0.33.0` (what `:latest` pointed to); golang,
  distroless and postgres images pinned by digest; actions pinned by commit SHA
  (`checkout` v4.4.0, `setup-go` v5.6.0); `.github/dependabot.yml` updates Go modules, the
  Dockerfile and actions weekly. The PostgreSQL manifest digest may need manual bumps.

### SEC-006: No rate limiting and no NetworkPolicy for the server and bot

- **Severity**: Informational · **Confidence**: High · **Status**: In Progress
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: `overlays/production/ingress.yaml`, `base/server.yaml`,
  `components/telegram/telegram.yaml`
- **Evidence**: the ingress sets no rate limit; only PostgreSQL has a NetworkPolicy.
- **Impact**: tokens are long random values, so guessing is impractical; the gap is resource use
  from unauthenticated floods and unrestricted egress if a pod is compromised.
- **Remediation**: `nginx.ingress.kubernetes.io/limit-rps` on the ingress; NetworkPolicies that
  allow the bot to reach only the server and Telegram, and the server only PostgreSQL and HTTPS.
- **Resolution** (2026-11-11): ingress `limit-rps: 10`, `limit-connections: 20`;
  NetworkPolicies: server egress DNS/5432/443 (base), server ingress only from `ingress-nginx`
  and the bot (production), bot ingress none and egress DNS/server/443 (telegram component).
  Rendered with `kubectl kustomize`; not yet applied to a cluster. Needs a CNI that enforces
  NetworkPolicy and ingress-nginx in the `ingress-nginx` namespace.

## Audit history

| Date | Commit | Scope | New | Resolved | Reopened |
|---|---|---|---|---|---|
| 2026-11-11 | `599c564` | full (first audit) | SEC-001 to SEC-006 | none | none |
| 2026-11-11 | `bug/security-findings` | remediation re-check, vLLM routes | none (SEC-004 raised to Medium) | SEC-001, SEC-003, SEC-005 | none |
