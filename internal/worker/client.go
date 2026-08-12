package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// APIClient is the deliberately small control-plane boundary used by a worker.
// Keeping these DTOs private lets the worker evolve independently of the HTTP
// transport package.
type APIClient interface {
	Register(context.Context, RegisterRequest) (WorkerRecord, error)
	WorkerHeartbeat(context.Context, string, WorkerHeartbeatRequest) error
	Claim(context.Context, string, string, time.Duration, time.Duration) (*Claim, error)
	Start(context.Context, Claim) error
	AttemptHeartbeat(context.Context, Claim, time.Duration) (time.Time, error)
	Complete(context.Context, Claim, Completion) error
	RunState(context.Context, string) (string, error)
}

type RegisterRequest struct {
	ID        string            `json:"id,omitempty"`
	Name      string            `json:"name"`
	Adapter   string            `json:"adapter"`
	Labels    map[string]string `json:"labels,omitempty"`
	Version   string            `json:"version,omitempty"`
	SessionID string            `json:"session_id"`
	Resources WorkerResources   `json:"resources"`
}

type WorkerRecord struct {
	ID string `json:"id"`
}

type WorkerHeartbeatRequest struct {
	State     string            `json:"state,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Version   string            `json:"version,omitempty"`
	SessionID string            `json:"session_id"`
	Resources WorkerResources   `json:"resources"`
}

type WorkerResources struct {
	GPUs        []GPUResource `json:"gpus"`
	ObservedAt  time.Time     `json:"observed_at"`
	ProbeStatus string        `json:"probe_status"`
}

type GPUResource struct {
	ID                 string  `json:"id"`
	Index              int     `json:"index"`
	Name               string  `json:"name"`
	Vendor             string  `json:"vendor"`
	TotalMemoryBytes   int64   `json:"total_memory_bytes"`
	FreeMemoryBytes    int64   `json:"free_memory_bytes"`
	UtilizationPercent float64 `json:"utilization_percent"`
	TemperatureCelsius float64 `json:"temperature_celsius"`
}

type ResourceAllocation struct {
	WorkerID            string        `json:"worker_id"`
	GPUIDs              []string      `json:"gpu_ids,omitempty"`
	GPUs                []GPUResource `json:"gpus,omitempty"`
	ResourcesObservedAt time.Time     `json:"resources_observed_at,omitempty"`
}

type Recipe struct {
	Adapter         string            `json:"adapter"`
	Command         []string          `json:"command"`
	WorkingDir      string            `json:"working_dir,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	ExpectedOutputs []string          `json:"expected_outputs,omitempty"`
	TimeoutSeconds  int               `json:"timeout_seconds,omitempty"`
}

type Claim struct {
	AttemptID  string             `json:"attempt_id"`
	RunID      string             `json:"run_id"`
	Fence      int64              `json:"fence"`
	LeaseToken string             `json:"lease_token"`
	ExpiresAt  time.Time          `json:"expires_at"`
	Recipe     Recipe             `json:"recipe"`
	Allocation ResourceAllocation `json:"allocation"`
}

// UnmarshalJSON accepts both the current flat claim response and the planned
// internal response shape ({attempt, run}). This keeps the polling client
// isolated from a transport migration.
func (c *Claim) UnmarshalJSON(data []byte) error {
	type flat Claim
	var wire struct {
		flat
		Attempt *struct {
			ID             string              `json:"id"`
			RunID          string              `json:"run_id"`
			Fence          int64               `json:"fence"`
			LeaseToken     string              `json:"lease_token"`
			ExpiresAt      time.Time           `json:"expires_at"`
			LeaseExpiresAt time.Time           `json:"lease_expires_at"`
			Allocation     *ResourceAllocation `json:"allocation"`
		} `json:"attempt"`
		Run *struct {
			ID     string `json:"id"`
			Recipe Recipe `json:"recipe"`
		} `json:"run"`
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*c = Claim(wire.flat)
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = wire.LeaseExpiresAt
	}
	if wire.Attempt != nil {
		c.AttemptID = wire.Attempt.ID
		c.RunID = wire.Attempt.RunID
		c.Fence = wire.Attempt.Fence
		if wire.Attempt.LeaseToken != "" {
			c.LeaseToken = wire.Attempt.LeaseToken
		}
		if !wire.Attempt.ExpiresAt.IsZero() {
			c.ExpiresAt = wire.Attempt.ExpiresAt
		} else if !wire.Attempt.LeaseExpiresAt.IsZero() {
			c.ExpiresAt = wire.Attempt.LeaseExpiresAt
		}
		if wire.Attempt.Allocation != nil {
			c.Allocation = *wire.Attempt.Allocation
		}
	}
	if wire.Run != nil {
		if c.RunID == "" {
			c.RunID = wire.Run.ID
		}
		c.Recipe = wire.Run.Recipe
	}
	return nil
}

