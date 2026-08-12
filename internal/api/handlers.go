package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.version})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Ready(r.Context()); err != nil {
		writeServiceError(w, r, fmt.Errorf("%w: %v", ErrUnavailable, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) createExperiment(w http.ResponseWriter, r *http.Request) {
	var input CreateExperimentInput
	if !s.decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "name is required")
		return
	}
	result, err := s.service.CreateExperiment(r.Context(), input)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) listExperiments(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	items, err := s.service.ListExperiments(r.Context(), limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items), "count": len(items)})
}

func (s *Server) getExperiment(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.GetExperiment(r.Context(), r.PathValue("experiment_id"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var input CreateRunInput
	if !s.decode(w, r, &input) {
		return
	}
	input.ExperimentID = r.PathValue("experiment_id")
	if strings.TrimSpace(input.Recipe.Adapter) == "" || len(input.Recipe.Command) == 0 {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "recipe.adapter and recipe.command are required")
		return
	}
	if input.MaxAttempts < 0 {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "max_attempts must be positive")
		return
	}
	if input.ResourceRequirements.GPUCount < 0 || input.ResourceRequirements.MinFreeGPUMemoryBytes < 0 ||
		input.ResourceRequirements.GPUCount > 16 ||
		(input.ResourceRequirements.MinFreeGPUMemoryBytes > 0 && input.ResourceRequirements.GPUCount == 0) {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "resource_requirements must use a positive gpu_count and non-negative memory")
		return
	}
	result, err := s.service.CreateRun(r.Context(), input)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	items, err := s.service.ListRuns(r.Context(), r.PathValue("experiment_id"), limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items), "count": len(items)})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.GetRun(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listAttempts(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	items, err := s.service.ListAttempts(r.Context(), r.PathValue("run_id"), limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items), "count": len(items)})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason,omitempty"`
	}
	if !s.decodeOptional(w, r, &body) {
		return
	}
	result, err := s.service.CancelRun(r.Context(), r.PathValue("run_id"), body.Reason)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "after must be a non-negative integer")
			return
		}
		after = value
	}
	items, err := s.service.ListEvents(r.Context(), r.PathValue("run_id"), after, limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items), "count": len(items)})
}

func (s *Server) registerWorker(w http.ResponseWriter, r *http.Request) {
	var input RegisterWorkerInput
	if !s.decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Adapter) == "" {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "name and adapter are required")
		return
	}
	if strings.TrimSpace(input.SessionID) == "" {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "session_id is required")
		return
	}
	result, err := s.service.RegisterWorker(r.Context(), input)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	items, err := s.service.ListWorkers(r.Context(), limit)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items), "count": len(items)})
}

