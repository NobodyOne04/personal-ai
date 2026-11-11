# Security Report

Persistent security baseline for personal-ai. Updated in place after every audit; findings keep
their IDs and stay listed after they are resolved.

## Latest audit

- **Date**: 2026-11-11 (first audit)
- **Commit**: `599c564` (main)
- **Scope**: full: Go application (REST, OpenAI-compatible API, Telegram bot, CLI client), RunPod
  and vLLM adapters, PostgreSQL adapter, config and secrets, Dockerfile, Kubernetes manifests,
  GitHub Actions CI, files tracked in git.
- **Overall assessment**: solid for a single-owner platform. No secrets in history, no injection
  or auth bypass found. The main risk is what a holder of the management token can run on
  RunPod (SEC-001). The rest is hardening.
- **Methodology and tools**: manual code and config review; `gitleaks` 8.x (git history and
  working tree, no leaks); `go vet` (clean).
- **Strengths observed**: bearer tokens compared as SHA-256 digests in constant time, separate
  management and inference scopes; secrets only from the environment; responses never include
  runtime endpoints or keys; log redaction of credential keys; Telegram allowlist (unknown users
  ignored); distroless non-root image, read-only root filesystem, dropped capabilities, no service
  account token; NetworkPolicy on PostgreSQL; request body limits; CI with `contents: read` and
  gitleaks on every PR.
- **Limitations, not verified**:
  - Dependency CVEs: `govulncheck`, `osv-scanner`, `trivy` and `semgrep` are not installed and were
    not installed during the audit. `pgx/v5` v5.12.0, `golang.org/x/*` and the Go standard library
    are unchecked.
  - `staticcheck` is built with an older Go than `go.mod` requires and could not run.
  - No dynamic testing; RunPod and vLLM behaviour inferred from code and their public docs.
  - Not read in depth: `adapters/inbound/cli`, SQL migrations, `internal/apiclient`.

## Findings

### SEC-001: Management API can launch any container image with the Hugging Face token

- **Severity**: Medium · **Confidence**: High · **Status**: Open
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

### SEC-002: Production database connection encrypts without verifying the server

- **Severity**: Low · **Confidence**: High · **Status**: Open
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: `deployments/kubernetes/overlays/production/secrets.env.example`
- **Evidence**: the template uses `sslmode=require`, which encrypts but accepts any server
  certificate.
- **Attack scenario**: an attacker on the network path between the cluster and the managed
  database impersonates it and reads or alters traffic, including credentials.
- **Impact**: database credential and data exposure; needs a network position.
- **Remediation**: `sslmode=verify-full` with `sslrootcert` pointing at the provider's CA.
- **References**: CWE-295.

### SEC-003: Build binaries and scratch files tracked in git

- **Severity**: Low · **Confidence**: High · **Status**: Open
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

### SEC-004: vLLM routes outside `/v1` are reachable without the runtime key

- **Severity**: Informational · **Confidence**: Medium · **Status**: Open
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: vLLM pods behind the RunPod HTTPS proxy (ADR 0004)
- **Evidence**: vLLM's `--api-key` guards the OpenAI routes only; routes like `/health`,
  `/metrics` and `/version` are served on the same port, exposed through the public proxy URL.
- **Impact**: someone who learns the pod's proxy URL sees the model and runtime metrics; no
  inference without the key. Not verified against a live pod.
- **Remediation**: accept, or put the runtime behind a small authenticating proxy in the pod.

### SEC-005: Images and actions pinned by tag, not digest

- **Severity**: Informational · **Confidence**: High · **Status**: Open
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

### SEC-006: No rate limiting and no NetworkPolicy for the server and bot

- **Severity**: Informational · **Confidence**: High · **Status**: Open
- **First detected**: 2026-11-11 · **Last verified**: 2026-11-11
- **Component**: `overlays/production/ingress.yaml`, `base/server.yaml`,
  `components/telegram/telegram.yaml`
- **Evidence**: the ingress sets no rate limit; only PostgreSQL has a NetworkPolicy.
- **Impact**: tokens are long random values, so guessing is impractical; the gap is resource use
  from unauthenticated floods and unrestricted egress if a pod is compromised.
- **Remediation**: `nginx.ingress.kubernetes.io/limit-rps` on the ingress; NetworkPolicies that
  allow the bot to reach only the server and Telegram, and the server only PostgreSQL and HTTPS.

## Audit history

| Date | Commit | Scope | New | Resolved | Reopened |
|---|---|---|---|---|---|
| 2026-11-11 | `599c564` | full (first audit) | SEC-001 to SEC-006 | none | none |
