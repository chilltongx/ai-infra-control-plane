package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ID                 string
	Name               string
	Adapter            string
	Labels             map[string]string
	Version            string
	SessionID          string
	ResourceProbe      ResourceProbe
	ArtifactRoot       string
	LeaseTTL           time.Duration
	ClaimWait          time.Duration
	IdlePollInterval   time.Duration
	HeartbeatInterval  time.Duration
	CancelPollInterval time.Duration
	// OneShot registers, performs one bounded claim, executes at most one
	// attempt, and exits. It is intended for ephemeral Kubernetes Jobs.
	OneShot bool
}

type Runner struct {
	client            APIClient
	config            Config
	logger            *slog.Logger
	resourcesMu       sync.Mutex
	lastGoodResources WorkerResources
}

var errCancellationRequested = errors.New("run cancellation requested")

var errLeaseExpired = errors.New("attempt lease expired")

const (
	completionRetryWindow = 10 * time.Second
	completionRetryBase   = 100 * time.Millisecond
	completionRetryMax    = time.Second
)

func NewRunner(client APIClient, config Config, logger *slog.Logger) (*Runner, error) {
	if client == nil {
		return nil, errors.New("worker API client is required")
	}
	if strings.TrimSpace(config.Name) == "" || strings.TrimSpace(config.Adapter) == "" {
		return nil, errors.New("worker name and adapter are required")
	}
	config.SessionID = strings.TrimSpace(config.SessionID)
	if config.SessionID == "" {
		var err error
		config.SessionID, err = newSessionID()
		if err != nil {
			return nil, fmt.Errorf("generate worker session ID: %w", err)
		}
	}
	if config.ResourceProbe == nil {
		config.ResourceProbe = NewNVIDIAResourceProbe()
	}
	if config.ArtifactRoot == "" {
		config.ArtifactRoot = filepath.Join(os.TempDir(), "ai-infra-worker-artifacts")
	}
	if config.LeaseTTL <= 0 {
		config.LeaseTTL = 30 * time.Second
	}
	if config.LeaseTTL < time.Second || config.LeaseTTL%time.Second != 0 {
		return nil, errors.New("lease TTL must be a whole number of seconds")
	}
	if config.ClaimWait <= 0 {
		config.ClaimWait = 20 * time.Second
	}
	if config.IdlePollInterval <= 0 {
		config.IdlePollInterval = time.Second
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = config.LeaseTTL / 3
	}
	if config.HeartbeatInterval <= 0 || config.HeartbeatInterval > config.LeaseTTL/3 {
		return nil, fmt.Errorf("heartbeat interval must be positive and no greater than one third of lease TTL")
	}
	if config.CancelPollInterval <= 0 {
		config.CancelPollInterval = time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{client: client, config: config, logger: logger}, nil
}

func (r *Runner) Run(ctx context.Context) error {
	resources := r.observeResources(ctx)
	worker, err := r.client.Register(ctx, RegisterRequest{
		ID: r.config.ID, Name: r.config.Name, Adapter: r.config.Adapter,
		Labels: r.labelsFor(resources), Version: r.config.Version,
		SessionID: r.config.SessionID, Resources: resources,
	})
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	if worker.ID == "" {
		return errors.New("control plane returned an empty worker ID")
	}
	r.logger.Info("worker registered", "worker_id", worker.ID, "adapter", r.config.Adapter)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go r.workerHeartbeatLoop(heartbeatCtx, worker.ID)
	if r.config.OneShot {
		claim, err := r.client.Claim(ctx, worker.ID, r.config.SessionID, r.config.LeaseTTL, r.config.ClaimWait)
		if err != nil {
			return fmt.Errorf("claim one-shot attempt: %w", err)
		}
		if claim == nil {
			r.logger.Info("one-shot worker found no eligible work", "worker_id", worker.ID)
			return nil
		}
		return r.executeClaim(ctx, *claim)
	}
	for ctx.Err() == nil {
		claim, err := r.client.Claim(ctx, worker.ID, r.config.SessionID, r.config.LeaseTTL, r.config.ClaimWait)
		if err != nil {
			r.logger.Error("claim failed", "error", err)
			if !waitContext(ctx, r.config.IdlePollInterval) {
				break
			}
			continue
		}
		if claim == nil {
			continue
		}
		if err := r.executeClaim(ctx, *claim); err != nil {
			r.logger.Error("attempt processing failed", "run_id", claim.RunID, "attempt_id", claim.AttemptID, "error", err)
		}
	}
	return ctx.Err()
}

func (r *Runner) workerHeartbeatLoop(ctx context.Context, workerID string) {
	interval := r.config.HeartbeatInterval
	if interval > 10*time.Second {
		interval = 10 * time.Second
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	heartbeat := func() {
		resources := r.observeResources(ctx)
		if err := r.client.WorkerHeartbeat(ctx, workerID, WorkerHeartbeatRequest{
			State: "online", Labels: r.labelsFor(resources), Version: r.config.Version,
			SessionID: r.config.SessionID, Resources: resources,
		}); err != nil && ctx.Err() == nil {
			r.logger.Warn("worker heartbeat failed", "worker_id", workerID, "error", err)
		}
	}
	heartbeat()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			heartbeat()
		}
	}
}