type Artifact struct {
	Name      string `json:"name"`
	URI       string `json:"uri"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type Completion struct {
	Outcome   string             `json:"outcome"`
	ExitCode  *int               `json:"exit_code,omitempty"`
	Error     string             `json:"error,omitempty"`
	Metrics   map[string]float64 `json:"metrics,omitempty"`
	Artifacts []Artifact         `json:"artifacts,omitempty"`
}

type HTTPClient struct {
	base     *url.URL
	http     *http.Client
	apiToken string
}

func NewHTTPClient(rawBaseURL string, client *http.Client) (*HTTPClient, error) {
	return NewHTTPClientWithToken(rawBaseURL, client, strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_TOKEN")))
}

// NewHTTPClientWithToken configures an explicit bearer token. The token is
// held in memory and sent only in the Authorization header.
func NewHTTPClientWithToken(rawBaseURL string, client *http.Client, apiToken string) (*HTTPClient, error) {
	base, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid control-plane URL %q", rawBaseURL)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("unsupported control-plane URL scheme %q", base.Scheme)
	}
	if client == nil {
		client = &http.Client{Timeout: 35 * time.Second}
	}
	return &HTTPClient{base: base, http: client, apiToken: strings.TrimSpace(apiToken)}, nil
}

func (c *HTTPClient) Register(ctx context.Context, input RegisterRequest) (WorkerRecord, error) {
	var output WorkerRecord
	err := c.do(ctx, http.MethodPost, "/v1/workers", nil, input, &output, http.StatusCreated, http.StatusOK)
	return output, err
}

func (c *HTTPClient) WorkerHeartbeat(ctx context.Context, workerID string, input WorkerHeartbeatRequest) error {
	return c.do(ctx, http.MethodPost, route("/v1/workers", workerID, "heartbeat"), nil, input, nil, http.StatusOK)
}

func (c *HTTPClient) Claim(ctx context.Context, workerID, sessionID string, leaseTTL, wait time.Duration) (*Claim, error) {
	query := url.Values{}
	query.Set("lease_ttl", leaseTTL.String())
	query.Set("wait", wait.String())
	body := struct {
		SessionID string `json:"session_id"`
	}{SessionID: sessionID}
	var output Claim
	err := c.do(ctx, http.MethodPost, route("/v1/workers", workerID, "claim"), query, body, &output, http.StatusOK, http.StatusNoContent)
	if errors.Is(err, errNoContent) {
		return nil, nil
	}
	return &output, err
}

func (c *HTTPClient) Start(ctx context.Context, claim Claim) error {
	return c.attemptMutation(ctx, claim, "start", nil)
}

func (c *HTTPClient) AttemptHeartbeat(ctx context.Context, claim Claim, extendBy time.Duration) (time.Time, error) {
	if extendBy < time.Second || extendBy%time.Second != 0 {
		return time.Time{}, fmt.Errorf("heartbeat extension must be a whole number of seconds")
	}
	body := struct {
		LeaseToken    string `json:"lease_token"`
		Fence         int64  `json:"fence"`
		ExtendSeconds int64  `json:"extend_seconds"`
	}{claim.LeaseToken, claim.Fence, int64(extendBy / time.Second)}
	var output struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	err := c.do(ctx, http.MethodPost, route("/v1/runs", claim.RunID, "attempts", claim.AttemptID, "heartbeat"), nil, body, &output, http.StatusOK)
	return output.ExpiresAt, err
}

func (c *HTTPClient) Complete(ctx context.Context, claim Claim, result Completion) error {
	body := struct {
		LeaseToken string             `json:"lease_token"`
		Fence      int64              `json:"fence"`
		Outcome    string             `json:"outcome"`
		ExitCode   *int               `json:"exit_code,omitempty"`
		Error      string             `json:"error,omitempty"`
		Metrics    map[string]float64 `json:"metrics,omitempty"`
		Artifacts  []Artifact         `json:"artifacts,omitempty"`
	}{claim.LeaseToken, claim.Fence, result.Outcome, result.ExitCode, result.Error, result.Metrics, result.Artifacts}
	return c.attemptMutation(ctx, claim, "complete", body)
}

func (c *HTTPClient) RunState(ctx context.Context, runID string) (string, error) {
	var output struct {
		State             string     `json:"state"`
		CancelRequestedAt *time.Time `json:"cancel_requested_at"`
	}
	if err := c.do(ctx, http.MethodGet, route("/v1/runs", runID), nil, nil, &output, http.StatusOK); err != nil {
		return "", err
	}
	if output.CancelRequestedAt != nil && output.State != "succeeded" && output.State != "failed" && output.State != "cancelled" {
		return "cancel_requested", nil
	}
	return output.State, nil
}

func (c *HTTPClient) attemptMutation(ctx context.Context, claim Claim, action string, payload any) error {
	if payload == nil {
		payload = struct {
			LeaseToken string `json:"lease_token"`
			Fence      int64  `json:"fence"`
		}{claim.LeaseToken, claim.Fence}
	}
	return c.do(ctx, http.MethodPost, route("/v1/runs", claim.RunID, "attempts", claim.AttemptID, action), nil, payload, nil, http.StatusOK)
}

var errNoContent = errors.New("no content")

// HTTPStatusError preserves a non-success response's status code so callers
// can distinguish a lost lease/fence (409) from retryable transport and 5xx
// failures without parsing an error string.
type HTTPStatusError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func (c *HTTPClient) do(ctx context.Context, method, routePath string, query url.Values, body, output any, statuses ...int) error {
	u := *c.base
	u.Path = path.Join(strings.TrimSuffix(c.base.Path, "/"), routePath)
	u.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, routePath, err)
	}
	defer func() { _ = response.Body.Close() }()
	for _, allowed := range statuses {
		if response.StatusCode == allowed {
			if allowed == http.StatusNoContent {
				return errNoContent
			}
			if output == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				return nil
			}
			if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output); err != nil {
				return fmt.Errorf("decode %s %s response: %w", method, routePath, err)
			}
			return nil
		}
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	return &HTTPStatusError{
		Method: method, Path: routePath, StatusCode: response.StatusCode,
		Body: strings.TrimSpace(string(data)),
	}
}

func route(base string, parts ...string) string {
	values := []string{base}
	for _, part := range parts {
		values = append(values, url.PathEscape(part))
	}
	return path.Join(values...)
}
