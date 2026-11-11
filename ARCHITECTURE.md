# personal-ai — Architecture Proposal

Status: **Phases 1–8 implemented** (foundation, persistence, RunPod, vLLM, deployment orchestration, REST +
OpenAI-compatible API, scheduling/usage, CLI + Telegram). The design below was proposed first; open questions were
then resolved by the project owner delegating the call (see §6a, §6b). Where the code differs from the proposal
text in §3–§7, §6a and §6b are authoritative and list each difference.
Workspace state when the design was written: empty (greenfield).

> The Core owns the business rules. Adapters own the technology. `cmd/` owns the wiring.

---

## 1. Dependency Rules

```text
cmd/*  ──►  adapters/*  ──►  core/application  ──►  core/domain
   │                              │
   │                              └──►  core/ports  ──►  core/domain
   └─────────────►  core/application, core/ports (wiring only)
```

Hard rules (enforce in CI with a `go list`/depguard-style import check):

1. `core/domain` imports only the standard library (e.g. `time`, `errors`, `fmt`). It performs no I/O, so it needs no `context`.
2. `core/ports` imports `core/domain` and the standard library only.
3. `core/application` imports `core/domain`, `core/ports`, and the standard library only.
4. `core` never imports anything under `adapters/`, `cmd/`, or `config/`.
5. Inbound adapters import `core/application` (use-case types) and `core/domain` (for DTO mapping). They never import outbound adapters.
6. Outbound adapters import `core/ports` and `core/domain` only. They never import inbound adapters or each other.
7. Only `cmd/*` imports both inbound and outbound adapters, plus `config/`.
8. `config/` imports the standard library and YAML/env libs. Core never reads config directly; `cmd/` translates config into plain values passed to constructors.

## 2. Package Responsibilities

| Package | Responsibility | Must not contain |
|---|---|---|
| `core/domain/model` | `Model` entity, `Requirements`, `GenerationDefaults`, validation | runtime-specific parsing |
| `core/domain/compute` | `Capability`, `Instance`, `InstanceID`, `ProvisionSpec`, fit/matching rules | provider IDs as types |
| `core/domain/deployment` | `Deployment` aggregate, `Status`, transition table, `Pin`, activity tracking | I/O |
| `core/domain/inference` | `Request`, `Message`, `Params`, `Response`, `Chunk`, `TokenUsage` | wire formats |
| `core/domain/usage` | `Record`, cost estimate vs. billed cost types | provider billing formats |
| `core/application/model` | Model management use cases | HTTP, SQL |
| `core/application/deployment` | Deployment lifecycle orchestration, routing/resolution, reconciliation | vendor clients |
| `core/application/scheduling` | Idle detection, limits, pin policy (pure decisions + a `Tick` use case) | timers/cron (the trigger lives in `cmd/`) |
| `core/ports` | Interfaces the Core needs (see §5) | vendor-shaped methods |
| `adapters/inbound/rest` | Management API `/api/v1`, OpenAI-compatible `/v1`, auth, validation, error mapping | business decisions |
| `adapters/inbound/telegram` | Bot commands, allowlist, per-user chat state mapping | lifecycle logic |
| `adapters/inbound/cli` | Commands; talks to REST API (see ambiguity A3) | business logic |
| `adapters/outbound/compute/runpod` | RunPod HTTP client, mapping, retries, idempotency | leaking RunPod types |
| `adapters/outbound/inference/vllm` | Runtime bootstrap spec, OpenAI-protocol client, SSE parsing | leaking vLLM schemas |
| `adapters/outbound/persistence/postgres` | Repositories, record mapping, tx support | domain logic |
| `adapters/outbound/messaging` | In-process event bus | broker deps |
| `cmd/server` | Wires REST + inference + scheduler loop + reconciler | logic |
| `cmd/telegram` | Wires Telegram bot | logic |
| `cmd/cli` | Wires CLI | logic |

