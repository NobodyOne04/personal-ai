# 0008. Allowlist runtime images and gate remote model code

- Status: Accepted
- Date: 2026-11-11

## Context
SEC-001: the model `runtime` settings and a deployment's `runtime_config` could name any
container image, enable `trust_remote_code` or pass any `extra_args`. A holder of the management
token could run their own code on a RunPod GPU next to `HF_TOKEN` and `VLLM_API_KEY`.

## Decision
- `runtime.allowed_images` in config lists the images models may use; `runtime.image` is always
  allowed; anything else is refused when a deployment is prepared.
- `runtime.allow_trust_remote_code` (default `false`) gates `trust_remote_code` and
  `--trust-remote-code` in `extra_args`.
- A deployment's `runtime_config` cannot set `image`, `trust_remote_code` or `extra_args`; only
  the model (catalog) can.

## Alternatives
- Pass `HF_TOKEN` only to models that ask for it: still lets a custom image run arbitrary code.
- Restrict model registration to config only: removes the management API's model editing.

## Rationale
The operator decides, in config, which code may run on rented GPUs; the API can choose among
approved options only.

## Consequences
- Positive: a leaked management token can no longer run custom containers or remote code.
- Negative: a new vLLM image or a model needing remote code requires a config change and restart.

## References
`docs/security/SECURITY_REPORT.md` SEC-001; ADR 0004; `adapters/outbound/inference/vllm/vllm.go`.
