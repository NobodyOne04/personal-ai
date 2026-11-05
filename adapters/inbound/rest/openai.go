package rest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/internal/apitypes"
)

func (s *server) openAIModels(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Inference.ListAvailableModels(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := apitypes.OpenAIModelList{Object: "list", Data: make([]apitypes.OpenAIModel, len(ms))}
	for i, m := range ms {
		out.Data[i] = apitypes.OpenAIModel{ID: string(m.Model.ID), Object: "model", OwnedBy: "personal-ai",
			Ready: m.Ready, Starting: m.Starting}
	}
	writeJSON(w, http.StatusOK, out)
}

func toDomainRequest(in apitypes.ChatRequest) inference.Request {
	msgs := make([]inference.Message, len(in.Messages))
	for i, m := range in.Messages {
		msgs[i] = inference.Message{Role: inference.Role(m.Role), Content: string(m.Content)}
	}
	return inference.Request{
		ModelID:  model.ID(in.Model),
		Messages: msgs,
		Params: inference.Params{Temperature: in.Temperature, TopP: in.TopP, MaxTokens: in.MaxTokens,
			Stop: []string(in.Stop)},
		Stream: in.Stream,
	}
}

func toUsage(u inference.TokenUsage) apitypes.Usage {
	return apitypes.Usage{PromptTokens: u.Input, CompletionTokens: u.Output, TotalTokens: u.Total()}
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var in apitypes.ChatRequest
	// Lenient: OpenAI clients send fields this server ignores (user, n, ...).
	if !s.decode(w, r, &in, false) {
		return
	}
	req := toDomainRequest(in)
	id := "chatcmpl-" + newRequestID()
	created := time.Now().Unix()

	if !in.Stream {
		res, err := s.Inference.Chat(r.Context(), s.Subject, req)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		resp := res.Response
		writeJSON(w, http.StatusOK, apitypes.ChatResponse{
			ID: id, Object: "chat.completion", Created: created, Model: string(resp.ModelID),
			Choices: []apitypes.ChatChoice{{
				Message:      apitypes.ChatMessage{Role: string(inference.RoleAssistant), Content: apitypes.ChatContent(resp.Message.Content)},
				FinishReason: string(resp.FinishReason),
			}},
			Usage: toUsage(resp.Usage),
		})
		return
	}

	// Streaming. Errors before the first byte are ordinary HTTP errors; after
	// that they are delivered as a terminal event.
	stream, err := s.Inference.ChatStream(r.Context(), s.Subject, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Streams may outlive the server's read deadline; lift it.
	_ = rc.SetReadDeadline(time.Time{})

	includeUsage := in.StreamOptions != nil && in.StreamOptions.IncludeUsage
	modelName := string(stream.ModelID)
	write := func(c apitypes.ChatChunk) bool {
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, modelName
		b, err := json.Marshal(c)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	if !write(apitypes.ChatChunk{Choices: []apitypes.ChunkChoice{{Delta: apitypes.Delta{Role: string(inference.RoleAssistant)}}}}) {
		drain(stream.Chunks)
		return
	}
	var usage *inference.TokenUsage
	for c := range stream.Chunks {
		if c.Err != nil {
			s.Logger.Warn("stream failed", "request_id", w.Header().Get("X-Request-ID"), "error", c.Err)
			write(apitypes.ChatChunk{Choices: []apitypes.ChunkChoice{}, Error: &apitypes.ErrorDetail{
				Message: "generation failed", Type: "server_error", Code: "generation_failed"}})
			drain(stream.Chunks)
			return
		}
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Delta == "" && c.FinishReason == "" {
			continue
		}
		choice := apitypes.ChunkChoice{Delta: apitypes.Delta{Content: c.Delta}}
		if c.FinishReason != "" {
			fr := string(c.FinishReason)
			choice.FinishReason = &fr
		}
		if !write(apitypes.ChatChunk{Choices: []apitypes.ChunkChoice{choice}}) {
			// Client went away; the request context is cancelled, which stops
			// generation upstream and records the request as interrupted.
			drain(stream.Chunks)
			return
		}
	}
	if includeUsage && usage != nil {
		u := toUsage(*usage)
		write(apitypes.ChatChunk{Choices: []apitypes.ChunkChoice{}, Usage: &u})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

func drain(ch <-chan inference.Chunk) {
	for range ch {
	}
}