## 3. Domain Entities and Value Objects

**Model** (`model.Model`): `ID`, `Name`, `Artifact` (opaque string ref), `Description`, `Family`, `ParamCount`, `Requirements{MinVRAM, RecommendedVRAM, MinGPUCount}`, `Capabilities` (chat, completion, tools…), `Generation` defaults, `Runtime` (opaque `map[string]any`, interpreted only by the runtime adapter), `Status` (active/deactivated).
Rule: `Requirements.SatisfiedBy(compute.Capability) bool`.

**Compute** (`compute`): `Capability{ID, GPUModel, GPUCount, VRAMPerGPU, CPU, MemoryGB, StorageGB, HourlyCost *Money, Region}`, `ProvisionSpec{Capability ref, StorageGB, Labels, IdempotencyKey}`, `Instance{ID InstanceID, ProviderRef opaque string, Capability, Status, Endpoint opaque Endpoint, CreatedAt, HourlyCost}`, `InstanceStatus{Requested, Provisioning, Running, Terminating, Terminated, Failed, Unknown}` (platform-owned states; adapters map provider states to these).

**Deployment** (`deployment.Deployment`): `ID`, `ModelID`, `InstanceID` (nullable until provisioned), `Status`, `RuntimeConfig` (opaque), `Endpoint`, `Pinned bool`, `ActiveRequests int` (see ambiguity A5), `CreatedAt`, `ReadyAt`, `LastActivityAt`, `Health`, `Failure *Failure`, `IdempotencyKey`, `Version` (optimistic locking).
`Status`: Pending, Provisioning, Initializing, Starting, Ready, Unhealthy, Stopping, Stopped, Failed.
Transitions (single table in `deployment` package; the only way to change status is `d.Transition(to, now)`):

```text
Pending → Provisioning | Failed
Provisioning → Initializing | Stopping | Failed
Initializing → Starting | Stopping | Failed
Starting → Ready | Stopping | Failed
Ready → Unhealthy | Stopping
Unhealthy → Ready | Stopping | Failed
Stopping → Stopped | Failed
Stopped → Pending            (restart)
Failed → Stopping | Pending  (cleanup / retry)
```

**Inference** (`inference`): `Request{ID, ModelID, Messages []Message, Params, Stream bool}`, `Message{Role, Content}`, `Params{Temperature, TopP, MaxTokens, Stop…}`, `Response{Message, FinishReason, Usage}`, `Chunk{Delta, FinishReason, Usage*}`, `TokenUsage{Input, Output, Total}`.

**Usage** (`usage.Record`): `ID`, `Kind` (inference | uptime), `DeploymentID`, `ModelID`, tokens, `Duration`, `Outcome` (completed | interrupted | failed), `Cost{Amount, Currency, Basis: Estimated | Billed}`, `At`. Only `Estimated` is produced initially; `Billed` exists as a type so figures are never conflated.

**Preferences**: `Preferences{SubjectID, DefaultModelID}` — keyed by an opaque subject string (`"owner"` for now), which keeps multi-user possible.

## 4. Application Use Cases

Model management: `RegisterModel`, `UpdateModel`, `DeactivateModel`, `ListModels`, `GetModel`, `ValidateRequirements` (does any known capability satisfy it?), `SetDefaultModel`, `GetDefaultModel`.

Deployment: `CreateDeployment`, `StartDeployment`, `StopDeployment`, `RestartDeployment`, `DeleteDeployment`, `GetDeployment`, `ListDeployments`, `FindReadyDeployment(modelID)`, `CheckHealth`, `PinDeployment`/`UnpinDeployment`, `Reconcile` (startup).

Inference: `Chat` (non-stream), `ChatStream`, `ListAvailableModels`. Both resolve model → policy (`Reject | StartAndWait | StartAndReturnPending`) → ready deployment → runtime port, bracket the call with active-request tracking, and record usage on completion *or* interruption.

