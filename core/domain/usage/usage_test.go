package usage

import (
	"testing"
	"time"

	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/inference"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestUptimeRecordEstimatesCost(t *testing.T) {
	hourly := &compute.Money{Currency: "USD", Micros: 2_000_000}
	r := UptimeRecord("u1", "d1", "m1", hourly, 90*time.Minute, t0)
	if r.Kind != KindUptime || r.Cost == nil {
		t.Fatalf("unexpected record: %+v", r)
	}
	if r.Cost.Basis != BasisEstimated || r.Cost.Amount.Micros != 3_000_000 || r.Cost.Amount.Currency != "USD" {
		t.Fatalf("bad cost: %+v", r.Cost)
	}
}

func TestUptimeRecordUnknownRateIsNotZeroCost(t *testing.T) {
	r := UptimeRecord("u1", "d1", "m1", nil, time.Hour, t0)
	if r.Cost != nil {
		t.Fatalf("cost must stay nil (unknown) when the rate is unknown, got %+v", r.Cost)
	}
}

func inferenceRec(in, out int, d time.Duration, at time.Time) Record {
	return Record{Kind: KindInference, Outcome: OutcomeCompleted, Tokens: inference.TokenUsage{Input: in, Output: out}, Duration: d, At: at}
}

func TestSummarize(t *testing.T) {
	records := []Record{
		inferenceRec(10, 20, 2*time.Second, t0),
		inferenceRec(5, 5, time.Second, t0.Add(time.Hour)),
		UptimeRecord("u1", "d1", "m1", &compute.Money{Currency: "USD", Micros: 1_000_000}, time.Hour, t0),
		UptimeRecord("u2", "d1", "m1", nil, 30*time.Minute, t0),
		{Kind: KindUptime, Duration: time.Hour, At: t0, Cost: &Cost{Amount: compute.Money{Currency: "USD", Micros: 700_000}, Basis: BasisBilled}},
	}
	s := Summarize(records, TimeRange{})

	if s.Requests != 2 || s.InputTokens != 15 || s.OutputTokens != 25 || s.TotalTokens != 40 {
		t.Fatalf("inference totals wrong: %+v", s)
	}
	if s.InferenceTime != 3*time.Second {
		t.Fatalf("InferenceTime = %v", s.InferenceTime)
	}
	if s.Uptime != 150*time.Minute {
		t.Fatalf("Uptime = %v", s.Uptime)
	}
	if s.EstimatedMicros["USD"] != 1_000_000 {
		t.Fatalf("estimated = %v", s.EstimatedMicros)
	}
	if s.BilledMicros["USD"] != 700_000 {
		t.Fatalf("billed must be tracked separately, got %v", s.BilledMicros)
	}
	if s.UptimeRecordsWithUnknownCost != 1 {
		t.Fatalf("unknown-cost count = %d, want 1", s.UptimeRecordsWithUnknownCost)
	}
}

func TestSummarizeSeparatesCurrencies(t *testing.T) {
	records := []Record{
		UptimeRecord("a", "d", "m", &compute.Money{Currency: "USD", Micros: 1_000_000}, time.Hour, t0),
		UptimeRecord("b", "d", "m", &compute.Money{Currency: "EUR", Micros: 2_000_000}, time.Hour, t0),
	}
	s := Summarize(records, TimeRange{})
	if s.EstimatedMicros["USD"] != 1_000_000 || s.EstimatedMicros["EUR"] != 2_000_000 {
		t.Fatalf("currencies were mixed: %v", s.EstimatedMicros)
	}
}

func TestSummarizeTimeRange(t *testing.T) {
	records := []Record{
		inferenceRec(1, 1, 0, t0.Add(-time.Second)), // before
		inferenceRec(1, 1, 0, t0),                   // included (From inclusive)
		inferenceRec(1, 1, 0, t0.Add(time.Hour)),    // excluded (To exclusive)
	}
	s := Summarize(records, TimeRange{From: t0, To: t0.Add(time.Hour)})
	if s.Requests != 1 {
		t.Fatalf("Requests = %d, want 1", s.Requests)
	}
	if got := Summarize(records, TimeRange{From: t0}).Requests; got != 2 {
		t.Fatalf("open upper bound: %d, want 2", got)
	}
	if got := Summarize(records, TimeRange{To: t0}).Requests; got != 1 {
		t.Fatalf("open lower bound: %d, want 1", got)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil, TimeRange{})
	if s.Requests != 0 || s.EstimatedMicros == nil || s.BilledMicros == nil {
		t.Fatalf("empty summary should have usable maps: %+v", s)
	}
}
