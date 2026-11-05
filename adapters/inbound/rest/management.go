package rest

import (
	"fmt"
	"net/http"
	"time"

	deploymentapp "personal-ai/core/application/deployment"
	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/model"
	"personal-ai/core/domain/usage"
	"personal-ai/internal/apitypes"
)

// ---- mapping ----

func modelToDTO(m model.Model) apitypes.Model {
	feats := make([]string, len(m.Features))
	for i, f := range m.Features {
		feats[i] = string(f)
	}
	return apitypes.Model{
		ID: string(m.ID), Name: m.Name, Artifact: m.Artifact, Description: m.Description, Family: m.Family,
		ParamCount: m.ParamCount, Status: string(m.Status), Features: feats,
		Requirements: apitypes.Requirements{MinVRAMGB: m.Requirements.MinVRAMGB,
			RecommendedVRAMGB: m.Requirements.RecommendedVRAMGB, MinGPUCount: m.Requirements.MinGPUCount},
		Generation: apitypes.Generation{Temperature: m.Generation.Temperature, TopP: m.Generation.TopP,
			MaxTokens: m.Generation.MaxTokens},
		Runtime: m.Runtime,
	}
}

func modelFromDTO(d apitypes.Model) model.Model {
	status := model.Status(d.Status)
	if status == "" {
		status = model.StatusActive
	}
	feats := make([]model.Feature, 0, len(d.Features))
	for _, f := range d.Features {
		feats = append(feats, model.Feature(f))
	}
	if len(feats) == 0 {
		feats = []model.Feature{model.FeatureChat}
	}
	minGPUs := d.Requirements.MinGPUCount
	if minGPUs == 0 {
		minGPUs = 1
	}
	return model.Model{
		ID: model.ID(d.ID), Name: d.Name, Artifact: d.Artifact, Description: d.Description, Family: d.Family,
		ParamCount: d.ParamCount, Status: status, Features: feats,
		Requirements: model.Requirements{MinVRAMGB: d.Requirements.MinVRAMGB,
			RecommendedVRAMGB: d.Requirements.RecommendedVRAMGB, MinGPUCount: minGPUs},
		Generation: model.Generation{Temperature: d.Generation.Temperature, TopP: d.Generation.TopP,
			MaxTokens: d.Generation.MaxTokens},
		Runtime: d.Runtime,
	}
}

func (s *server) deploymentToDTO(d deployment.Deployment) apitypes.Deployment {
	out := apitypes.Deployment{
		ID: string(d.ID), ModelID: string(d.ModelID), Status: string(d.Status), Health: string(d.Health),
		Pinned: d.Pinned, HasInstance: d.InstanceID != nil, RuntimeConfig: d.RuntimeConfig,
		ActiveRequests: s.Deployments.ActiveRequests(d.ID),
		CreatedAt:      d.CreatedAt, ReadyAt: d.ReadyAt, LastActivityAt: d.LastActivityAt,
	}
	if d.InstanceID != nil {
		out.InstanceID = string(*d.InstanceID)
	}
	if d.Failure != nil {
		out.Failure = &apitypes.Failure{Stage: string(d.Failure.Stage), Reason: d.Failure.Reason, At: d.Failure.At}
	}
	return out
}

func capabilityToDTO(c compute.Capability) apitypes.Capability {
	out := apitypes.Capability{ID: string(c.ID), GPUModel: c.GPUModel, GPUCount: c.GPUCount, VRAMPerGPUGB: c.VRAMPerGPUGB}
	if c.HourlyCost != nil {
		v := c.HourlyCost.Micros
		out.HourlyCostMicros, out.Currency = &v, c.HourlyCost.Currency
	}
	return out
}

// ---- models ----

func (s *server) listModels(w http.ResponseWriter, r *http.Request) {
	ms, err := s.Models.List(r.Context(), r.URL.Query().Get("all") == "true")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := apitypes.ModelList{Models: make([]apitypes.Model, len(ms))}
	for i, m := range ms {
		out.Models[i] = modelToDTO(m)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) registerModel(w http.ResponseWriter, r *http.Request) {
	var in apitypes.Model
	if !s.decode(w, r, &in, true) {
		return
	}
	m, err := s.Models.Register(r.Context(), modelFromDTO(in))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, modelToDTO(m))
}

func (s *server) getModel(w http.ResponseWriter, r *http.Request) {
	m, err := s.Models.Get(r.Context(), model.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, modelToDTO(m))
}

func (s *server) updateModel(w http.ResponseWriter, r *http.Request) {
	var in apitypes.Model
	if !s.decode(w, r, &in, true) {
		return
	}
	id := r.PathValue("id")
	if in.ID == "" {
		in.ID = id
	}
	if in.ID != id {
		writeError(w, http.StatusBadRequest, "id_mismatch", "the model id cannot be changed")
		return
	}
	m, err := s.Models.Update(r.Context(), modelFromDTO(in))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, modelToDTO(m))
}

func (s *server) deactivateModel(w http.ResponseWriter, r *http.Request) {
	m, err := s.Models.Deactivate(r.Context(), model.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, modelToDTO(m))
}