Scheduling: `Tick(now)` — detect idle, respect pins and active requests, enforce `MaxActive`, clean up Failed deployments. Invoked by a ticker in `cmd/server`.

Usage: `GetUsageSummary(range)`, `RecordUptime` (called from `Tick`).

**CreateDeployment orchestration with recovery** (key reliability design):
1. Load model; reject if inactive. 2. Derive compute need; pick a `Capability`. 3. Persist Deployment `Pending` with a generated idempotency key *before* any external call. 4. Transition `Provisioning`; call `Compute.Provision(key)`; persist `InstanceID` immediately. 5. `Initializing` → `Runtime.Initialize`. 6. `Starting` → poll `Runtime.Health` until ready or timeout. 7. `Ready`; publish event.
On failure at steps 4–6: if an `InstanceID` is recorded, call `Terminate` (idempotent); if terminate fails, deployment stays `Failed` with the instance retained so the reconciler/scheduler retries cleanup. A deployment is never dropped while an instance is attached.

## 5. Ports (`core/ports`)

```go
type ComputeProvider interface {
    Capabilities(ctx) ([]compute.Capability, error)
    Provision(ctx, compute.ProvisionSpec) (compute.Instance, error)   // idempotent on spec.IdempotencyKey
    GetInstance(ctx, compute.InstanceID) (compute.Instance, error)
    ListInstances(ctx) ([]compute.Instance, error)                    // needed for reconciliation / orphan detection
    Terminate(ctx, compute.InstanceID) error                          // idempotent
}

type InferenceRuntime interface {
    Initialize(ctx, InitSpec{Model, Instance, RuntimeConfig}) (RuntimeHandle, error)
    Health(ctx, RuntimeHandle) (Health, error)
    Models(ctx, RuntimeHandle) ([]string, error)
    Chat(ctx, RuntimeHandle, inference.Request) (inference.Response, error)
    ChatStream(ctx, RuntimeHandle, inference.Request) (<-chan inference.Chunk, error) // closes on ctx cancel; errors delivered as terminal Chunk.Err
    Release(ctx, RuntimeHandle) error
}

type ModelRepository interface { Save; Get; List; }
type DeploymentRepository interface { Save (optimistic version); Get; List; FindByModel; ListByStatus; }
type UsageRepository interface { Append; Summarize(range) }
type PreferenceRepository interface { Get; Set }
type EventPublisher interface { Publish(ctx, Event) error }
type Clock interface { Now() time.Time }
type IDGenerator interface { NewID() string }
```

Small additions only when needed: `TxRunner` (if a use case must update deployment + usage atomically) — to be added in Phase 2 only if a concrete use case requires it. `ActivityTracker` is *not* a port; active-request count is domain state (see A5).

## 6. Ambiguities and Missing Requirements

