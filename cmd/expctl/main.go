// expctl is the command-line client for ForgeGrid.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultBaseURL = "http://127.0.0.1:8080"

type client struct {
	baseURL  string
	http     *http.Client
	compact  bool
	apiToken string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "expctl:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("expctl", flag.ContinueOnError)
	global.SetOutput(stderr)
	baseURL := global.String("server", envOr("EXPERIMENT_CONTROL_PLANE_URL", defaultBaseURL), "control-plane base URL")
	timeout := global.Duration("timeout", 30*time.Second, "request timeout")
	compact := global.Bool("compact", false, "print compact JSON")
	tokenFile := global.String("token-file", "", "read bearer token from file (defaults to CONTROL_PLANE_API_TOKEN)")
	global.Usage = func() { printUsage(stderr) }
	if err := global.Parse(args); err != nil {
		return err
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		global.Usage()
		return errors.New("command is required")
	}
	if remaining[0] == "help" || remaining[0] == "--help" || remaining[0] == "-h" {
		global.Usage()
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	apiToken := strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_TOKEN"))
	if strings.TrimSpace(*tokenFile) != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read token file: %w", err)
		}
		apiToken = strings.TrimSpace(string(data))
		if apiToken == "" {
			return errors.New("token file is empty")
		}
	}
	c := client{
		baseURL:  strings.TrimRight(*baseURL, "/"),
		http:     &http.Client{},
		compact:  *compact,
		apiToken: apiToken,
	}
	return dispatch(ctx, c, remaining, stdout, stderr)
}

func dispatch(ctx context.Context, c client, args []string, stdout, stderr io.Writer) error {
	switch args[0] {
	case "health":
		return c.call(ctx, http.MethodGet, "/healthz", nil, "", stdout)
	case "experiment", "experiments":
		return experimentCommand(ctx, c, args[1:], stdout, stderr)
	case "run", "runs":
		return runCommand(ctx, c, args[1:], stdout, stderr)
	case "worker", "workers":
		return workerCommand(ctx, c, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func experimentCommand(ctx context.Context, c client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("experiment subcommand is required: create, list, get")
	}
	switch args[0] {
	case "list":
		set := flag.NewFlagSet("experiment list", flag.ContinueOnError)
		set.SetOutput(stderr)
		limit := set.Int("limit", 100, "maximum results")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		return c.call(ctx, http.MethodGet, "/v1/experiments?limit="+strconv.Itoa(*limit), nil, "", stdout)
	case "get":
		if len(args) != 2 {
			return errors.New("usage: expctl experiment get EXPERIMENT_ID")
		}
		return c.call(ctx, http.MethodGet, "/v1/experiments/"+pathSegment(args[1]), nil, "", stdout)
	case "create":
		set := flag.NewFlagSet("experiment create", flag.ContinueOnError)
		set.SetOutput(stderr)
		name := set.String("name", "", "experiment name (required)")
		description := set.String("description", "", "description")
		labels := stringMapFlag{}
		set.Var(&labels, "label", "label key=value (repeatable)")
		idempotencyKey := set.String("idempotency-key", "", "safe retry key")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return errors.New("--name is required")
		}
		body := map[string]any{"name": *name, "description": *description, "labels": map[string]string(labels)}
		return c.call(ctx, http.MethodPost, "/v1/experiments", body, *idempotencyKey, stdout)
	default:
		return fmt.Errorf("unknown experiment subcommand %q", args[0])
	}
}

func runCommand(ctx context.Context, c client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("run subcommand is required: create, list, get, cancel, events")
	}
	switch args[0] {
	case "create":
		set := flag.NewFlagSet("run create", flag.ContinueOnError)
		set.SetOutput(stderr)
		experimentID := set.String("experiment", "", "experiment ID (required)")
		adapter := set.String("adapter", "local", "worker adapter")
		workingDir := set.String("working-dir", "", "worker working directory")
		timeoutSeconds := set.Int("run-timeout", 0, "run timeout in seconds")
		priority := set.Int("priority", 0, "higher values run first")
		maxAttempts := set.Int("max-attempts", 1, "maximum attempts")
		gpuCount := set.Int("gpus", 0, "number of GPUs required")
		minFreeGPUBytes := set.Int64("min-free-gpu-memory", 0, "minimum free bytes required on each GPU")
		idempotencyKey := set.String("idempotency-key", "", "safe retry key")
		requiredLabels := stringMapFlag{}
		environment := stringMapFlag{}
		expectedOutputs := stringSliceFlag{}
		set.Var(&requiredLabels, "require-label", "required worker label key=value (repeatable)")
		set.Var(&environment, "env", "environment key=value (repeatable)")
		set.Var(&expectedOutputs, "expected-output", "relative artifact path (repeatable)")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if *experimentID == "" {
			return errors.New("--experiment is required")
		}
		if *gpuCount < 0 || *gpuCount > 16 || *minFreeGPUBytes < 0 || (*minFreeGPUBytes > 0 && *gpuCount == 0) {
			return errors.New("--gpus must be 0-16 and is required when --min-free-gpu-memory is set")
		}
		command := set.Args()
		if len(command) == 0 {
			return errors.New("command is required after flags (use -- before command flags)")
		}
		body := map[string]any{
			"recipe": map[string]any{
				"adapter":          *adapter,
				"command":          command,
				"working_dir":      *workingDir,
				"environment":      map[string]string(environment),
				"expected_outputs": []string(expectedOutputs),
				"timeout_seconds":  *timeoutSeconds,
			},
			"required_labels":       map[string]string(requiredLabels),
			"resource_requirements": map[string]any{"gpu_count": *gpuCount, "min_free_gpu_memory_bytes": *minFreeGPUBytes},
			"priority":              *priority,
			"max_attempts":          *maxAttempts,
		}
		path := "/v1/experiments/" + pathSegment(*experimentID) + "/runs"
		return c.call(ctx, http.MethodPost, path, body, *idempotencyKey, stdout)
	case "list":
		set := flag.NewFlagSet("run list", flag.ContinueOnError)
		set.SetOutput(stderr)
		experimentID := set.String("experiment", "", "experiment ID (required)")
		limit := set.Int("limit", 100, "maximum results")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if *experimentID == "" {
			return errors.New("--experiment is required")
		}
		path := "/v1/experiments/" + pathSegment(*experimentID) + "/runs?limit=" + strconv.Itoa(*limit)
		return c.call(ctx, http.MethodGet, path, nil, "", stdout)
	case "get":
		if len(args) != 2 {
			return errors.New("usage: expctl run get RUN_ID")
		}
		return c.call(ctx, http.MethodGet, "/v1/runs/"+pathSegment(args[1]), nil, "", stdout)
	case "cancel":
		set := flag.NewFlagSet("run cancel", flag.ContinueOnError)
		set.SetOutput(stderr)
		reason := set.String("reason", "", "cancellation reason")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 1 {
			return errors.New("usage: expctl run cancel [--reason TEXT] RUN_ID")
		}
		return c.call(ctx, http.MethodPost, "/v1/runs/"+pathSegment(set.Arg(0))+"/cancel", map[string]string{"reason": *reason}, "", stdout)
	case "events":
		set := flag.NewFlagSet("run events", flag.ContinueOnError)
		set.SetOutput(stderr)
		after := set.Int64("after", 0, "only events after sequence")
		limit := set.Int("limit", 100, "maximum results")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 1 {
			return errors.New("usage: expctl run events [--after N] RUN_ID")
		}
		path := fmt.Sprintf("/v1/runs/%s/events?after=%d&limit=%d", pathSegment(set.Arg(0)), *after, *limit)
		return c.call(ctx, http.MethodGet, path, nil, "", stdout)
	default:
		return fmt.Errorf("unknown run subcommand %q", args[0])
	}
}

