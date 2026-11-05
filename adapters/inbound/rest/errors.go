package rest

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	deploymentapp "personal-ai/core/application/deployment"
	inferenceapp "personal-ai/core/application/inference"
	modelapp "personal-ai/core/application/model"
	"personal-ai/core/domain/compute"
	"personal-ai/core/domain/deployment"
	"personal-ai/core/domain/inference"
	"personal-ai/core/domain/model"
	"personal-ai/core/ports"
)

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// retryAfterSeconds is suggested to clients while a deployment starts.
const retryAfterSeconds = 30

// mapping is how an application error is presented over HTTP.
type mapping struct {
	status int
	code   string
	// expose says whether err.Error() is safe and useful to return. Messages
	// of unexpected errors may contain internals and are replaced.
	expose bool
}

var errorTable = []struct {
	err error
	m   mapping
}{
	{ports.ErrNotFound, mapping{http.StatusNotFound, "not_found", true}},
	{model.ErrInvalid, mapping{http.StatusBadRequest, "invalid_model", true}},
	{deployment.ErrInvalid, mapping{http.StatusBadRequest, "invalid_deployment", true}},
	{inference.ErrInvalidRequest, mapping{http.StatusBadRequest, "invalid_request", true}},
	{compute.ErrInvalidSpec, mapping{http.StatusBadRequest, "invalid_compute_spec", true}},
	{modelapp.ErrAlreadyExists, mapping{http.StatusConflict, "already_exists", true}},
	{ports.ErrVersionConflict, mapping{http.StatusConflict, "conflict", true}},
	{deployment.ErrInvalidTransition, mapping{http.StatusConflict, "invalid_transition", true}},
	{deploymentapp.ErrNotDeletable, mapping{http.StatusConflict, "not_deletable", true}},
	{deploymentapp.ErrNotServing, mapping{http.StatusConflict, "not_serving", true}},
	{modelapp.ErrModelInactive, mapping{http.StatusUnprocessableEntity, "model_inactive", true}},
	{deploymentapp.ErrModelInactive, mapping{http.StatusUnprocessableEntity, "model_inactive", true}},
	{inferenceapp.ErrModelUnavailable, mapping{http.StatusUnprocessableEntity, "model_unavailable", true}},
	{inferenceapp.ErrUnsupported, mapping{http.StatusUnprocessableEntity, "unsupported", true}},
	{modelapp.ErrNoDefault, mapping{http.StatusUnprocessableEntity, "no_default_model", true}},
	{model.ErrNoSuitableCompute, mapping{http.StatusUnprocessableEntity, "no_suitable_compute", true}},
	{deploymentapp.ErrLimitReached, mapping{http.StatusTooManyRequests, "deployment_limit_reached", true}},
	{inferenceapp.ErrDeploymentPending, mapping{http.StatusServiceUnavailable, "model_starting", true}},
	{inferenceapp.ErrModelNotDeployed, mapping{http.StatusServiceUnavailable, "model_not_deployed", true}},
	{deploymentapp.ErrDeploymentFailed, mapping{http.StatusServiceUnavailable, "deployment_failed", true}},
	{ports.ErrCapacityUnavailable, mapping{http.StatusServiceUnavailable, "capacity_unavailable", false}},
	{ports.ErrRuntimeUnavailable, mapping{http.StatusBadGateway, "runtime_unavailable", false}},
	{deploymentapp.ErrCleanupFailed, mapping{http.StatusBadGateway, "cleanup_failed", false}},
	{context.DeadlineExceeded, mapping{http.StatusGatewayTimeout, "timeout", false}},
}

func classify(err error) mapping {
	for _, e := range errorTable {
		if errors.Is(err, e.err) {
			return e.m
		}
	}
	return mapping{http.StatusInternalServerError, "internal_error", false}
}

// fail writes the HTTP form of an application error and logs server-side
// failures with their full message.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	m := classify(err)
	msg := err.Error()
	if !m.expose {
		msg = http.StatusText(m.status)
	}
	if m.status >= 500 {
		s.Logger.Warn("request failed", "request_id", w.Header().Get("X-Request-ID"), "path", r.URL.Path,
			"status", m.status, "error_category", m.code, "error", err)
	}
	if m.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	writeError(w, m.status, m.code, msg)
}