- **A1. Runtime bootstrapping on RunPod.** Who starts vLLM? Likely the compute adapter provisions a pod from a container image supplied via generic `ProvisionSpec` (image + command + ports), and the vLLM adapter *produces* that launch spec in `Initialize`. The port boundary needs a generic `LaunchSpec{Image, Args, Env, Ports}` in the domain. **Decision needed:** confirm this split.
- **A2. Network reachability.** How does the gateway reach a pod's vLLM port: RunPod proxy URL, public TCP, or a tunnel? Affects `Endpoint` shape and security (spec §14 forbids exposing runtime endpoints). Proposal: opaque `Endpoint{URL, Headers}` produced by the compute adapter; runtime API key generated per deployment.
- **A3. CLI transport.** Spec allows REST or shared use cases. Proposal: **REST only**, so the CLI needs no DB/RunPod credentials. Telegram, by contrast, runs in `cmd/telegram` with in-process use cases (needs DB access), or could also use REST. **Decision needed:** Telegram via direct use cases or via REST? REST-only for both is simpler and safer, at the cost of one hop.
- **A4. Process topology.** Is `cmd/telegram` a separate process sharing the DB, or part of `cmd/server`? If separate, two processes could run schedulers/reconcilers concurrently; only `cmd/server` should own those.
- **A5. Active-request tracking across restarts/processes.** In-memory counters are lost on restart and not shared. Proposal: counter in-process for single-instance server, plus `LastActivityAt` persisted; idle check requires `ActiveRequests==0` *and* idle timeout elapsed. Revisit if multiple server replicas are ever needed.
- **A6. Model-not-deployed policy default.** Auto-start spends money. Proposal default: `Reject` with a clear error; `StartAndWait`/`StartAndReturnPending` opt-in per config, with a configured cost ceiling (`MaxActive`).
- **A7. Context length vs. VRAM.** `min_vram_gb` alone may not cover KV-cache needs at long context. Proposal: treat requirements as declared by the operator; no estimation formula in Core.
- **A8. Multi-GPU / multiple deployments of the same model.** Proposal: allowed by data model; routing picks the least-recently-used Ready deployment; load balancing deferred.
- **A9. Pricing.** No pricing port in v1. Hourly cost comes from `Capability.HourlyCost` as reported by the adapter; if absent, cost is recorded as unknown, not zero.
- **A10. Model catalog source of truth.** YAML seeds the registry at startup; after that, the DB is authoritative and the API mutates it. Need a rule for YAML-vs-DB conflicts. Proposal: YAML entries are upserted only if absent unless `--sync-models` is given.
- **A11. Auth.** Single static bearer token (hashed comparison, constant time) for REST and inference; separate token scopes (management vs. inference) are desirable, so two tokens are proposed.
- **A12. Go version, router (chi vs. `net/http`), Telegram library, migration tool.** Proposal: latest stable Go, standard `net/http` (1.22+ pattern routing), `pgx`, `golang-migrate`, `go-telegram/bot` or `telebot` (to be chosen in Phase 8), `gopkg.in/yaml.v3`.

## 6a. Decisions taken in Phase 1

The owner asked for these calls to be made without further consultation; each can be revisited.

| # | Decision |
|---|---|
| A1 | **Confirmed.** The runtime adapter produces a generic `compute.LaunchSpec` via a new port method `InferenceRuntime.Prepare` (no network I/O); the compute adapter executes it when provisioning. `Initialize` then binds the runtime to the provisioned instance and returns an opaque ref; `Health` is polled until the model is loaded. |
| A2 | **Confirmed.** `compute.Endpoint{URL, Headers}` is produced by the compute adapter; `String()` omits headers so it is safe to log. A per-deployment runtime key is generated in Phase 4/5. |
| A3 | **REST only** for both the CLI and Telegram. Only `cmd/server` holds database and provider credentials. |
| A4 | Only `cmd/server` owns the scheduler and reconciler; `cmd/telegram` is a separate process using REST. |
| A5 | **Changed.** In-flight request counts are *not* a field on `Deployment` (entities are reloaded from storage as copies, so a counter on them would be meaningless). They live in `deployment.Activity`, an in-memory concurrency-safe tracker in the domain package (pure, no I/O). `Deployment.IsIdle` takes the count as an argument; `scheduling.PlanActions` takes an `ActiveRequests` func. Last activity is still persisted. |
| A6 | Default not-deployed policy is `reject`; `start_and_wait` and `start_and_return_pending` are opt-in via `routing.not_deployed_policy`. |
| A7 | Requirements are operator-declared (`min_vram_gb` is *total* VRAM across GPUs); no estimation in the Core. |
| A8 | Multiple deployments per model are allowed by the data model; load balancing deferred. |
| A9 | `Capability.HourlyCost` is a nullable pointer. Unknown cost stays nil and is counted separately in `usage.Summary`; it is never recorded as zero. Estimated and billed costs are kept in separate fields and currencies are never summed together. |
| A10 | Config seeds the catalog; the database becomes authoritative in Phase 2. The conflict rule (insert-only unless an explicit sync flag is passed) will be implemented with the repository. |
| A11 | Two distinct bearer tokens (management, inference), each ≥ 24 characters and different from each other, enforced by `Config.ValidateForServer`. Enforcement in the HTTP layer arrives in Phase 6. |
| A12 | Go 1.24 toolchain, standard `net/http` pattern routing (no Chi), `log/slog`. See "Deviations" for YAML and later dependencies. |