func (r *Runner) labelsFor(resources WorkerResources) map[string]string {
	labels := make(map[string]string, len(r.config.Labels)+3)
	for key, value := range r.config.Labels {
		labels[key] = value
	}
	if labels["os"] == "" {
		labels["os"] = runtime.GOOS
	}
	if labels["arch"] == "" {
		labels["arch"] = runtime.GOARCH
	}
	if labels["accelerator"] == "" && len(resources.GPUs) > 0 {
		labels["accelerator"] = "nvidia"
	}
	return labels
}

func (r *Runner) executeClaim(parent context.Context, claim Claim) error {
	if claim.Recipe.Adapter != r.config.Adapter {
		return r.completeWithRetry(parent, claim, Completion{Outcome: "failed", Error: "claimed recipe adapter does not match worker adapter"})
	}
	if err := r.client.Start(parent, claim); err != nil {
		return fmt.Errorf("start attempt: %w", err)
	}
	artifactDir := filepath.Join(r.config.ArtifactRoot, safePathPart(claim.RunID), safePathPart(claim.AttemptID))
	if err := os.MkdirAll(artifactDir, 0o750); err != nil {
		return r.completeFailure(parent, claim, nil, fmt.Errorf("create artifact directory: %w", err))
	}
	prepared, err := PrepareRecipe(claim.Recipe, artifactDir)
	if err != nil {
		return r.completeFailure(parent, claim, nil, fmt.Errorf("prepare recipe: %w", err))
	}
	prepared.Env, err = environmentForAllocation(prepared.Env, claim.Allocation)
	if err != nil {
		return r.completeFailure(parent, claim, nil, fmt.Errorf("apply GPU allocation: %w", err))
	}

	executionCtx, cancel := context.WithCancel(parent)
	defer cancel()
	if prepared.Timeout > 0 {
		var timeoutCancel context.CancelFunc
		executionCtx, timeoutCancel = context.WithTimeout(executionCtx, prepared.Timeout)
		defer timeoutCancel()
	}

	stdoutPath := filepath.Join(artifactDir, "stdout.log")
	stderrPath := filepath.Join(artifactDir, "stderr.log")
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return r.completeFailure(parent, claim, nil, err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		_ = stdout.Close()
		return r.completeFailure(parent, claim, nil, err)
	}

	monitorCtx, stopMonitor := context.WithCancel(executionCtx)
	monitorDone := make(chan error, 1)
	go func() {
		monitorDone <- r.monitor(monitorCtx, cancel, claim)
	}()

	command := exec.CommandContext(executionCtx, prepared.Program, prepared.Args...)
	command.Dir = prepared.WorkingDir
	command.Env = prepared.Env
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	_ = stdout.Close()
	_ = stderr.Close()
	// Stop and join the monitor before assembling or submitting completion. In
	// particular, this prevents an in-flight heartbeat racing a completion.
	stopMonitor()
	leaseErr := <-monitorDone

	exitCode := 0
	if runErr != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	artifactPaths := []string{stdoutPath, stderrPath, prepared.ResultFile}
	expectedPaths, expectedErr := resolveExpectedOutputs(prepared.WorkingDir, claim.Recipe.ExpectedOutputs)
	artifactPaths = append(artifactPaths, expectedPaths...)
	artifacts := collectArtifacts(artifactPaths...)
	if expectedErr != nil && runErr == nil {
		runErr = expectedErr
	}
	metrics := map[string]float64{}
	if prepared.ResultFile != "" {
		if data, readErr := os.ReadFile(prepared.ResultFile); readErr == nil {
			if parsed, parseErr := ParseSGLangMetrics(data); parseErr == nil {
				metrics = parsed
			} else if runErr == nil {
				runErr = fmt.Errorf("parse benchmark metrics: %w", parseErr)
			}
		} else if runErr == nil {
			runErr = fmt.Errorf("read benchmark result: %w", readErr)
		}
	}
	if leaseErr != nil && !errors.Is(leaseErr, errCancellationRequested) {
		// Never submit a stale completion after a lost lease/fence.
		return leaseErr
	}
	result := Completion{Outcome: "succeeded", ExitCode: &exitCode, Metrics: metrics, Artifacts: artifacts}
	if errors.Is(leaseErr, errCancellationRequested) {
		result.Outcome = "cancelled"
		result.Error = "cancelled by operator"
	} else if runErr != nil {
		result.Outcome = "failed"
		result.Error = runErr.Error()
	}
	return r.completeWithRetry(parent, claim, result)
}

