package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
)

// DeploymentRepository implements ports.DeploymentRepository with optimistic
// locking on the version column.
type DeploymentRepository struct {
	db   *pgxpool.Pool
	keys keyCipher
}

var _ ports.DeploymentRepository = (*DeploymentRepository)(nil)

// NewDeploymentRepository returns a repository backed by the pool. keySecret
// encrypts runtime keys at rest (ADR 0009).
func NewDeploymentRepository(db *pgxpool.Pool, keySecret string) (*DeploymentRepository, error) {
	keys, err := newKeyCipher(keySecret)
	if err != nil {
		return nil, err
	}
	return &DeploymentRepository{db: db, keys: keys}, nil
}

const deploymentColumns = `id, model_id, instance_id, status, runtime_config, runtime_ref, endpoint,
	pinned, health, failure_stage, failure_reason, failed_at, idempotency_key, version,
	created_at, ready_at, last_activity_at, updated_at, runtime_key_ciphertext, runtime_key_version`

// endpointRecord is the stored form of compute.Endpoint.
type endpointRecord struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Save inserts a new deployment (Version 0) or updates an existing one whose
// stored version equals d.Version. The returned deployment carries the new
// version.
func (r *DeploymentRepository) Save(ctx context.Context, d deployment.Deployment) (deployment.Deployment, error) {
	runtimeCfg, err := json.Marshal(nonNilMap(d.RuntimeConfig))
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("marshal runtime config: %w", err)
	}
	var endpoint []byte
	if d.Endpoint != nil {
		if endpoint, err = json.Marshal(endpointRecord{URL: d.Endpoint.URL, Headers: d.Endpoint.Headers}); err != nil {
			return deployment.Deployment{}, fmt.Errorf("marshal endpoint: %w", err)
		}
	}
	var instanceID *string
	if d.InstanceID != nil {
		s := string(*d.InstanceID)
		instanceID = &s
	}
	var stage, reason *string
	var failedAt *time.Time
	if d.Failure != nil {
		st, rs, at := string(d.Failure.Stage), d.Failure.Reason, d.Failure.At
		stage, reason, failedAt = &st, &rs, &at
	}

	var keyCT []byte
	var keyVersion *int16
	if d.RuntimeKey != "" {
		keyCT = r.keys.seal(string(d.ID), string(d.RuntimeKey))
		v := int16(runtimeKeyVersion)
		keyVersion = &v
	}

	args := []any{
		string(d.ID), string(d.ModelID), instanceID, string(d.Status), runtimeCfg, d.RuntimeRef, endpoint,
		d.Pinned, string(d.Health), stage, reason, failedAt, d.IdempotencyKey,
		d.CreatedAt, d.ReadyAt, d.LastActivityAt, d.UpdatedAt, keyCT, keyVersion,
	}

	if d.Version == 0 {
		_, err := r.db.Exec(ctx, `INSERT INTO deployments (
			id, model_id, instance_id, status, runtime_config, runtime_ref, endpoint,
			pinned, health, failure_stage, failure_reason, failed_at, idempotency_key,
			created_at, ready_at, last_activity_at, updated_at, runtime_key_ciphertext, runtime_key_version, version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,1)`, args...)
		switch {
		case isUniqueViolation(err):
			return deployment.Deployment{}, fmt.Errorf("insert deployment %q: %w", d.ID, ports.ErrVersionConflict)
		case isForeignKeyViolation(err):
			return deployment.Deployment{}, fmt.Errorf("insert deployment %q: model %q: %w", d.ID, d.ModelID, ports.ErrNotFound)
		case err != nil:
			return deployment.Deployment{}, fmt.Errorf("insert deployment %q: %w", d.ID, err)
		}
		d.Version = 1
		return d, nil
	}

	tag, err := r.db.Exec(ctx, `UPDATE deployments SET
			model_id = $2, instance_id = $3, status = $4, runtime_config = $5, runtime_ref = $6,
			endpoint = $7, pinned = $8, health = $9, failure_stage = $10, failure_reason = $11,
			failed_at = $12, idempotency_key = $13, created_at = $14, ready_at = $15,
			last_activity_at = GREATEST(last_activity_at, $16), updated_at = $17,
			runtime_key_ciphertext = $18, runtime_key_version = $19,
			version = version + 1
		WHERE id = $1 AND version = $20`, append(args, d.Version)...)
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("update deployment %q: %w", d.ID, err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deployments WHERE id = $1)`, string(d.ID)).Scan(&exists); err != nil {
			return deployment.Deployment{}, fmt.Errorf("check deployment %q: %w", d.ID, err)
		}
		if !exists {
			return deployment.Deployment{}, fmt.Errorf("update deployment %q: %w", d.ID, ports.ErrNotFound)
		}
		return deployment.Deployment{}, fmt.Errorf("update deployment %q: %w", d.ID, ports.ErrVersionConflict)
	}
	// Reload so the caller sees the persisted LastActivityAt (it may have been
	// moved forward concurrently) and the new version.
	return r.Get(ctx, d.ID)
}

// Get returns the deployment or ports.ErrNotFound.
func (r *DeploymentRepository) Get(ctx context.Context, id deployment.ID) (deployment.Deployment, error) {
	return r.one(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id = $1`, string(id))
}