### Deviations from the proposal text

1. **Config format was JSON, not YAML (temporary; replaced by YAML in ADR 0010).** The sandbox's egress policy denies `proxy.golang.org`, so no third-party module could be fetched. `config.Duration` implements `encoding.TextUnmarshaler` and every struct already has `yaml` tags, so moving to `yaml.v3` is a loader-only change (`readFile`). Unknown keys are rejected, and secrets can only come from the environment.
2. **`compute_instances` table deferred.** Phase 1 adds no instance repository port; a deployment records `InstanceID` before any later step, and the idempotency key plus `ComputeProvider.ListInstances` (labels echo the deployment ID) are what reconciliation uses to find orphans. The table in §7 is only added if Phase 5 shows a concrete need; until then treat the `compute_instances` DDL and the `deployments.instance_id` foreign key in §7 as provisional.
3. **Domain invariants beyond the transition table**, enforced in `Deployment.Transition`: entering `Initializing` requires an attached instance; entering `Ready` requires an endpoint; entering `Pending` or `Stopped` requires that no instance is attached (a deployment can never be recorded as stopped while still holding compute). A `Failed` deployment keeps its instance until released.
4. **`Pending → Stopping` added** to the transition table so a deployment can be cancelled before provisioning.
5. **Application packages `core/application/model` and `core/application/deployment`** currently contain only a package doc (`modelapp`, `deploymentapp`); their use cases belong to Phases 2 and 5. Nothing critical is stubbed: the `telegram` and `cli` executables exit non-zero with an explicit "not implemented" message.
6. **Extra packages introduced:** `internal/logging` (slog setup with credential-key redaction) and `internal/archtest` (fails the build when the import rules in §1 are violated; the rules table is itself unit-tested).
7. **Ports differ slightly from the sketch in §5:** `ComputeProvider.ListInstances`, `InferenceRuntime.Prepare`, `DeploymentRepository.FindByIdempotencyKey`, and a `RuntimeTarget{Endpoint, Ref}` value passed to runtime calls. Persistence ports return `ErrNotFound`/`ErrVersionConflict`.

## 7. Proposed Database Schema (PostgreSQL)

