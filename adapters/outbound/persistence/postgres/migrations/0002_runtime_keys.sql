-- ADR 0009: each deployment's random runtime key, encrypted with AES-256-GCM.
-- Deployments launched before this migration have no key and must be restarted.
ALTER TABLE deployments
  ADD COLUMN runtime_key_ciphertext BYTEA,
  ADD COLUMN runtime_key_version    SMALLINT;