func (s *server) activateModel(w http.ResponseWriter, r *http.Request) {
	m, err := s.Models.Activate(r.Context(), model.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, modelToDTO(m))
}

func (s *server) validateModel(w http.ResponseWriter, r *http.Request) {
	c, err := s.Models.ValidateRequirements(r.Context(), model.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, capabilityToDTO(c))
}

func (s *server) getDefaultModel(w http.ResponseWriter, r *http.Request) {
	m, err := s.Models.GetDefault(r.Context(), s.Subject)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.DefaultModel{ModelID: string(m.ID)})
}

func (s *server) setDefaultModel(w http.ResponseWriter, r *http.Request) {
	var in apitypes.DefaultModel
	if !s.decode(w, r, &in, true) {
		return
	}
	if err := s.Models.SetDefault(r.Context(), s.Subject, model.ID(in.ModelID)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// ---- deployments ----

func (s *server) listDeployments(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Deployments.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := r.URL.Query().Get("status")
	out := apitypes.DeploymentList{Deployments: []apitypes.Deployment{}}
	for _, d := range ds {
		if status == "" || string(d.Status) == status {
			out.Deployments = append(out.Deployments, s.deploymentToDTO(d))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) createDeployment(w http.ResponseWriter, r *http.Request) {
	var in apitypes.CreateDeployment
	if !s.decode(w, r, &in, true) {
		return
	}
	if in.ModelID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "model_id is required")
		return
	}
	d, err := s.Deployments.Create(r.Context(), deploymentapp.CreateRequest{
		ModelID: model.ID(in.ModelID), Pinned: in.Pinned, RuntimeConfig: in.RuntimeConfig})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/deployments/"+string(d.ID))
	writeJSON(w, http.StatusAccepted, s.deploymentToDTO(d))
}

func (s *server) getDeployment(w http.ResponseWriter, r *http.Request) {
	d, err := s.Deployments.Get(r.Context(), deployment.ID(r.PathValue("id")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.deploymentToDTO(d))
}

func (s *server) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	if err := s.Deployments.Delete(r.Context(), deployment.ID(r.PathValue("id"))); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deploymentAction adapts a lifecycle use case to a handler.
func (s *server) deploymentAction(status int, act func(*http.Request, deployment.ID) (deployment.Deployment, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := act(r, deployment.ID(r.PathValue("id")))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, status, s.deploymentToDTO(d))
	}
}

func (s *server) stopDeployment(w http.ResponseWriter, r *http.Request) {
	s.deploymentAction(http.StatusOK, func(r *http.Request, id deployment.ID) (deployment.Deployment, error) {
		return s.Deployments.Stop(r.Context(), id)
	})(w, r)
}

func (s *server) restartDeployment(w http.ResponseWriter, r *http.Request) {
	s.deploymentAction(http.StatusAccepted, func(r *http.Request, id deployment.ID) (deployment.Deployment, error) {
		return s.Deployments.Restart(r.Context(), id)
	})(w, r)
}

func (s *server) pinDeployment(w http.ResponseWriter, r *http.Request) {
	s.deploymentAction(http.StatusOK, func(r *http.Request, id deployment.ID) (deployment.Deployment, error) {
		return s.Deployments.Pin(r.Context(), id)
	})(w, r)
}

func (s *server) unpinDeployment(w http.ResponseWriter, r *http.Request) {
	s.deploymentAction(http.StatusOK, func(r *http.Request, id deployment.ID) (deployment.Deployment, error) {
		return s.Deployments.Unpin(r.Context(), id)
	})(w, r)
}

func (s *server) checkDeploymentHealth(w http.ResponseWriter, r *http.Request) {
	s.deploymentAction(http.StatusOK, func(r *http.Request, id deployment.ID) (deployment.Deployment, error) {
		return s.Deployments.CheckHealth(r.Context(), id)
	})(w, r)
}

// ---- usage ----

func (s *server) usageSummary(w http.ResponseWriter, r *http.Request) {
	var tr usage.TimeRange
	for name, dst := range map[string]*time.Time{"from": &tr.From, "to": &tr.To} {
		v := r.URL.Query().Get(name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_time", fmt.Sprintf("%s must be RFC 3339", name))
			return
		}
		*dst = t
	}
	if !tr.From.IsZero() && !tr.To.IsZero() && !tr.From.Before(tr.To) {
		writeError(w, http.StatusBadRequest, "invalid_time", "from must be before to")
		return
	}
	sum, err := s.Inference.UsageSummary(r.Context(), tr)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := apitypes.UsageSummary{
		Requests: sum.Requests, InputTokens: sum.InputTokens, OutputTokens: sum.OutputTokens,
		TotalTokens: sum.TotalTokens, InferenceSeconds: sum.InferenceTime.Seconds(), UptimeSeconds: sum.Uptime.Seconds(),
		EstimatedCostMicros: sum.EstimatedMicros, BilledCostMicros: sum.BilledMicros,
		UptimeRecordsWithUnknownCost: sum.UptimeRecordsWithUnknownCost,
	}
	if !tr.From.IsZero() {
		out.From = &tr.From
	}
	if !tr.To.IsZero() {
		out.To = &tr.To
	}
	writeJSON(w, http.StatusOK, out)
}