// FindByIdempotencyKey returns the deployment created with key.
func (r *DeploymentRepository) FindByIdempotencyKey(ctx context.Context, key string) (deployment.Deployment, error) {
	return r.one(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE idempotency_key = $1`, key)
}

// List returns all deployments, newest first.
func (r *DeploymentRepository) List(ctx context.Context) ([]deployment.Deployment, error) {
	return r.many(ctx, `SELECT `+deploymentColumns+` FROM deployments ORDER BY created_at DESC, id`)
}

// ListByStatus returns deployments in any of the statuses.
func (r *DeploymentRepository) ListByStatus(ctx context.Context, statuses ...deployment.Status) ([]deployment.Deployment, error) {
	ss := make([]string, len(statuses))
	for i, s := range statuses {
		ss[i] = string(s)
	}
	return r.many(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE status = ANY($1) ORDER BY created_at DESC, id`, ss)
}

// ListByModel returns deployments of the model.
func (r *DeploymentRepository) ListByModel(ctx context.Context, id model.ID) ([]deployment.Deployment, error) {
	return r.many(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE model_id = $1 ORDER BY created_at DESC, id`, string(id))
}

// RecordActivity moves last_activity_at forward without bumping the version.
func (r *DeploymentRepository) RecordActivity(ctx context.Context, id deployment.ID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE deployments SET last_activity_at = GREATEST(last_activity_at, $2) WHERE id = $1`, string(id), at)
	if err != nil {
		return fmt.Errorf("record activity for %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record activity for %q: %w", id, ports.ErrNotFound)
	}
	return nil
}

// Delete removes the record.
func (r *DeploymentRepository) Delete(ctx context.Context, id deployment.ID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM deployments WHERE id = $1`, string(id))
	if err != nil {
		return fmt.Errorf("delete deployment %q: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete deployment %q: %w", id, ports.ErrNotFound)
	}
	return nil
}

func (r *DeploymentRepository) one(ctx context.Context, sql string, args ...any) (deployment.Deployment, error) {
	d, err := r.scan(r.db.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return deployment.Deployment{}, ports.ErrNotFound
	}
	if err != nil {
		return deployment.Deployment{}, fmt.Errorf("get deployment: %w", err)
	}
	return d, nil
}

func (r *DeploymentRepository) many(ctx context.Context, sql string, args ...any) ([]deployment.Deployment, error) {
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()
	out := []deployment.Deployment{}
	for rows.Next() {
		d, err := r.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	return out, nil
}

func (r *DeploymentRepository) scan(row pgx.Row) (deployment.Deployment, error) {
	var (
		d                    deployment.Deployment
		id, modelID, status  string
		health               string
		instanceID           pgtype.Text
		runtimeCfg, endpoint []byte
		stage, reason        pgtype.Text
		failedAt, readyAt    pgtype.Timestamptz
		keyCT                []byte
		keyVersion           pgtype.Int2
	)
	err := row.Scan(&id, &modelID, &instanceID, &status, &runtimeCfg, &d.RuntimeRef, &endpoint,
		&d.Pinned, &health, &stage, &reason, &failedAt, &d.IdempotencyKey, &d.Version,
		&d.CreatedAt, &readyAt, &d.LastActivityAt, &d.UpdatedAt, &keyCT, &keyVersion)
	if err != nil {
		return deployment.Deployment{}, err
	}
	if len(keyCT) > 0 {
		if keyVersion.Int16 != runtimeKeyVersion {
			return deployment.Deployment{}, fmt.Errorf("deployment %q: unknown runtime key version %d", id, keyVersion.Int16)
		}
		key, err := r.keys.open(id, keyCT)
		if err != nil {
			return deployment.Deployment{}, fmt.Errorf("deployment %q: %w", id, err)
		}
		d.RuntimeKey = deployment.RuntimeKey(key)
	}
	d.ID = deployment.ID(id)
	d.ModelID = model.ID(modelID)
	d.Status = deployment.Status(status)
	d.Health = deployment.Health(health)
	if instanceID.Valid {
		v := compute.InstanceID(instanceID.String)
		d.InstanceID = &v
	}
	if len(runtimeCfg) > 0 {
		if err := json.Unmarshal(runtimeCfg, &d.RuntimeConfig); err != nil {
			return deployment.Deployment{}, fmt.Errorf("unmarshal runtime config: %w", err)
		}
		if len(d.RuntimeConfig) == 0 {
			d.RuntimeConfig = nil
		}
	}
	if len(endpoint) > 0 {
		var rec endpointRecord
		if err := json.Unmarshal(endpoint, &rec); err != nil {
			return deployment.Deployment{}, fmt.Errorf("unmarshal endpoint: %w", err)
		}
		d.Endpoint = &compute.Endpoint{URL: rec.URL, Headers: rec.Headers}
	}
	if stage.Valid || reason.Valid {
		d.Failure = &deployment.Failure{Stage: deployment.Status(stage.String), Reason: reason.String}
		if failedAt.Valid {
			d.Failure.At = failedAt.Time
		}
	}
	if readyAt.Valid {
		t := readyAt.Time
		d.ReadyAt = &t
	}
	return d, nil
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
