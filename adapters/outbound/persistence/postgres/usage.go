package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/core/ports"
)

// UsageRepository implements ports.UsageRepository.
type UsageRepository struct {
	db *pgxpool.Pool
}

var _ ports.UsageRepository = (*UsageRepository)(nil)

// NewUsageRepository returns a repository backed by the pool.
func NewUsageRepository(db *pgxpool.Pool) *UsageRepository { return &UsageRepository{db: db} }

// Append stores a record. Unknown cost is stored as NULL, never zero.
func (r *UsageRepository) Append(ctx context.Context, rec usage.Record) error {
	var micros *int64
	var currency, basis *string
	if rec.Cost != nil {
		m, c, b := rec.Cost.Amount.Micros, rec.Cost.Amount.Currency, string(rec.Cost.Basis)
		micros, currency, basis = &m, &c, &b
	}
	var depID *string
	if rec.DeploymentID != "" {
		s := string(rec.DeploymentID)
		depID = &s
	}
	_, err := r.db.Exec(ctx, `INSERT INTO usage_records (
			id, kind, deployment_id, model_id, outcome, input_tokens, output_tokens,
			duration_ms, cost_micros, cost_currency, cost_basis, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		rec.ID, string(rec.Kind), depID, string(rec.ModelID), string(rec.Outcome),
		rec.Tokens.Input, rec.Tokens.Output, rec.Duration.Milliseconds(),
		micros, currency, basis, rec.At)
	if isForeignKeyViolation(err) {
		// The deployment was deleted meanwhile; keep the record without it.
		_, err = r.db.Exec(ctx, `INSERT INTO usage_records (
				id, kind, deployment_id, model_id, outcome, input_tokens, output_tokens,
				duration_ms, cost_micros, cost_currency, cost_basis, occurred_at)
			VALUES ($1,$2,NULL,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			rec.ID, string(rec.Kind), string(rec.ModelID), string(rec.Outcome),
			rec.Tokens.Input, rec.Tokens.Output, rec.Duration.Milliseconds(),
			micros, currency, basis, rec.At)
	}
	if err != nil {
		return fmt.Errorf("append usage record: %w", err)
	}
	return nil
}

// Summarize loads the records in range and aggregates them with the domain
// rules, so the database and the domain cannot disagree on what a summary
// means.
func (r *UsageRepository) Summarize(ctx context.Context, tr usage.TimeRange) (usage.Summary, error) {
	var from, to *time.Time
	if !tr.From.IsZero() {
		from = &tr.From
	}
	if !tr.To.IsZero() {
		to = &tr.To
	}
	rows, err := r.db.Query(ctx, `SELECT id, kind, deployment_id, model_id, outcome, input_tokens,
			output_tokens, duration_ms, cost_micros, cost_currency, cost_basis, occurred_at
		FROM usage_records
		WHERE ($1::timestamptz IS NULL OR occurred_at >= $1)
		  AND ($2::timestamptz IS NULL OR occurred_at < $2)`, from, to)
	if err != nil {
		return usage.Summary{}, fmt.Errorf("query usage: %w", err)
	}
	defer rows.Close()

	var records []usage.Record
	for rows.Next() {
		var (
			rec                 usage.Record
			kind, modelID, outc string
			depID               pgtype.Text
			in, out, durMS      int64
			micros              pgtype.Int8
			currency, costBasis pgtype.Text
		)
		if err := rows.Scan(&rec.ID, &kind, &depID, &modelID, &outc, &in, &out, &durMS,
			&micros, &currency, &costBasis, &rec.At); err != nil {
			return usage.Summary{}, fmt.Errorf("scan usage: %w", err)
		}
		rec.Kind = usage.Kind(kind)
		rec.DeploymentID = deployment.ID(depID.String)
		rec.ModelID = model.ID(modelID)
		rec.Outcome = usage.Outcome(outc)
		rec.Tokens = inference.TokenUsage{Input: int(in), Output: int(out)}
		rec.Duration = time.Duration(durMS) * time.Millisecond
		if micros.Valid {
			rec.Cost = &usage.Cost{
				Amount: compute.Money{Currency: currency.String, Micros: micros.Int64},
				Basis:  usage.CostBasis(costBasis.String),
			}
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return usage.Summary{}, fmt.Errorf("query usage: %w", err)
	}
	return usage.Summarize(records, tr), nil
}

// PreferenceRepository implements ports.PreferenceRepository.
type PreferenceRepository struct {
	db *pgxpool.Pool
}

var _ ports.PreferenceRepository = (*PreferenceRepository)(nil)

// NewPreferenceRepository returns a repository backed by the pool.
func NewPreferenceRepository(db *pgxpool.Pool) *PreferenceRepository {
	return &PreferenceRepository{db: db}
}

// Get returns the subject's preferences or ports.ErrNotFound.
func (r *PreferenceRepository) Get(ctx context.Context, subjectID string) (model.Preferences, error) {
	var def pgtype.Text
	err := r.db.QueryRow(ctx, `SELECT default_model_id FROM preferences WHERE subject_id = $1`, subjectID).Scan(&def)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Preferences{}, ports.ErrNotFound
	}
	if err != nil {
		return model.Preferences{}, fmt.Errorf("get preferences: %w", err)
	}
	return model.Preferences{SubjectID: subjectID, DefaultModelID: model.ID(def.String)}, nil
}

// Set upserts the subject's preferences.
func (r *PreferenceRepository) Set(ctx context.Context, p model.Preferences) error {
	var def *string
	if p.DefaultModelID != "" {
		s := string(p.DefaultModelID)
		def = &s
	}
	_, err := r.db.Exec(ctx, `INSERT INTO preferences (subject_id, default_model_id, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (subject_id) DO UPDATE SET default_model_id = EXCLUDED.default_model_id, updated_at = now()`,
		p.SubjectID, def)
	if isForeignKeyViolation(err) {
		return fmt.Errorf("set preferences: model %q: %w", p.DefaultModelID, ports.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("set preferences: %w", err)
	}
	return nil
}
