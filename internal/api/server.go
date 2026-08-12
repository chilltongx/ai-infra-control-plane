package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxBodyBytes = 1 << 20
	defaultLeaseTTL     = 30 * time.Second
	maxLeaseTTL         = 10 * time.Minute
	maxClaimWait        = 30 * time.Second
)

type Options struct {
	Logger       *slog.Logger
	MaxBodyBytes int64
	Version      string
	// APIToken enables bearer authentication for /v1/* and /metrics. Health
	// and readiness remain open for infrastructure probes. The token is never
	// logged or included in an error response.
	APIToken string
	// UIHandler, when non-nil, serves the product UI and its assets from "/".
	// Keeping it injectable lets the executable choose an embedded or on-disk
	// filesystem without coupling the API package to frontend layout.
	UIHandler http.Handler
}

type Server struct {
	service      Service
	logger       *slog.Logger
	maxBodyBytes int64
	version      string
	authEnabled  bool
	apiTokenHash [sha256.Size]byte
	uiHandler    http.Handler
	metrics      *metrics
	handler      http.Handler
}

func New(service Service, options Options) *Server {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBodyBytes := options.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	apiToken := strings.TrimSpace(options.APIToken)
	s := &Server{
		service:      service,
		logger:       logger,
		maxBodyBytes: maxBodyBytes,
		version:      options.Version,
		authEnabled:  apiToken != "",
		apiTokenHash: sha256.Sum256([]byte(apiToken)),
		uiHandler:    options.UIHandler,
		metrics:      newMetrics(),
	}
	// requestID must be outermost so the request pointer seen by instrument is
	// the same one ServeMux annotates with its matched route pattern.
	s.handler = s.requestID(s.instrument(s.recover(s.authenticate(s.routes()))))
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

type HTTPOptions struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// ListenAndServe runs until ctx is cancelled or the HTTP server fails. Context
// cancellation triggers a bounded graceful shutdown.
func (s *Server) ListenAndServe(ctx context.Context, address string, options HTTPOptions) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	return s.Serve(ctx, listener, options)
}

// Serve runs on an already-bound listener until ctx is cancelled or the HTTP
// server fails. Owning the listener lets launchers bind port 0 without a
// check-then-bind race and learn the selected address before serving traffic.
func (s *Server) Serve(ctx context.Context, listener net.Listener, options HTTPOptions) error {
	if options.ReadHeaderTimeout <= 0 {
		options.ReadHeaderTimeout = 5 * time.Second
	}
	if options.ReadTimeout <= 0 {
		// Claim long-polls are capped at 30 seconds. Keep transport headroom for
		// request parsing and response serialization around that wait.
		options.ReadTimeout = 45 * time.Second
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = 60 * time.Second
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = 10 * time.Second
	}

	httpServer := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: options.ReadHeaderTimeout,
		ReadTimeout:       options.ReadTimeout,
		IdleTimeout:       options.IdleTimeout,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(listener) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), options.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-shutdownCtx.Done():
			_ = httpServer.Close()
			return fmt.Errorf("wait for HTTP server shutdown: %w", shutdownCtx.Err())
		}
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	if s.uiHandler != nil {
		// Exact UI routes keep the catch-all from masking API 405 responses.
		mux.Handle("GET /{$}", s.uiHandler)
		mux.Handle("GET /app.css", s.uiHandler)
		mux.Handle("GET /app.js", s.uiHandler)
	}
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	mux.HandleFunc("GET /v1/experiments", s.listExperiments)
	mux.HandleFunc("POST /v1/experiments", s.createExperiment)
	mux.HandleFunc("GET /v1/experiments/{experiment_id}", s.getExperiment)
	mux.HandleFunc("GET /v1/experiments/{experiment_id}/runs", s.listRuns)
	mux.HandleFunc("POST /v1/experiments/{experiment_id}/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs/{run_id}", s.getRun)
	mux.HandleFunc("GET /v1/runs/{run_id}/attempts", s.listAttempts)
	mux.HandleFunc("POST /v1/runs/{run_id}/cancel", s.cancelRun)
	mux.HandleFunc("GET /v1/runs/{run_id}/events", s.listEvents)
	mux.HandleFunc("GET /v1/workers", s.listWorkers)
	mux.HandleFunc("POST /v1/workers", s.registerWorker)
	mux.HandleFunc("POST /v1/workers/{worker_id}/heartbeat", s.heartbeatWorker)
	mux.HandleFunc("POST /v1/workers/{worker_id}/claim", s.claim)
	mux.HandleFunc("POST /v1/runs/{run_id}/attempts/{attempt_id}/start", s.startAttempt)
	mux.HandleFunc("POST /v1/runs/{run_id}/attempts/{attempt_id}/heartbeat", s.heartbeatAttempt)
	mux.HandleFunc("POST /v1/runs/{run_id}/attempts/{attempt_id}/complete", s.completeAttempt)
	return mux
}

type contextKey uint8

const (
	requestIDKey contextKey = iota
	idempotencyKey
)

func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

// IdempotencyKey returns the caller-provided mutation key after transport
// validation. Service implementations may use it to deduplicate create calls.
func IdempotencyKey(ctx context.Context) string {
	value, _ := ctx.Value(idempotencyKey).(string)
	return value
}

var safeHeaderValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if len(id) > 128 || !safeHeaderValue.MatchString(id) {
			id = randomID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" {
			if len(key) > 128 || !safeHeaderValue.MatchString(key) {
				writeError(w, id, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be 1-128 URL-safe characters")
				return
			}
			ctx = context.WithValue(ctx, idempotencyKey, key)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled || !protectedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		const prefix = "Bearer "
		authorization := r.Header.Get("Authorization")
		providedHash := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(authorization, prefix))))
		if !strings.HasPrefix(authorization, prefix) || subtle.ConstantTimeCompare(providedHash[:], s.apiTokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-infra-control-plane"`)
			writeError(w, RequestID(r.Context()), http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func protectedPath(path string) bool {
	return path == "/metrics" || path == "/v1" || strings.HasPrefix(path, "/v1/")
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.metrics.recordPanic()
				s.logger.Error("http panic", "request_id", RequestID(r.Context()), "panic", recovered, "stack", string(debug.Stack()))
				writeError(w, RequestID(r.Context()), http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		s.metrics.begin()
		recorder := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		s.metrics.end(r.Method, routePattern(r), status, time.Since(started))
		s.logger.Info("http request", "request_id", RequestID(r.Context()), "method", r.Method, "path", r.URL.Path, "route", routePattern(r), "status", status, "bytes", recorder.bytes, "duration_ms", time.Since(started).Milliseconds())
	})
}

func routePattern(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		return path
	}
	return r.Pattern
}

func randomID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(raw[:])
}

type metricKey struct {
	method string
	route  string
	status int
}

type metricValue struct {
	count    uint64
	duration float64
}

type metrics struct {
	mu       sync.Mutex
	inFlight int64
	panics   uint64
	requests map[metricKey]metricValue
}

func newMetrics() *metrics { return &metrics{requests: make(map[metricKey]metricValue)} }

func (m *metrics) begin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight++
}

func (m *metrics) recordPanic() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panics++
}

func (m *metrics) end(method, route string, status int, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight--
	key := metricKey{method: method, route: route, status: status}
	value := m.requests[key]
	value.count++
	value.duration += duration.Seconds()
	m.requests[key] = value
}