func (s *Server) heartbeatWorker(w http.ResponseWriter, r *http.Request) {
	var input WorkerHeartbeatInput
	if !s.decodeOptional(w, r, &input) {
		return
	}
	result, err := s.service.HeartbeatWorker(r.Context(), r.PathValue("worker_id"), input)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if !s.decodeOptional(w, r, &body) {
		return
	}
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		// Query support is retained for V0 clients, but official workers keep
		// the fencing credential out of URLs and send it in the JSON body.
		sessionID = strings.TrimSpace(r.URL.Query().Get("session_id"))
	}
	if sessionID == "" {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "session_id is required")
		return
	}
	leaseTTL, ok := parseDurationQuery(w, r, "lease_ttl", defaultLeaseTTL, time.Second, maxLeaseTTL)
	if !ok {
		return
	}
	wait, ok := parseDurationQuery(w, r, "wait", 0, 0, maxClaimWait)
	if !ok {
		return
	}
	result, found, err := s.service.Claim(r.Context(), ClaimInput{WorkerID: r.PathValue("worker_id"), SessionID: sessionID, LeaseTTL: leaseTTL, WaitTimeout: wait})
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) startAttempt(w http.ResponseWriter, r *http.Request) {
	var input AttemptMutationInput
	if !s.decode(w, r, &input) {
		return
	}
	input.RunID = r.PathValue("run_id")
	input.AttemptID = r.PathValue("attempt_id")
	if !validLeaseMutation(input.LeaseToken, input.Fence) {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "lease_token and a positive fence are required")
		return
	}
	result, err := s.service.StartAttempt(r.Context(), input)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) heartbeatAttempt(w http.ResponseWriter, r *http.Request) {
	var input AttemptMutationInput
	if !s.decode(w, r, &input) {
		return
	}
	input.RunID = r.PathValue("run_id")
	input.AttemptID = r.PathValue("attempt_id")
	if !validLeaseMutation(input.LeaseToken, input.Fence) {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "lease_token and a positive fence are required")
		return
	}
	if input.ExtendSeconds <= 0 || input.ExtendSeconds > int(maxLeaseTTL/time.Second) {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", fmt.Sprintf("extend_seconds must be between 1 and %d", int(maxLeaseTTL/time.Second)))
		return
	}
	result, err := s.service.HeartbeatAttempt(r.Context(), input)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) completeAttempt(w http.ResponseWriter, r *http.Request) {
	var input CompleteAttemptInput
	if !s.decode(w, r, &input) {
		return
	}
	input.RunID = r.PathValue("run_id")
	input.AttemptID = r.PathValue("attempt_id")
	if !validLeaseMutation(input.LeaseToken, input.Fence) {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "lease_token and a positive fence are required")
		return
	}
	if input.Outcome != "succeeded" && input.Outcome != "failed" && input.Outcome != "cancelled" {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "outcome must be succeeded, failed, or cancelled")
		return
	}
	result, err := s.service.CompleteAttempt(r.Context(), input)
	if err != nil {
		writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func validLeaseMutation(token string, fence int64) bool {
	return strings.TrimSpace(token) != "" && fence > 0
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, destination any) bool {
	return s.decodeJSON(w, r, destination, false)
}

func (s *Server) decodeOptional(w http.ResponseWriter, r *http.Request, destination any) bool {
	return s.decodeJSON(w, r, destination, true)
}

func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, destination any, optional bool) bool {
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mediaType != "" && mediaType != "application/json" {
		writeError(w, RequestID(r.Context()), http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	reader := http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return true
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, RequestID(r.Context()), http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_json", "request body must be one JSON object with known fields")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_json", "request body must contain a single JSON object")
		return false
	}
	return true
}

func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	const defaultLimit = 100
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 1000 {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "limit must be between 1 and 1000")
		return 0, false
	}
	return value, true
}

func parseDurationQuery(w http.ResponseWriter, r *http.Request, name string, fallback, minimum, maximum time.Duration) (time.Duration, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum || value > maximum {
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", fmt.Sprintf("%s must be a duration between %s and %s", name, minimum, maximum))
		return 0, false
	}
	return value, true
}

func writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, RequestID(r.Context()), http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, ErrConflict):
		writeError(w, RequestID(r.Context()), http.StatusConflict, "conflict", "request conflicts with current state")
	case errors.Is(err, ErrInvalid):
		writeError(w, RequestID(r.Context()), http.StatusBadRequest, "invalid_argument", "invalid request")
	case errors.Is(err, ErrLeaseLost):
		writeError(w, RequestID(r.Context()), http.StatusConflict, "lease_lost", "lease is expired, fenced, or owned by another worker")
	case errors.Is(err, ErrUnavailable):
		writeError(w, RequestID(r.Context()), http.StatusServiceUnavailable, "unavailable", "service is unavailable")
	default:
		writeError(w, RequestID(r.Context()), http.StatusInternalServerError, "internal", "internal server error")
	}
}

func writeError(w http.ResponseWriter, requestID string, status int, code, message string) {
	var envelope errorEnvelope
	envelope.Error.Code = code
	envelope.Error.Message = message
	envelope.Error.RequestID = requestID
	writeJSON(w, status, envelope)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