```sql
CREATE TABLE models (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  artifact      TEXT NOT NULL,
  description   TEXT NOT NULL DEFAULT '',
  family        TEXT,
  param_count   BIGINT,
  min_vram_gb   INT  NOT NULL,
  rec_vram_gb   INT,
  min_gpu_count INT  NOT NULL DEFAULT 1,
  capabilities  TEXT[] NOT NULL DEFAULT '{}',
  generation    JSONB NOT NULL DEFAULT '{}',
  runtime       JSONB NOT NULL DEFAULT '{}',   -- opaque; interpreted by runtime adapter
  status        TEXT NOT NULL CHECK (status IN ('active','deactivated')),
  created_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE compute_instances (
  id            TEXT PRIMARY KEY,              -- platform ID
  provider      TEXT NOT NULL,                 -- adapter name, e.g. 'runpod'
  provider_ref  TEXT NOT NULL,                 -- opaque, adapter-owned
  capability    JSONB NOT NULL,                -- GPU model/count/VRAM/etc. snapshot
  status        TEXT NOT NULL,
  endpoint      JSONB,
  hourly_cost   NUMERIC(12,6),
  currency      TEXT,
  created_at    TIMESTAMPTZ NOT NULL,
  terminated_at TIMESTAMPTZ,
  UNIQUE (provider, provider_ref)
);

CREATE TABLE deployments (
  id               TEXT PRIMARY KEY,
  model_id         TEXT NOT NULL REFERENCES models(id),
  instance_id      TEXT REFERENCES compute_instances(id),
  status           TEXT NOT NULL,
  runtime_config   JSONB NOT NULL DEFAULT '{}',
  runtime_handle   JSONB,
  endpoint         JSONB,
  pinned           BOOLEAN NOT NULL DEFAULT FALSE,
  health           TEXT NOT NULL DEFAULT 'unknown',
  failure_reason   TEXT,
  failure_stage    TEXT,
  idempotency_key  TEXT NOT NULL UNIQUE,
  version          BIGINT NOT NULL DEFAULT 0,   -- optimistic locking
  created_at       TIMESTAMPTZ NOT NULL,
  ready_at         TIMESTAMPTZ,
  last_activity_at TIMESTAMPTZ NOT NULL,
  updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX deployments_status_idx ON deployments (status);
CREATE INDEX deployments_model_idx  ON deployments (model_id, status);

CREATE TABLE usage_records (
  id             TEXT PRIMARY KEY,
  kind           TEXT NOT NULL CHECK (kind IN ('inference','uptime')),
  deployment_id  TEXT REFERENCES deployments(id) ON DELETE SET NULL,
  model_id       TEXT,
  outcome        TEXT,
  input_tokens   BIGINT, output_tokens BIGINT, total_tokens BIGINT,
  duration_ms    BIGINT,
  cost_amount    NUMERIC(14,6),
  cost_currency  TEXT,
  cost_basis     TEXT CHECK (cost_basis IN ('estimated','billed')),  -- NULL = unknown
  occurred_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX usage_time_idx ON usage_records (occurred_at);

CREATE TABLE preferences (
  subject_id       TEXT PRIMARY KEY,
  default_model_id TEXT REFERENCES models(id),
  updated_at       TIMESTAMPTZ NOT NULL
);
```

Prompts and generated text are intentionally **not** stored.

## 8. Composition Roots

**`cmd/server`**: load `config` → init `slog` → open Postgres pool (+ run/verify migrations) → build `runpod.Client`, `vllm.Runtime`, `postgres` repos, in-process event bus, system clock, ID generator → construct application services (model, deployment, inference, scheduling) → build REST handler (management + inference, auth middleware) → start HTTP server, scheduler ticker (calls `scheduling.Tick`), and run `Reconcile` once before accepting traffic → graceful shutdown on SIGINT/SIGTERM (stop accepting, drain streams with deadline, stop ticker).

**`cmd/telegram`**: load config → init logging → (per A3) either build the same service graph as the server or an HTTP client to the REST API → build Telegram adapter with allowlist of user IDs → long-poll → graceful shutdown.

**`cmd/cli`**: load config (API URL + token) → construct REST client → Cobra-free command dispatch (stdlib `flag` subcommands, to avoid a dependency) → streaming chat renders SSE to stdout; Ctrl-C cancels context.

## 9. Testing Approach (summary)

Fakes for every port live in `internal/testing/fakes` (or per-package `_test` helpers), including a fake compute provider that can inject delays/failures and a fake runtime that streams scripted chunks. Domain transition table is tested exhaustively (table-driven). Orchestration failure paths (provision ok → init fails → cleanup, cleanup fails → retained) are unit tests. Import-boundary check runs as a test (`go list -deps`) so the dependency rules in §1 are enforced automatically.

## 6b. Decisions taken in Phases 2–8

