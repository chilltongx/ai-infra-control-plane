package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chilltongx/ai-infra-control-plane/internal/worker"
)

var version = "dev"

func main() {
	if handled, err := runInternal(os.Args[1:]); handled {
		if err != nil {
			slog.Error("internal worker command failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func runInternal(args []string) (bool, error) {
	if len(args) == 0 || args[0] != worker.InternalSleepCommand {
		return false, nil
	}
	if len(args) != 2 {
		return true, errors.New("internal sleep requires exactly one duration")
	}
	duration, err := time.ParseDuration(args[1])
	if err != nil || duration < 0 || duration > 24*time.Hour {
		return true, fmt.Errorf("invalid internal sleep duration %q", args[1])
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	<-timer.C
	return true, nil
}

func run() error {
	var (
		controlPlane = flag.String("control-plane", envOr("CONTROL_PLANE_URL", "http://127.0.0.1:8080"), "control-plane base URL")
		workerID     = flag.String("id", os.Getenv("WORKER_ID"), "stable worker ID (optional)")
		sessionID    = flag.String("session-id", os.Getenv("WORKER_SESSION_ID"), "worker process session ID (generated when empty)")
		name         = flag.String("name", envOr("WORKER_NAME", hostname()), "worker name")
		adapter      = flag.String("adapter", envOr("WORKER_ADAPTER", worker.AdapterDemoSleep), "recipe adapter")
		labels       = flag.String("labels", os.Getenv("WORKER_LABELS"), "comma-separated key=value worker labels")
		artifacts    = flag.String("artifacts", envOr("WORKER_ARTIFACT_ROOT", "./artifacts"), "artifact root")
		leaseTTL     = flag.Duration("lease-ttl", 30*time.Second, "attempt lease TTL")
		claimWait    = flag.Duration("claim-wait", 20*time.Second, "long-poll claim wait")
		oneShot      = flag.Bool("one-shot", false, "claim at most one attempt, then exit")
	)
	flag.Parse()
	parsedLabels, err := parseLabels(*labels)
	if err != nil {
		return err
	}
	client, err := worker.NewHTTPClient(*controlPlane, nil)
	if err != nil {
		return err
	}
	runner, err := worker.NewRunner(client, worker.Config{
		ID: *workerID, Name: *name, Adapter: *adapter, Labels: parsedLabels, SessionID: *sessionID,
		Version: version, ArtifactRoot: *artifacts, LeaseTTL: *leaseTTL, ClaimWait: *claimWait,
		OneShot: *oneShot,
	}, slog.Default())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = runner.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func parseLabels(input string) (map[string]string, error) {
	result := make(map[string]string)
	for _, entry := range strings.Split(input, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("invalid label %q; expected key=value", entry)
		}
		result[key] = value
	}
	return result, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "local-worker"
	}
	return name
}