func (r *Runner) monitor(ctx context.Context, cancel context.CancelFunc, claim Claim) error {
	heartbeats := time.NewTicker(r.config.HeartbeatInterval)
	cancels := time.NewTicker(r.config.CancelPollInterval)
	leaseExpiresAt := claim.ExpiresAt
	if leaseExpiresAt.IsZero() {
		leaseExpiresAt = time.Now().Add(r.config.LeaseTTL)
	}
	leaseTimer := time.NewTimer(time.Until(leaseExpiresAt))
	defer heartbeats.Stop()
	defer cancels.Stop()
	defer leaseTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-leaseTimer.C:
			cancel()
			return errLeaseExpired
		case <-heartbeats.C:
			heartbeatCtx, stopHeartbeat := context.WithDeadline(ctx, leaseExpiresAt)
			renewedUntil, err := r.client.AttemptHeartbeat(heartbeatCtx, claim, r.config.LeaseTTL)
			stopHeartbeat()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if isLeaseConflict(err) {
					cancel()
					return fmt.Errorf("attempt lease lost: %w", err)
				}
				if !time.Now().Before(leaseExpiresAt) {
					cancel()
					return fmt.Errorf("%w after heartbeat failure: %v", errLeaseExpired, err)
				}
				if !isTransientAPIError(err) {
					cancel()
					return fmt.Errorf("attempt heartbeat rejected: %w", err)
				}
				r.logger.Warn("attempt heartbeat temporarily failed; retaining current lease", "run_id", claim.RunID, "attempt_id", claim.AttemptID, "lease_expires_at", leaseExpiresAt, "error", err)
				continue
			}
			if renewedUntil.IsZero() {
				renewedUntil = time.Now().Add(r.config.LeaseTTL)
			}
			if !renewedUntil.After(time.Now()) {
				cancel()
				return fmt.Errorf("%w: heartbeat returned %s", errLeaseExpired, renewedUntil)
			}
			leaseExpiresAt = renewedUntil
			resetTimer(leaseTimer, time.Until(leaseExpiresAt))
		case <-cancels.C:
			state, err := r.client.RunState(ctx, claim.RunID)
			if err == nil && (state == "cancel_requested" || state == "cancelled") {
				cancel()
				return errCancellationRequested
			}
		}
	}
}

func (r *Runner) completeFailure(ctx context.Context, claim Claim, exitCode *int, err error) error {
	completionErr := r.completeWithRetry(ctx, claim, Completion{Outcome: "failed", ExitCode: exitCode, Error: err.Error()})
	if completionErr != nil {
		return errors.Join(err, completionErr)
	}
	return nil
}

func (r *Runner) completeWithRetry(ctx context.Context, claim Claim, completion Completion) error {
	retryCtx, cancel := context.WithTimeout(ctx, completionRetryWindow)
	defer cancel()
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := r.client.Complete(retryCtx, claim, completion); err == nil {
			return nil
		} else {
			lastErr = err
			if isLeaseConflict(err) || !isTransientAPIError(err) {
				return fmt.Errorf("complete attempt: %w", err)
			}
		}
		delay := completionRetryBase << min(attempt, 4)
		if delay > completionRetryMax {
			delay = completionRetryMax
		}
		if !waitContext(retryCtx, delay) {
			return fmt.Errorf("complete attempt retry deadline reached: %w", lastErr)
		}
	}
}

func isLeaseConflict(err error) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == 409
}