| Area | Decision |
|---|---|
| Persistence | `pgx/v5` with a small embedded migration runner (`postgres.Migrate`, advisory-locked, one transaction per file) instead of `golang-migrate`. The catalog table is `model_catalog`; generation defaults are columns, not JSONB. `compute_instances` remains deferred (deviation 2 holds: reconciliation works from instance labels). |
| Ports | `DeploymentRepository` gained `RecordActivity` (moves `last_activity_at` forward without taking part in optimistic locking, so concurrent requests never conflict with lifecycle updates) and `Delete`. `PrepareSpec`/`InitSpec` carry `DeploymentID`. `ports.ErrRuntimeUnavailable` added. |
| RunPod | REST v1 via `net/http`. Hardware **offerings are declared in config** (`compute.runpod.offerings`), not discovered: nothing the operator has not approved is ever provisioned. Idempotency = pod name `<prefix><idempotency key>` looked up before create; labels travel as `PAI_LABEL_*` env vars. Endpoint = RunPod HTTPS proxy. `EXITED` pods map to `failed` so they are cleaned up. |
| vLLM / A2 | Per-deployment runtime key = HMAC-SHA256(`PERSONAL_AI_RUNTIME_KEY_SECRET`, deployment ID), passed as `VLLM_API_KEY` env. No runtime credential is stored anywhere. The runtime ref holds only deployment ID + served model name. |
| Orchestration | Every status change goes through a reload→mutate→save loop with optimistic locking; a launch and a concurrent stop cannot overwrite each other. `Create` returns the Pending record (HTTP 202) and launches in the background. A launch interrupted between "provider created the pod" and "ID persisted" is caught by a label sweep (`releaseUnattached`). Restart rotates the idempotency key. Uptime usage is recorded when compute is released (not per tick), costed from the instance's hourly rate, unknown if absent. |
| Reconciliation | Pending → relaunched; mid-launch → Failed("interrupted by restart") then released; Stopping / Failed-with-instance → stop retried; Ready whose instance vanished → stopped; unreferenced platform instances → terminated. Failures are logged, not fatal. |
| Inference | New package `core/application/inference` (`inferenceapp`). Routing picks the least recently used Ready deployment. Under `reject`, a model that is already starting yields `model_starting` (503 + Retry-After) rather than `model_not_deployed`. Streams record usage as completed / failed / interrupted. |
| Scheduling | `scheduling.Scheduler.Tick` health-checks serving deployments, stops idle ones via `StopIfIdle` (the idle check is repeated inside the optimistic update, so an arriving request is never cut off), and retries cleanup of failed ones. |
| REST | stdlib `net/http`. Two bearer scopes compared as SHA-256 digests in constant time. Deployment responses never include endpoints or credentials. Error codes are stable strings (`model_not_deployed`, `deployment_limit_reached`, …). |
| Clients / A3 | Wire types live in `internal/apitypes` (stdlib only) and are shared by the server and `internal/apiclient`, which the CLI and Telegram bot both use. Telegram uses the Bot API directly (no library), ignores non-allowlisted users silently, keeps per-chat history in memory only, sends plain text. Management commands are enabled only if the bot holds a management token. |
| Secrets | New env-only secrets: `PERSONAL_AI_RUNTIME_KEY_SECRET` (required, ≥ 24 chars), `PERSONAL_AI_HF_TOKEN` (optional); `PERSONAL_AI_RUNPOD_API_KEY` is now required by the server. `PERSONAL_AI_API_URL` overrides `client.api_url`. |
| Seeding / A10 | Implemented: config models are inserted if absent; `server -sync-models` overwrites. `default_model` from config is applied only when no default is stored. |

## 10. Running and Next Steps

- `make test` runs unit tests; `make test-integration` starts a disposable PostgreSQL in Docker and also runs the
  repository tests and a full server boot (each test uses its own schema).
- Not yet verified against live RunPod/vLLM: the adapters are tested against faithful HTTP stubs. Before first real
  use, deploy one small model with a low `max_active` and watch the logs.
- Possible follow-ups: YAML config loader (now that modules can be fetched), periodic orphan sweep (today only at
  startup and on interrupted launches), a `compute_instances` table if billing reconciliation needs it, and the
  reserved `adapters/inbound/web` UI.
