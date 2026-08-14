package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxCapturedProcessStderr = 1000

// splitHandler keeps Docker's stdout/stderr streams useful without making a
// stream choice the source of truth for severity. Consumers should use the
// structured level field when they need to filter records.
type splitHandler struct {
	stdout slog.Handler
	stderr slog.Handler
}

func (h *splitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= slog.LevelWarn {
		return h.stderr.Enabled(ctx, level)
	}
	return h.stdout.Enabled(ctx, level)
}

func (h *splitHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelWarn {
		return h.stderr.Handle(ctx, record)
	}
	return h.stdout.Handle(ctx, record)
}

func (h *splitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &splitHandler{stdout: h.stdout.WithAttrs(attrs), stderr: h.stderr.WithAttrs(attrs)}
}

func (h *splitHandler) WithGroup(name string) slog.Handler {
	return &splitHandler{stdout: h.stdout.WithGroup(name), stderr: h.stderr.WithGroup(name)}
}

func loggerFromEnv() (*slog.Logger, error) {
	level, err := parseLogLevel(env("LOG_LEVEL", "info"))
	if err != nil {
		return nil, err
	}
	format := strings.ToLower(env("LOG_FORMAT", "json"))
	if format != "json" && format != "text" {
		return nil, fmt.Errorf("LOG_FORMAT must be json or text")
	}
	return newLogger(format, level, os.Stdout, os.Stderr), nil
}

func newLogger(format string, level slog.Level, stdout, stderr io.Writer) *slog.Logger {
	options := &slog.HandlerOptions{Level: level}
	var stdoutHandler, stderrHandler slog.Handler
	if format == "text" {
		stdoutHandler = slog.NewTextHandler(stdout, options)
		stderrHandler = slog.NewTextHandler(stderr, options)
	} else {
		stdoutHandler = slog.NewJSONHandler(stdout, options)
		stderrHandler = slog.NewJSONHandler(stderr, options)
	}
	return slog.New(&splitHandler{stdout: stdoutHandler, stderr: stderrHandler})
}

func bootstrapLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func parseLogLevel(value string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(value))); err != nil {
		return 0, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error: %w", err)
	}
	return level, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = b.Buffer.Write(p[:remaining])
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	return b.Buffer.String()
}

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

type requestContextKey string

const (
	requestIDContextKey  requestContextKey = "request_id"
	requestLogContextKey requestContextKey = "request_logger"
)

func requestIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey).(string)
	return value
}

func loggerFromContext(ctx context.Context) *slog.Logger {
	logger, _ := ctx.Value(requestLogContextKey).(*slog.Logger)
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func requestID(headerValue string) string {
	if validRequestID(headerValue) {
		return headerValue
	}
	return id()
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *responseRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func requestLogging(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	logger = logger.With("component", "http")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := requestID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
		ctx = context.WithValue(ctx, requestLogContextKey, logger)
		r = r.WithContext(ctx)

		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" && status < http.StatusBadRequest {
			level = slog.LevelDebug
		}
		attrs := []any{
			"event", "http.request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(started).Milliseconds(),
			"request_id", requestID,
		}
		if jobID := jobIDFromPath(r.URL.Path); jobID != "" {
			attrs = append(attrs, "job_id", jobID)
		}
		logger.Log(r.Context(), level, "http request", attrs...)
	})
}

func jobIDFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "jobs" {
		return parts[2]
	}
	return ""
}

func internalError(w http.ResponseWriter, r *http.Request, status int, err error) {
	requestIDValue := requestIDFromContext(r.Context())
	if requestIDValue == "" {
		requestIDValue = requestID(r.Header.Get("X-Request-ID"))
		w.Header().Set("X-Request-ID", requestIDValue)
	}
	attrs := []any{"event", "http.error", "status", status, "request_id", requestIDValue, "error", err}
	if jobID := jobIDFromPath(r.URL.Path); jobID != "" {
		attrs = append(attrs, "job_id", jobID)
	}
	loggerFromContext(r.Context()).Error("internal server error", attrs...)
	out(w, status, APIError{Error: "internal server error", RequestID: requestIDValue})
}

func (s *Server) component(name string) *slog.Logger {
	if s.log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)).With("component", name)
	}
	return s.log.With("component", name)
}
