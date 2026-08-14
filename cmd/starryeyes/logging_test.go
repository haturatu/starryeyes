package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"
)

func TestSplitHandlerRoutesByLevel(t *testing.T) {
	var stdout, stderr bytes.Buffer
	logger := newLogger("json", slog.LevelDebug, &stdout, &stderr)
	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	if got := strings.Count(stdout.String(), "\n"); got != 2 {
		t.Fatalf("stdout records = %d, want 2", got)
	}
	if got := strings.Count(stderr.String(), "\n"); got != 2 {
		t.Fatalf("stderr records = %d, want 2", got)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode stdout record: %v", err)
		}
		if record["level"] != "DEBUG" && record["level"] != "INFO" {
			t.Errorf("stdout level = %v, want DEBUG or INFO", record["level"])
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode stderr record: %v", err)
		}
		if record["level"] != "WARN" && record["level"] != "ERROR" {
			t.Errorf("stderr level = %v, want WARN or ERROR", record["level"])
		}
	}
}

func TestLoggerLevelAndFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	logger := newLogger("text", slog.LevelInfo, &stdout, &stderr)
	logger.Debug("hidden")
	logger.Info("visible")
	if strings.Contains(stdout.String(), "hidden") || !strings.Contains(stdout.String(), "visible") {
		t.Errorf("stdout = %q, want only visible info record", stdout.String())
	}
	if _, err := parseLogLevel("verbose"); err == nil {
		t.Fatal("parseLogLevel(verbose) succeeded")
	}
}

func TestBoundedBuffer(t *testing.T) {
	var buffer boundedBuffer
	buffer.limit = 4
	if n, err := buffer.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write() = (%d, %v), want (6, nil)", n, err)
	}
	if got := buffer.String(); got != "abcd" {
		t.Errorf("buffer = %q, want abcd", got)
	}
	if !buffer.truncated {
		t.Error("buffer.truncated = false, want true")
	}
}

func TestValidRequestID(t *testing.T) {
	for _, value := range []string{"abc123", "trace-123_foo.bar", strings.Repeat("a", 128)} {
		if !validRequestID(value) {
			t.Errorf("validRequestID(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "has space", "has/slash", "line\nbreak", strings.Repeat("a", 129)} {
		if validRequestID(value) {
			t.Errorf("validRequestID(%q) = true, want false", value)
		}
	}
}

func TestRequestLoggingReplacesInvalidRequestID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	logger := newLogger("json", slog.LevelDebug, &stdout, &stderr)
	handler := requestLogging(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-123", nil)
	request.Header.Set("X-Request-ID", strings.Repeat("x", 129))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	got := recorder.Header().Get("X-Request-ID")
	if got == "" || got == request.Header.Get("X-Request-ID") || !validRequestID(got) {
		t.Fatalf("response request ID = %q, want generated valid request ID", got)
	}
	if !strings.Contains(stdout.String(), `"request_id":"`+got+`"`) {
		t.Errorf("stdout = %q, want generated request ID", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty stderr", stderr.String())
	}
}

func TestRequestLoggingKeepsClientErrorsInformational(t *testing.T) {
	var stdout, stderr bytes.Buffer
	logger := newLogger("json", slog.LevelDebug, &stdout, &stderr)
	handler := requestLogging(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/jobs", nil))

	if !strings.Contains(stdout.String(), `"event":"http.request"`) || !strings.Contains(stdout.String(), `"status":422`) || !strings.Contains(stdout.String(), `"level":"INFO"`) {
		t.Errorf("stdout = %q, want informational 422 access log", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want no warning/error access log", stderr.String())
	}
}

func TestRequestLoggingAndInternalError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	logger := newLogger("json", slog.LevelDebug, &stdout, &stderr)
	handler := requestLogging(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalError(w, r, http.StatusInternalServerError, errors.New("database is locked"))
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-123", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	var response APIError
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "internal server error" || response.RequestID == "" {
		t.Errorf("API error = %#v, want generic error with request ID", response)
	}
	if recorder.Header().Get("X-Request-ID") != response.RequestID {
		t.Errorf("response request ID = %q, body request ID = %q", recorder.Header().Get("X-Request-ID"), response.RequestID)
	}
	if !strings.Contains(stderr.String(), "database is locked") || !strings.Contains(stderr.String(), "job-123") {
		t.Errorf("stderr = %q, want internal error and job ID", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"http.error"`) {
		t.Errorf("stderr = %q, want http.error event", stderr.String())
	}
	if strings.Contains(stderr.String(), `"event":"http.request"`) {
		t.Errorf("stderr = %q, http.request must not be an error record", stderr.String())
	}
	if !strings.Contains(stdout.String(), `"event":"http.request"`) || !strings.Contains(stdout.String(), `"level":"INFO"`) {
		t.Errorf("stdout = %q, want informational access log", stdout.String())
	}
}
