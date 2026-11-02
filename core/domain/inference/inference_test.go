package inference

import (
	"errors"
	"testing"

	"personal-ai/core/domain/model"
)

func f64(v float64) *float64 { return &v }
func ip(v int) *int          { return &v }

func validRequest() Request {
	return Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
}

func TestRequestValidate(t *testing.T) {
	if err := validRequest().Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	tests := map[string]func(*Request){
		"no messages":      func(r *Request) { r.Messages = nil },
		"unknown role":     func(r *Request) { r.Messages[0].Role = "robot" },
		"empty content":    func(r *Request) { r.Messages[0].Content = "" },
		"temperature low":  func(r *Request) { r.Params.Temperature = f64(-0.1) },
		"temperature high": func(r *Request) { r.Params.Temperature = f64(2.1) },
		"top_p zero":       func(r *Request) { r.Params.TopP = f64(0) },
		"top_p high":       func(r *Request) { r.Params.TopP = f64(1.1) },
		"max tokens zero":  func(r *Request) { r.Params.MaxTokens = ip(0) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := validRequest()
			mutate(&r)
			if err := r.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
}

func TestRequestValidateAcceptsBoundaries(t *testing.T) {
	r := validRequest()
	r.Params = Params{Temperature: f64(0), TopP: f64(1), MaxTokens: ip(1)}
	if err := r.Validate(); err != nil {
		t.Fatalf("boundary values rejected: %v", err)
	}
	r.Params.Temperature = f64(2)
	if err := r.Validate(); err != nil {
		t.Fatalf("temperature 2 rejected: %v", err)
	}
}

func TestWithDefaults(t *testing.T) {
	g := model.Generation{Temperature: f64(0.7), TopP: f64(0.9), MaxTokens: 2048}

	got := Params{}.WithDefaults(g)
	if *got.Temperature != 0.7 || *got.TopP != 0.9 || *got.MaxTokens != 2048 {
		t.Fatalf("defaults not applied: %+v", got)
	}

	explicit := Params{Temperature: f64(0), TopP: f64(0.5), MaxTokens: ip(10)}.WithDefaults(g)
	if *explicit.Temperature != 0 || *explicit.TopP != 0.5 || *explicit.MaxTokens != 10 {
		t.Fatalf("explicit values were overridden: %+v", explicit)
	}

	none := Params{}.WithDefaults(model.Generation{})
	if none.Temperature != nil || none.TopP != nil || none.MaxTokens != nil {
		t.Fatalf("nothing should be set when there are no defaults: %+v", none)
	}
}

func TestWithDefaultsDoesNotAliasModelDefaults(t *testing.T) {
	g := model.Generation{Temperature: f64(0.7)}
	p := Params{}.WithDefaults(g)
	*p.Temperature = 1.5
	if *g.Temperature != 0.7 {
		t.Fatal("mutating the request params changed the model's defaults")
	}
}

func TestTokenUsageTotal(t *testing.T) {
	if got := (TokenUsage{Input: 3, Output: 4}).Total(); got != 7 {
		t.Fatalf("Total = %d", got)
	}
}
