# personal-ai

A self-hosted gateway for running open LLMs on rented GPUs, at the lowest practical cost.

You register models; the platform starts GPU instances on [RunPod](https://www.runpod.io) running
[vLLM](https://github.com/vllm-project/vllm) when you need them, serves them through an
**OpenAI-compatible API**, and shuts them down when they go idle. It is driven by a REST API, a CLI and a
Telegram bot.

- **OpenAI-compatible** `/v1/chat/completions` (streaming and non-streaming) and `/v1/models`, so existing
  OpenAI SDKs and tools work by changing the base URL.
- **Cost control**: idle deployments are stopped automatically, a hard cap limits how many run at once, and only
  hardware you approve in config is ever rented.
- **Never leaks compute**: every deployment is tracked through a strict state machine; failed launches release
  their GPU, and startup reconciliation terminates anything orphaned by a crash.
- **Usage accounting**: tokens, inference time, GPU uptime and estimated cost (estimated and billed costs are never
  mixed; unknown prices are reported as unknown, not zero).
- **Private by design**: prompts and answers are never stored or logged; runtime endpoints and credentials are never
  exposed through the API.

> **Status:** all planned phases are implemented and tested against PostgreSQL and HTTP stubs of RunPod and vLLM.
> It has not yet been exercised against live RunPod. Start with a small model and `max_active: 1`.

## Contents

- [How it works](#how-it-works)
- [Quick start (local)](#quick-start-local)
- [Configuration](#configuration)
- [Using the API](#using-the-api)
- [CLI](#cli)
- [Telegram bot](#telegram-bot)
- [Kubernetes](#kubernetes)
- [Development](#development)
- [Security notes](#security-notes)

## How it works

```text
 CLI ─┐                    ┌───────────────── personal-ai server ─────────────────┐
      ├── REST /api/v1 ───►│ REST adapter ─► use cases (models, deployments,      │──► PostgreSQL
 Bot ─┤   OpenAI /v1  ───► │                 inference, scheduler) ─► ports       │──► RunPod API
 SDKs ┘                    └──────────────────────────────────────────────────────┘──► vLLM on the pod
```

The code follows a hexagonal architecture: business rules live in `core/` and know nothing about HTTP, SQL,
RunPod or vLLM; adapters in `adapters/` implement them; `cmd/` wires everything together. Import rules are
enforced by a test. See [ARCHITECTURE.md](ARCHITECTURE.md) for the design and every decision taken.

**Deployment lifecycle:** `pending → provisioning → initializing → starting → ready ⇄ unhealthy → stopping →
stopped` (or `failed`). Creating a deployment returns immediately; the GPU pod boots and vLLM loads the model in
the background (typically a few minutes).

**When a request arrives for a model that is not running**, `routing.not_deployed_policy` decides:

| Policy | Behaviour |
|---|---|
| `reject` (default) | `503 model_not_deployed`. You start deployments explicitly. Never spends money on its own. |
| `start_and_return_pending` | Starts a deployment and answers `503 model_starting` with `Retry-After`. |
| `start_and_wait` | Starts a deployment and holds the request until it is ready (up to `routing.wait_timeout`). |

## Quick start (local)

Requirements: Go 1.25+, Docker, a RunPod API key.

```bash
# 1. Secrets come from the environment only, never from files in the repo.
export PERSONAL_AI_MANAGEMENT_TOKEN=$(openssl rand -hex 32)
export PERSONAL_AI_INFERENCE_TOKEN=$(openssl rand -hex 32)
export PERSONAL_AI_RUNTIME_KEY_SECRET=$(openssl rand -hex 32)
export PERSONAL_AI_RUNPOD_API_KEY=rpa_...
export PERSONAL_AI_HF_TOKEN=hf_...          # optional, for gated models

# 2. Describe your models and approved GPUs (start from the example).
cp config/config.example.yaml config/local.yaml   # then edit "models" and "compute.runpod.offerings"

# 3. Start PostgreSQL in Docker and run the server on :8080.
make run-server CONFIG=config/local.yaml
```

In another terminal:

```bash
make build
export PERSONAL_AI_API_URL=http://localhost:8080
./bin/personal-ai models list
./bin/personal-ai deploy my-model -wait     # rents a GPU: this costs money
./bin/personal-ai chat "Hello!"
./bin/personal-ai deployments list
./bin/personal-ai deployments stop <id>    # or let the idle timeout do it
```

## Configuration

Non-secret settings live in a YAML file (`PERSONAL_AI_CONFIG` or `-config`). Unknown keys are rejected so typos
fail loudly. See [`config/config.example.yaml`](config/config.example.yaml).

| Section | Key settings |
|---|---|
| `server` | `addr`, timeouts, `max_body_bytes` |
| `scheduling` | `interval` (tick), `idle_timeout` (stop after this long unused), `max_active` (hard cap) |
| `routing` | `not_deployed_policy`, `wait_timeout` |
| `compute.runpod` | `cloud_type` (`SECURE`/`COMMUNITY`), `offerings` (the only hardware that may be rented, with `hourly_usd` for cost estimates) |
| `deployment` | `provision_timeout`, `ready_timeout`, `poll_interval`, `storage_gb` |
| `runtime` | `image` (default `vllm/vllm-openai:v0.33.0`), `allowed_images` (other images models may name; anything else is refused), `allow_trust_remote_code` (default `false`), `allow_extra_args` (default `false`: free-form vLLM flags can load code). A deployment's `runtime_config` cannot change `image`, `trust_remote_code` or `extra_args` |
| `client` | `api_url`, `timeout` (used by the CLI and the bot) |
| `telegram` | `allowed_user_ids` (mandatory allowlist) |
| `models` | the catalog seeded on startup; `default_model` |

A model entry (under `models:`):

```yaml
- id: qwen2.5-7b
  name: Qwen 2.5 7B Instruct
  artifact: Qwen/Qwen2.5-7B-Instruct
  features: [chat]
  requirements: { min_vram_gb: 24, min_gpu_count: 1 }
  runtime: { max_model_len: 16384, dtype: auto, gpu_memory_utilization: 0.9 }
  generation: { temperature: 0.7, max_tokens: 2048 }
```

`requirements.min_vram_gb` is total VRAM across GPUs; the cheapest approved offering that satisfies it is chosen.
`runtime` keys map to vLLM flags: `max_model_len`, `dtype`, `gpu_memory_utilization`, `tensor_parallel_size`,
`quantization`, `trust_remote_code`, `enforce_eager`, `extra_args`, `image`, `port`.

On startup the config catalog is inserted into the database **only for models that do not exist yet**; after
that the database (and the API) is authoritative. Use `server -sync-models` to overwrite from the file.

### Environment variables

| Variable | Used by | Required |
|---|---|---|
| `PERSONAL_AI_DATABASE_URL` | server | yes |
| `PERSONAL_AI_MANAGEMENT_TOKEN` | server, CLI, bot (optional) | yes, ≥ 24 chars |
| `PERSONAL_AI_INFERENCE_TOKEN` | server, CLI, bot | yes, ≥ 24 chars, different from the management token |
| `PERSONAL_AI_RUNPOD_API_KEY` | server | yes |
| `PERSONAL_AI_RUNTIME_KEY_SECRET` | server | yes, ≥ 24 chars (encrypts the per-deployment vLLM keys stored in the database; changing it means restarting deployments) |
| `PERSONAL_AI_HF_TOKEN` | server | no (gated Hugging Face models) |
| `PERSONAL_AI_TELEGRAM_TOKEN` | bot | yes for the bot |
| `PERSONAL_AI_API_URL` | CLI, bot | no (default `http://localhost:8080`) |
| `PERSONAL_AI_CONFIG` | all | no |
| `PERSONAL_AI_SERVER_ADDR`, `PERSONAL_AI_LOG_LEVEL`, `PERSONAL_AI_LOG_FORMAT` | all | no |

## Using the API

Two bearer tokens with separate scopes: the **management token** for `/api/v1/*`, the **inference token** for
`/v1/*`. `GET /healthz` is unauthenticated.

### OpenAI-compatible inference

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $PERSONAL_AI_INFERENCE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-7b","stream":true,"messages":[{"role":"user","content":"Hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="<inference token>")
r = client.chat.completions.create(model="qwen2.5-7b", messages=[{"role": "user", "content": "Hi"}])
```

Omit `model` to use the default model. `GET /v1/models` lists active models with `ready`/`starting` flags.

### Management API

| Method & path | Purpose |
|---|---|
| `GET /api/v1/models[?all=true]` · `POST /api/v1/models` | list / register |
| `GET`·`PUT /api/v1/models/{id}` | get / replace |
| `POST /api/v1/models/{id}/activate`·`/deactivate` | availability |
| `GET /api/v1/models/{id}/validate` | which approved hardware it would run on, and the price |
| `GET`·`PUT /api/v1/preferences/default-model` | default model |
| `GET /api/v1/deployments[?status=ready]` · `POST /api/v1/deployments` | list / create (`{"model_id":"…","pinned":false}`, returns 202) |
| `GET`·`DELETE /api/v1/deployments/{id}` | get / delete (only when stopped) |
| `POST /api/v1/deployments/{id}/stop`·`/restart`·`/pin`·`/unpin`·`/health` | lifecycle |
| `GET /api/v1/usage[?from=RFC3339&to=RFC3339]` | tokens, uptime, estimated cost |

Errors use the OpenAI envelope `{"error":{"message","type","code"}}` with stable codes such as
`model_not_deployed`, `model_starting`, `deployment_limit_reached`, `no_default_model`, `not_found`.
**Pinned** deployments are never stopped for idleness.

## CLI

```text
personal-ai [-json] <command>
  health
  models list [-all] | get <id> | register <file|-> | update <file|-> | activate <id> | deactivate <id> | validate <id>
  default [<id>]
  deploy <model> [-pin] [-wait]
  deployments list [-status s] | get|stop|restart|delete|pin|unpin|health <id>
  usage [-since 24h | -from T -to T]
  chat [-model id] [-system text] [-no-stream] [message...]   # no message → interactive (/reset, /exit)
```

The CLI only talks to the REST API; it needs `PERSONAL_AI_API_URL` and the token(s) for the commands you use.

## Telegram bot

A separate process (`bin/telegram`) that uses the REST API. Only users listed in `telegram.allowed_user_ids`
are answered; everyone else is ignored silently. Commands: `/models`, `/model [id]`, `/reset`, `/status`,
`/deploy <model>`, `/stop <id>`, `/help`; any other text is a chat message. Management commands are enabled only
when the bot has `PERSONAL_AI_MANAGEMENT_TOKEN`. History is kept in memory per chat (last 20 messages).

## Kubernetes

Manifests live in [`deployments/kubernetes`](deployments/kubernetes) as a Kustomize base plus components and
overlays:

```text
deployments/kubernetes/
├── base/                    namespace, ConfigMap (config.yaml), server Deployment + Service
├── components/postgres/     in-cluster PostgreSQL StatefulSet + NetworkPolicy (server-only access)
├── components/telegram/     Telegram bot Deployment
└── overlays/
    ├── local/               base + in-cluster PostgreSQL, local image
    └── production/          base + Telegram + Ingress (TLS, streaming-friendly), external DB, registry image
```

**Local (minikube, kind, Docker Desktop):**

```bash
minikube start
make minikube-load                    # build personal-ai:dev and load it into the cluster
make k8s-secrets OVERLAY=local        # generates tokens + DB password into overlays/local/secrets.env
$EDITOR deployments/kubernetes/overlays/local/secrets.env   # set PERSONAL_AI_RUNPOD_API_KEY
$EDITOR deployments/kubernetes/base/config.yaml             # models and offerings
make k8s-apply OVERLAY=local
make k8s-port-forward                 # API on http://localhost:8080
```

**Production:** push an image (`make docker-build docker-push IMAGE=ghcr.io/you/personal-ai TAG=0.1.0`), set the
image, host and `config.yaml` in `overlays/production`, create `secrets.env` (or better, feed the
`personal-ai-secrets` Secret from a secret manager), then `make k8s-apply OVERLAY=production`.

Notes:

- The server runs as **exactly one replica** with the `Recreate` strategy: it owns the scheduler, the startup
  reconciler and in-flight request tracking. Do not scale it horizontally. The bot is single-replica too
  (Telegram long polling).
- Pods run as non-root with a read-only root filesystem, no service-account token and all capabilities dropped;
  the namespace enforces the `restricted` Pod Security Standard.
- `secrets.env` files are git-ignored. Config and Secret names carry content hashes, so changing them rolls the
  pods automatically.
- `make k8s-delete` removes the namespace, **including the in-cluster database**.

## Development

```bash
make help              # all targets
make check             # gofmt, vet, unit tests with -race
make test-integration  # + PostgreSQL repositories and a full server boot (Docker)
make docker-build      # 25 MB distroless image containing server, telegram and the CLI
```

| Path | What |
|---|---|
| `core/domain` | entities and rules (models, compute, deployment state machine, inference, usage) |
| `core/application` | use cases: `model`, `deployment` (orchestration, reconciliation), `inference` (routing), `scheduling` |
| `core/ports` | interfaces the core needs |
| `adapters/inbound` | `rest`, `cli`, `telegram` |
| `adapters/outbound` | `persistence/postgres` (+ embedded migrations), `compute/runpod`, `inference/vllm`, `messaging` |
| `internal/apitypes`, `internal/apiclient` | shared REST wire types and client |
| `internal/testing/fakes` | in-memory fakes of every port |
| `internal/archtest` | fails the build if a dependency rule is broken |

## Security notes

- Secrets are accepted only from the environment; they are redacted in logs and never written to the database.
- Tokens are compared as SHA-256 digests in constant time. Use long random values and rotate them by restarting.
- Each GPU deployment gets its own random vLLM API key, new on every launch and restart, stored in PostgreSQL
  encrypted (AES-256-GCM) with `PERSONAL_AI_RUNTIME_KEY_SECRET` (ADR 0009); the gateway is the only client that
  knows it. Deployments started before this change have no key: restart them.
- Always put the server behind TLS (the production overlay's Ingress) when it is reachable from the internet.

## License

[Apache 2.0](LICENSE)
