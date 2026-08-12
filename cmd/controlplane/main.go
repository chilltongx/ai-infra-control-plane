// controlplane runs ForgeGrid's durable API and embedded UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	controlplane "github.com/chilltongx/ai-infra-control-plane"
	"github.com/chilltongx/ai-infra-control-plane/internal/api"
	"github.com/chilltongx/ai-infra-control-plane/internal/store"
)

var version = "dev"

type config struct {
	address           string
	dataPath          string
	leaseSweep        time.Duration
	workerStaleAfter  time.Duration
	shutdownTimeout   time.Duration
	logLevel          string
	showVersion       bool
	apiToken          string
	listenAddressFile string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "controlplane:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	configuration, err := parseConfig(args, stderr)
	if err != nil {
		return err
	}
	if configuration.showVersion {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}
	level, err := parseLogLevel(configuration.logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	repository, err := store.NewJSONStore(configuration.dataPath, store.WithWorkerHeartbeatTTL(configuration.workerStaleAfter))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	service := store.NewAPIService(repository)
	server := api.New(service, api.Options{
		Logger:    logger,
		Version:   version,
		APIToken:  configuration.apiToken,
		UIHandler: controlplane.UIHandler(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sweepExpiredLeases(ctx, repository, configuration.leaseSweep, logger)
	listener, err := net.Listen("tcp", configuration.address)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	if configuration.listenAddressFile != "" {
		if err := writeListenAddress(configuration.listenAddressFile, listener.Addr().String()); err != nil {
			return err
		}
		defer os.Remove(configuration.listenAddressFile)
	}
	logger.Info("control plane starting", "address", listener.Addr().String(), "data_path", configuration.dataPath, "version", version)
	err = server.Serve(ctx, listener, api.HTTPOptions{ShutdownTimeout: configuration.shutdownTimeout})
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	logger.Info("control plane stopped")
	return nil
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	set := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	set.SetOutput(stderr)
	configuration := config{}
	set.StringVar(&configuration.address, "listen", envOr("CONTROL_PLANE_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	set.StringVar(&configuration.dataPath, "data", envOr("CONTROL_PLANE_DATA", "./data/control-plane.json"), "durable JSON snapshot path")
	set.DurationVar(&configuration.leaseSweep, "lease-sweep", 2*time.Second, "expired lease reconciliation interval")
	set.DurationVar(&configuration.workerStaleAfter, "worker-stale-after", 30*time.Second, "mark workers offline after missed heartbeats")
	set.DurationVar(&configuration.shutdownTimeout, "shutdown-timeout", 10*time.Second, "graceful shutdown timeout")
	set.StringVar(&configuration.logLevel, "log-level", envOr("CONTROL_PLANE_LOG_LEVEL", "info"), "debug, info, warn, or error")
	set.StringVar(&configuration.listenAddressFile, "listen-address-file", "", "atomically write the bound listen address to this file")
	configuration.apiToken = strings.TrimSpace(os.Getenv("CONTROL_PLANE_API_TOKEN"))
	set.BoolVar(&configuration.showVersion, "version", false, "print version and exit")
	if err := set.Parse(args); err != nil {
		return config{}, err
	}
	if set.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	if strings.TrimSpace(configuration.address) == "" || strings.TrimSpace(configuration.dataPath) == "" {
		return config{}, errors.New("listen address and data path are required")
	}
	if configuration.leaseSweep <= 0 || configuration.workerStaleAfter <= 0 || configuration.shutdownTimeout <= 0 {
		return config{}, errors.New("lease-sweep, worker-stale-after, and shutdown-timeout must be positive")
	}
	loopback, err := isLoopbackListenAddress(configuration.address)
	if err != nil {
		return config{}, err
	}
	if !loopback && configuration.apiToken == "" {
		return config{}, errors.New("CONTROL_PLANE_API_TOKEN is required when listening on a non-loopback address")
	}
	return configuration, nil
}

func writeListenAddress(path, address string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("prepare listen address directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".listen-address-*.tmp")
	if err != nil {
		return fmt.Errorf("create listen address file: %w", err)
	}
	temporaryName := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("secure listen address file: %w", err)
	}
	if _, err := fmt.Fprintln(temporary, address); err != nil {
		cleanup()
		return fmt.Errorf("write listen address file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync listen address file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("close listen address file: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		_ = os.Remove(temporaryName)
		return fmt.Errorf("publish listen address file: %w", err)
	}
	return nil
}

func isLoopbackListenAddress(address string) (bool, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false, fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q", raw)
	}
}

func sweepExpiredLeases(ctx context.Context, repository *store.JSONStore, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := repository.ExpireLeases(ctx)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("lease reconciliation failed", "error", err)
				}
				continue
			}
			if len(result.Expired) != 0 {
				logger.Warn("expired leases reconciled", "count", len(result.Expired))
			}
		}
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
