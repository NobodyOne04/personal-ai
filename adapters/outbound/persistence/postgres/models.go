package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
)

// ModelRepository implements ports.ModelRepository.
type ModelRepository struct {
	db     *pgxpool.Pool
	logger *slog.Logger
}

var _ ports.ModelRepository = (*ModelRepository)(nil)

// NewModelRepository returns a repository backed by the pool.
func NewModelRepository(db *pgxpool.Pool, logger *slog.Logger) *ModelRepository {
	if logger == nil {
		logger = slog.Default()
	}

	return &ModelRepository{
		db:     db,
		logger: logger,
	}
}

// Save inserts or replaces a model.
func (mr *ModelRepository) Save(ctx context.Context, m model.Model) error {
	features := make([]string, len(m.Features))
	for i, f := range m.Features {
		features[i] = string(f)
	}

	runtime, err := json.Marshal(m.Runtime)

	if err != nil {
		return fmt.Errorf("marshal model runtime: %w", err)
	}

	_, err = mr.db.Exec(ctx, `
		INSERT INTO model_catalog (
			id,
			name,
			artifact,
			description,
			family,
			param_count,
			min_vram_gb,
			recommended_vram_gb,
			min_gpu_count,
			features,
			temperature,
			top_p,
			max_tokens,
			runtime,
			status
		)
		VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15
		)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			artifact = EXCLUDED.artifact,
			description = EXCLUDED.description,
			family = EXCLUDED.family,
			param_count = EXCLUDED.param_count,
			min_vram_gb = EXCLUDED.min_vram_gb,
			recommended_vram_gb = EXCLUDED.recommended_vram_gb,
			min_gpu_count = EXCLUDED.min_gpu_count,
			features = EXCLUDED.features,
			temperature = EXCLUDED.temperature,
			top_p = EXCLUDED.top_p,
			max_tokens = EXCLUDED.max_tokens,
			runtime = EXCLUDED.runtime,
			status = EXCLUDED.status,
			updated_at = now()
	`,
		string(m.ID),
		m.Name,
		m.Artifact,
		m.Description,
		m.Family,
		m.ParamCount,
		m.Requirements.MinVRAMGB,
		m.Requirements.RecommendedVRAMGB,
		m.Requirements.MinGPUCount,
		features,
		nullableFloat(m.Generation.Temperature),
		nullableFloat(m.Generation.TopP),
		m.Generation.MaxTokens,
		runtime,
		string(m.Status),
	)

	if err != nil {
		mr.logger.Error("failed to save model",
			"model_id", m.ID,
			"error", err,
		)
		return fmt.Errorf("save model %q: %w", m.ID, err)
	}

	return nil
}

// Get returns a model or ports.ErrNotFound.
func (mr *ModelRepository) Get(
	ctx context.Context,
	id model.ID,
) (model.Model, error) {
	row := mr.db.QueryRow(ctx, `
		SELECT
			id,
			name,
			artifact,
			description,
			family,
			param_count,
			min_vram_gb,
			recommended_vram_gb,
			min_gpu_count,
			features,
			temperature,
			top_p,
			max_tokens,
			runtime,
			status
		FROM model_catalog
		WHERE id = $1
	`, string(id))

	m, err := scanModel(row)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Model{}, ports.ErrNotFound
		}

		return model.Model{}, fmt.Errorf("get model %q: %w", id, err)
	}

	return m, nil
}

// List returns all models ordered by ID.
func (mr *ModelRepository) List(ctx context.Context) ([]model.Model, error) {
	rows, err := mr.db.Query(ctx, `
		SELECT
			id,
			name,
			artifact,
			description,
			family,
			param_count,
			min_vram_gb,
			recommended_vram_gb,
			min_gpu_count,
			features,
			temperature,
			top_p,
			max_tokens,
			runtime,
			status
		FROM model_catalog
		ORDER BY id
	`)

	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	defer rows.Close()

	models := make([]model.Model, 0)

	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}

		models = append(models, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate models: %w", err)
	}

	return models, nil
}

// scanModel maps a PostgreSQL row into a domain model.
func scanModel(row pgx.Row) (model.Model, error) {
	var (
		m           model.Model
		id          string
		status      string
		features    []string
		temperature pgtype.Float8
		topP        pgtype.Float8
		runtime     []byte
	)

	err := row.Scan(
		&id,
		&m.Name,
		&m.Artifact,
		&m.Description,
		&m.Family,
		&m.ParamCount,
		&m.Requirements.MinVRAMGB,
		&m.Requirements.RecommendedVRAMGB,
		&m.Requirements.MinGPUCount,
		&features,
		&temperature,
		&topP,
		&m.Generation.MaxTokens,
		&runtime,
		&status,
	)

	if err != nil {
		return model.Model{}, err
	}

	m.ID = model.ID(id)
	m.Status = model.Status(status)

	m.Features = make([]model.Feature, len(features))
	for i, f := range features {
		m.Features[i] = model.Feature(f)
	}

	if temperature.Valid {
		v := temperature.Float64
		m.Generation.Temperature = &v
	}

	if topP.Valid {
		v := topP.Float64
		m.Generation.TopP = &v
	}

	if err := json.Unmarshal(runtime, &m.Runtime); err != nil {
		return model.Model{}, fmt.Errorf("unmarshal model runtime: %w", err)
	}

	return m, nil
}

func nullableFloat(v *float64) pgtype.Float8 {
	if v == nil {
		return pgtype.Float8{}
	}

	return pgtype.Float8{
		Float64: *v,
		Valid:   true,
	}
}