func isTransientAPIError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		// Transport failures and truncated/invalid responses are safe to retry;
		// mutation requests are fenced and completion is idempotent server-side.
		return true
	}
	return statusErr.StatusCode == http.StatusRequestTimeout ||
		statusErr.StatusCode == http.StatusTooEarly ||
		statusErr.StatusCode == http.StatusTooManyRequests ||
		statusErr.StatusCode >= http.StatusInternalServerError
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if delay < 0 {
		delay = 0
	}
	timer.Reset(delay)
}

func collectArtifacts(paths ...string) []Artifact {
	result := make([]Artifact, 0, len(paths))
	for _, filePath := range paths {
		if filePath == "" {
			continue
		}
		file, err := os.Open(filePath)
		if err != nil {
			continue
		}
		hash := sha256.New()
		bytes, copyErr := io.Copy(hash, file)
		_ = file.Close()
		if copyErr != nil {
			continue
		}
		result = append(result, Artifact{Name: filepath.Base(filePath), URI: "file://" + filepath.ToSlash(filePath), SHA256: hex.EncodeToString(hash.Sum(nil)), SizeBytes: bytes})
	}
	return result
}

func resolveExpectedOutputs(workingDir string, expected []string) ([]string, error) {
	if len(expected) == 0 {
		return nil, nil
	}
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current working directory: %w", err)
		}
	}
	root, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory links: %w", err)
	}
	paths := make([]string, 0, len(expected))
	for _, relative := range expected {
		if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) == "." {
			return nil, fmt.Errorf("expected output must be a non-empty relative file: %q", relative)
		}
		candidate := filepath.Join(root, filepath.Clean(relative))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return nil, fmt.Errorf("resolve expected output %q: %w", relative, err)
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("expected output escapes working directory: %q", relative)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("expected output is not a regular file: %q", relative)
		}
		paths = append(paths, resolved)
	}
	return paths, nil
}

func safePathPart(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return "unknown"
	}
	var output strings.Builder
	for _, character := range input {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			output.WriteRune(character)
		} else {
			output.WriteByte('_')
		}
	}
	return output.String()
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	// UUID version 4 and RFC 4122 variant bits make logs easier to recognize
	// while keeping the identifier dependency-free.
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func (r *Runner) observeResources(ctx context.Context) WorkerResources {
	observed := r.config.ResourceProbe.Probe(ctx)
	if observed.ObservedAt.IsZero() {
		observed.ObservedAt = time.Now().UTC()
	} else {
		observed.ObservedAt = observed.ObservedAt.UTC()
	}
	r.resourcesMu.Lock()
	defer r.resourcesMu.Unlock()
	switch observed.ProbeStatus {
	case ResourceProbeOK:
		observed.GPUs = cloneGPUs(observed.GPUs)
		r.lastGoodResources = observed
	case ResourceProbeUnavailable:
		observed.GPUs = []GPUResource{}
	case ResourceProbeError:
		observed.GPUs = cloneGPUs(r.lastGoodResources.GPUs)
	default:
		observed.ProbeStatus = ResourceProbeError
		observed.GPUs = cloneGPUs(r.lastGoodResources.GPUs)
	}
	if observed.GPUs == nil {
		observed.GPUs = []GPUResource{}
	}
	return observed
}

func cloneGPUs(input []GPUResource) []GPUResource {
	if len(input) == 0 {
		return []GPUResource{}
	}
	return append([]GPUResource(nil), input...)
}

func environmentForAllocation(environment []string, allocation ResourceAllocation) ([]string, error) {
	if len(allocation.GPUs) == 0 {
		return append([]string(nil), environment...), nil
	}
	output := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "CUDA_VISIBLE_DEVICES") {
			continue
		}
		output = append(output, entry)
	}
	indexes := make([]string, len(allocation.GPUs))
	seen := make(map[int]struct{}, len(allocation.GPUs))
	for index, gpu := range allocation.GPUs {
		if gpu.Index < 0 {
			return nil, fmt.Errorf("GPU %q has negative index %d", gpu.ID, gpu.Index)
		}
		if _, duplicate := seen[gpu.Index]; duplicate {
			return nil, fmt.Errorf("GPU index %d is allocated more than once", gpu.Index)
		}
		seen[gpu.Index] = struct{}{}
		indexes[index] = fmt.Sprintf("%d", gpu.Index)
	}
	return append(output, "CUDA_VISIBLE_DEVICES="+strings.Join(indexes, ",")), nil
}