func workerCommand(ctx context.Context, c client, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("worker subcommand is required: register, list")
	}
	switch args[0] {
	case "list":
		set := flag.NewFlagSet("worker list", flag.ContinueOnError)
		set.SetOutput(stderr)
		limit := set.Int("limit", 100, "maximum results")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		return c.call(ctx, http.MethodGet, "/v1/workers?limit="+strconv.Itoa(*limit), nil, "", stdout)
	case "register":
		set := flag.NewFlagSet("worker register", flag.ContinueOnError)
		set.SetOutput(stderr)
		id := set.String("id", "", "stable worker ID")
		name := set.String("name", "", "worker name (required)")
		adapter := set.String("adapter", "local", "adapter")
		version := set.String("version", "", "worker version")
		sessionID := set.String("session-id", "", "worker process session ID (required)")
		labels := stringMapFlag{}
		set.Var(&labels, "label", "worker label key=value (repeatable)")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" || strings.TrimSpace(*sessionID) == "" {
			return errors.New("--name and --session-id are required")
		}
		body := map[string]any{"id": *id, "name": *name, "adapter": *adapter, "version": *version, "session_id": *sessionID, "labels": map[string]string(labels)}
		return c.call(ctx, http.MethodPost, "/v1/workers", body, "", stdout)
	default:
		return fmt.Errorf("unknown worker subcommand %q", args[0])
	}
}

func (c client) call(ctx context.Context, method, path string, body any, idempotencyKey string, stdout io.Writer) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if c.apiToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusNoContent {
		_, err := fmt.Fprintln(stdout, "null")
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &envelope) == nil && envelope.Error.Message != "" {
			return fmt.Errorf("HTTP %d %s: %s (request_id=%s)", response.StatusCode, envelope.Error.Code, envelope.Error.Message, envelope.Error.RequestID)
		}
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return fmt.Errorf("server returned invalid JSON: %w", err)
	}
	var formatted []byte
	if c.compact {
		formatted, err = json.Marshal(value)
	} else {
		formatted, err = json.MarshalIndent(value, "", "  ")
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(formatted))
	return err
}

type stringMapFlag map[string]string

func (value *stringMapFlag) String() string { return fmt.Sprint(map[string]string(*value)) }
func (value *stringMapFlag) Set(raw string) error {
	key, item, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return errors.New("expected key=value")
	}
	if *value == nil {
		*value = make(map[string]string)
	}
	(*value)[key] = item
	return nil
}

type stringSliceFlag []string

func (value *stringSliceFlag) String() string { return strings.Join(*value, ",") }
func (value *stringSliceFlag) Set(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("value must not be empty")
	}
	*value = append(*value, raw)
	return nil
}

func pathSegment(value string) string {
	replacer := strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "#", "%23")
	return replacer.Replace(value)
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `ForgeGrid CLI

Usage:
  expctl [global flags] health
  expctl [global flags] experiment create|list|get ...
  expctl [global flags] run create|list|get|cancel|events ...
  expctl [global flags] worker register|list ...

Global flags:
  -server URL       control-plane URL (default http://127.0.0.1:8080)
  -timeout DURATION request timeout (default 30s)
	-token-file PATH read bearer token from file (environment fallback: CONTROL_PLANE_API_TOKEN)
  -compact          compact JSON output

Run "expctl COMMAND -h" for command flags.`)
}
