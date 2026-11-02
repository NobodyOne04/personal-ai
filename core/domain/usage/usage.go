// Package usage defines records of inference and infrastructure activity and
// the pure calculations over them. It depends on no provider's billing format.
package usage

import (
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
)

// Kind classifies a usage record.
type Kind string

const (
	KindInference Kind = "inference"
	KindUptime    Kind = "uptime"
)

// Outcome says how an inference request ended.
type Outcome string

const (
	OutcomeCompleted   Outcome = "completed"
	OutcomeInterrupted Outcome = "interrupted"
	OutcomeFailed      Outcome = "failed"
)

// CostBasis distinguishes figures we computed from figures a provider billed.
// Estimated and billed amounts must never be mixed.
type CostBasis string

const (
	BasisEstimated CostBasis = "estimated"
	BasisBilled    CostBasis = "billed"
)

// Cost is an amount with its provenance.
type Cost struct {
	Amount compute.Money
	Basis  CostBasis
}

// Record is one unit of recorded activity. Prompts and generated text are
// never part of a record.
type Record struct {
	ID           string
	Kind         Kind
	DeploymentID deployment.ID
	ModelID      model.ID
	Outcome      Outcome
	Tokens       inference.TokenUsage
	Duration     time.Duration
	// Cost is nil when the cost is unknown. Unknown is not zero.
	Cost *Cost
	At   time.Time
}

// UptimeRecord builds a record for deployment uptime. The cost is estimated
// from the instance's hourly rate; if the rate is unknown, Cost stays nil
// rather than being invented.
func UptimeRecord(id string, dep deployment.ID, modelID model.ID, hourly *compute.Money, uptime time.Duration, at time.Time) Record {
	r := Record{ID: id, Kind: KindUptime, DeploymentID: dep, ModelID: modelID, Duration: uptime, At: at}
	if hourly != nil {
		r.Cost = &Cost{Amount: hourly.ForDuration(uptime), Basis: BasisEstimated}
	}
	return r
}

// TimeRange selects records with From <= At < To. A zero bound is unbounded.
type TimeRange struct {
	From time.Time
	To   time.Time
}

func (tr TimeRange) contains(t time.Time) bool {
	if !tr.From.IsZero() && t.Before(tr.From) {
		return false
	}
	if !tr.To.IsZero() && !t.Before(tr.To) {
		return false
	}
	return true
}

// Summary aggregates records. Costs are kept per currency and per basis.
type Summary struct {
	Requests      int
	InputTokens   int64
	OutputTokens  int64
	TotalTokens   int64
	InferenceTime time.Duration
	Uptime        time.Duration

	EstimatedMicros map[string]int64
	BilledMicros    map[string]int64
	// UptimeRecordsWithUnknownCost counts uptime records whose cost could not
	// be determined, so consumers know the cost totals are incomplete.
	UptimeRecordsWithUnknownCost int
}

// Summarize aggregates the records that fall in the range.
func Summarize(records []Record, tr TimeRange) Summary {
	s := Summary{EstimatedMicros: map[string]int64{}, BilledMicros: map[string]int64{}}
	for _, r := range records {
		if !tr.contains(r.At) {
			continue
		}
		switch r.Kind {
		case KindInference:
			s.Requests++
			s.InputTokens += int64(r.Tokens.Input)
			s.OutputTokens += int64(r.Tokens.Output)
			s.TotalTokens += int64(r.Tokens.Total())
			s.InferenceTime += r.Duration
		case KindUptime:
			s.Uptime += r.Duration
			if r.Cost == nil {
				s.UptimeRecordsWithUnknownCost++
			}
		}
		if r.Cost != nil {
			switch r.Cost.Basis {
			case BasisEstimated:
				s.EstimatedMicros[r.Cost.Amount.Currency] += r.Cost.Amount.Micros
			case BasisBilled:
				s.BilledMicros[r.Cost.Amount.Currency] += r.Cost.Amount.Micros
			}
		}
	}
	return s
}
