package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenAPIContract(t *testing.T) {
	server := httptest.NewServer(newRouter(&Server{cfg: Config{
		Chunk:      64 << 20,
		MaxWidth:   7680,
		MaxHeight:  4320,
		MaxStreams: 64,
	}}))
	t.Cleanup(server.Close)

	for _, path := range []string{
		"/docs",
		"/openapi.json",
		"/openapi.yaml",
		"/openapi-3.0.json",
		"/openapi-3.0.yaml",
		"/schemas/APIHealthResponse.json",
	} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s: got status %d, want %d", path, response.StatusCode, http.StatusOK)
		}
	}

	document := openAPIDocument(t, server.URL)
	if document["openapi"] != "3.1.0" {
		t.Errorf("OpenAPI version = %q, want 3.1.0", document["openapi"])
	}
	info := object(t, document["info"])
	if info["title"] != "Starryeyes API" || info["version"] != "2.0.0" {
		t.Errorf("API info = %#v, want Starryeyes API 2.0.0", info)
	}

	paths := object(t, document["paths"])
	for _, path := range []string{
		"/healthz",
		"/v1/capabilities",
		"/v1/jobs",
		"/v1/jobs/{id}/chunks",
		"/v1/jobs/{id}/chunks/{chunk}",
		"/v1/jobs/{id}/complete",
		"/v1/jobs/{id}",
		"/v1/jobs/{id}/output",
	} {
		if _, ok := paths[path]; !ok {
			t.Errorf("OpenAPI document is missing %s", path)
		}
	}
	assertUniqueOperationIDs(t, paths)

	create := operation(t, paths, "/v1/jobs", "post")
	if _, ok := content(t, object(t, create["requestBody"]), "application/json"); !ok {
		t.Error("createJob does not document an application/json request body")
	}
	if _, ok := object(t, create["responses"])["201"]; !ok {
		t.Error("createJob does not document a 201 response")
	}
	assertRequiredHeader(t, create, "Idempotency-Key")

	listChunks := operation(t, paths, "/v1/jobs/{id}/chunks", "get")
	if _, ok := object(t, listChunks["responses"])["200"]; !ok {
		t.Error("listVerifiedChunks does not document a 200 response")
	}

	chunk := operation(t, paths, "/v1/jobs/{id}/chunks/{chunk}", "put")
	if _, ok := content(t, object(t, chunk["requestBody"]), "application/octet-stream"); !ok {
		t.Error("uploadChunk does not document an application/octet-stream request body")
	}
	assertChecksumHeader(t, chunk)

	complete := operation(t, paths, "/v1/jobs/{id}/complete", "post")
	if _, ok := object(t, complete["responses"])["202"]; !ok {
		t.Error("completeJob does not document a 202 response")
	}

	output := operation(t, paths, "/v1/jobs/{id}/output", "get")
	outputResponse := object(t, object(t, output["responses"])["200"])
	outputContent := object(t, outputResponse["content"])
	for _, mediaType := range []string{"video/mp4", "video/webm", "video/x-matroska", "application/octet-stream"} {
		media, ok := outputContent[mediaType]
		if !ok {
			t.Errorf("downloadOutput does not document %s", mediaType)
			continue
		}
		if object(t, object(t, media)["schema"])["format"] != "binary" {
			t.Errorf("downloadOutput %s response is not documented as binary", mediaType)
		}
	}

	schemas := object(t, object(t, document["components"])["schemas"])
	assertRequestSchemaConstraints(t, schemas)
}

func TestAPITypedResponses(t *testing.T) {
	service, server := newAPITestServer(t)
	payload := []byte(`{"input":{"filename":"clip.mp4","size":1024},"output":{"preset":"web-1080p"}}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "typed-response-test-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create response status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	var created APICreateJobResponse
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" || created.State != pending || created.ReservationBytes != 0 || created.Upload != nil {
		t.Errorf("create response = %#v, want a pending job without upload instructions", created)
	}
	service.admitPending()

	jobResponse, err := http.Get(server.URL + "/v1/jobs/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer jobResponse.Body.Close()
	if jobResponse.StatusCode != http.StatusOK {
		t.Fatalf("get response status = %d, want %d", jobResponse.StatusCode, http.StatusOK)
	}
	var job APIJobResponse
	if err := json.NewDecoder(jobResponse.Body).Decode(&job); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if job.ID != created.ID || job.State != admitted || job.Filename != "clip.mp4" || job.Error != nil || job.OutputURL != nil || job.Upload == nil || job.Upload.Mode != "chunked" || job.Upload.ChunkSize == 0 || job.Upload.RequiredHeader != "X-Chunk-SHA256" {
		t.Errorf("job response = %#v, want an admitted job with upload instructions", job)
	}

	request, err = http.NewRequest(http.MethodPost, server.URL+"/v1/jobs", bytes.NewBufferString("{"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "invalid-json-test-key")
	invalid, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer invalid.Body.Close()
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d, want %d", invalid.StatusCode, http.StatusBadRequest)
	}
	var apiError APIError
	if err := json.NewDecoder(invalid.Body).Decode(&apiError); err != nil {
		t.Fatalf("decode API error: %v", err)
	}
	if apiError.Error == "" {
		t.Error("invalid create returned an empty API error")
	}
}

func TestUploadAdmissionQueue(t *testing.T) {
	service, server := newAPITestServer(t)
	const inputSize = int64(2 << 20)
	if _, err := service.db.Exec(`UPDATE capacity SET total=?`, inputSize); err != nil {
		t.Fatal(err)
	}

	first := createAPITestJob(t, server.URL, inputSize)
	second := createAPITestJob(t, server.URL, inputSize)
	if first.State != pending || second.State != pending {
		t.Fatalf("create states = %q, %q; want both PENDING", first.State, second.State)
	}

	service.admitPending()
	firstJob, err := service.job(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := service.job(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstJob.State == secondJob.State {
		t.Fatalf("admission states = %q, %q; want exactly one admitted", firstJob.State, secondJob.State)
	}
	admittedJob, pendingID := firstJob, second.ID
	if secondJob.State == admitted {
		admittedJob, pendingID = secondJob, first.ID
	}
	if admittedJob.Reserved != inputSize {
		t.Errorf("admitted reservation = %d, want %d", admittedJob.Reserved, inputSize)
	}
	service.release(admittedJob)
	service.admitPending()
	if job := waitForJobState(t, service, pendingID, admitted); job.Reserved != inputSize {
		t.Errorf("newly admitted reservation = %d, want %d", job.Reserved, inputSize)
	}
}

func createAPITestJob(t *testing.T, baseURL string, size int64) APICreateJobResponse {
	t.Helper()
	payload := []byte(`{"input":{"filename":"clip.mp4","size":` + strconv.FormatInt(size, 10) + `},"output":{"preset":"web-1080p"}}`)
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/jobs", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", id()+id())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create response status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	var job APICreateJobResponse
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return job
}

func waitForJobState(t *testing.T, service *Server, id string, states ...string) Job {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, err := service.job(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if job.State == state {
				return job
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, err := service.job(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("job %s state = %s, want one of %v", id, job.State, states)
	return Job{}
}

func newAPITestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dataDir := t.TempDir()
	outputDir := t.TempDir()
	for _, path := range []string{dataDir, filepath.Join(dataDir, "spool"), outputDir} {
		if err := os.MkdirAll(path, 0750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "jobs.sqlite?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	server := &Server{
		db:  db,
		cfg: Config{Data: dataDir, Output: outputDir, Capacity: 8 << 20, Chunk: 1 << 20, Active: 1, MaxWidth: 7680, MaxHeight: 4320, MaxStreams: 64, MaxDuration: 86400, UploadRetention: 7 * 24 * time.Hour},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem: make(chan struct{}, 1),
	}
	if err := server.schema(); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(newRouter(server))
	t.Cleanup(httpServer.Close)
	return server, httpServer
}

func openAPIDocument(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	response, err := http.Get(baseURL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var document map[string]any
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode OpenAPI document: %v", err)
	}
	return document
}

func operation(t *testing.T, paths map[string]any, path, method string) map[string]any {
	t.Helper()
	item, ok := paths[path]
	if !ok {
		t.Fatalf("OpenAPI document is missing path %s", path)
	}
	operation, ok := object(t, item)[method]
	if !ok {
		t.Fatalf("OpenAPI document is missing %s %s", method, path)
	}
	return object(t, operation)
}

func content(t *testing.T, requestBody map[string]any, mediaType string) (map[string]any, bool) {
	t.Helper()
	content, ok := requestBody["content"]
	if !ok {
		return nil, false
	}
	value, ok := object(t, content)[mediaType]
	if !ok {
		return nil, false
	}
	return object(t, value), true
}

func assertChecksumHeader(t *testing.T, operation map[string]any) {
	t.Helper()
	parameters, ok := operation["parameters"].([]any)
	if !ok {
		t.Fatal("uploadChunk has no documented parameters")
	}
	for _, parameter := range parameters {
		value := object(t, parameter)
		if value["name"] != "X-Chunk-SHA256" || value["in"] != "header" {
			continue
		}
		if value["required"] != true {
			t.Error("X-Chunk-SHA256 is not required")
		}
		schema := object(t, value["schema"])
		if schema["pattern"] != "^[0-9a-fA-F]{64}$" || schema["minLength"] != float64(64) || schema["maxLength"] != float64(64) {
			t.Errorf("X-Chunk-SHA256 schema = %#v, want a 64-character hexadecimal SHA-256", schema)
		}
		return
	}
	t.Error("uploadChunk does not document X-Chunk-SHA256")
}

func assertRequiredHeader(t *testing.T, operation map[string]any, name string) {
	t.Helper()
	parameters, ok := operation["parameters"].([]any)
	if !ok {
		t.Fatalf("operation has no documented %s header", name)
	}
	for _, parameter := range parameters {
		value := object(t, parameter)
		if value["name"] == name && value["in"] == "header" && value["required"] == true {
			return
		}
	}
	t.Errorf("operation does not document required %s header", name)
}

func assertRequestSchemaConstraints(t *testing.T, schemas map[string]any) {
	t.Helper()
	input := object(t, schemas["Input"])
	inputProperties := object(t, input["properties"])
	if object(t, inputProperties["size"])["minimum"] != float64(1) {
		t.Errorf("input size schema = %#v, want minimum 1", inputProperties["size"])
	}
	if object(t, inputProperties["sha256"])["pattern"] != "^[0-9a-fA-F]{64}$" {
		t.Errorf("input SHA-256 schema = %#v, want hexadecimal checksum pattern", inputProperties["sha256"])
	}
	if object(t, inputProperties["filename"])["minLength"] != float64(1) {
		t.Errorf("input filename schema = %#v, want minLength 1", inputProperties["filename"])
	}

	output := object(t, schemas["Output"])
	outputProperties := object(t, output["properties"])
	assertEnum(t, object(t, outputProperties["preset"]), "web-1080p", "archive-av1")
	assertEnum(t, object(t, outputProperties["container"]), "mp4", "mkv", "webm")
	if !strings.Contains(object(t, outputProperties["preset"])["description"].(string), "override") {
		t.Error("preset schema does not explain that explicit output fields override preset defaults")
	}
	video := object(t, schemas["Video"])
	videoProperties := object(t, video["properties"])
	assertEnum(t, object(t, videoProperties["codec"]), "h264", "hevc", "av1", "vp9")
	quality := object(t, schemas["Quality"])
	qualityProperties := object(t, quality["properties"])
	assertEnum(t, object(t, qualityProperties["mode"]), "quality", "crf")
	if object(t, qualityProperties["value"])["minimum"] != float64(0) || object(t, qualityProperties["value"])["maximum"] != float64(100) {
		t.Errorf("quality value schema = %#v, want 0 through 100", qualityProperties["value"])
	}
	if object(t, qualityProperties["crf"])["minimum"] != float64(0) || object(t, qualityProperties["crf"])["maximum"] != float64(63) {
		t.Errorf("CRF schema = %#v, want 0 through 63", qualityProperties["crf"])
	}
	if !strings.Contains(object(t, qualityProperties["crf"])["description"].(string), "h264 and hevc allow 0 through 51") {
		t.Error("CRF schema does not explain codec-specific CRF ranges")
	}
	resolution := object(t, schemas["Resolution"])
	resolutionProperties := object(t, resolution["properties"])
	assertEnum(t, object(t, resolutionProperties["mode"]), "source", "fit")
	if object(t, resolutionProperties["width"])["minimum"] != float64(2) || object(t, resolutionProperties["height"])["minimum"] != float64(2) {
		t.Errorf("resolution schema = %#v, want a minimum of 2 pixels", resolutionProperties)
	}
	audio := object(t, schemas["Audio"])
	audioProperties := object(t, audio["properties"])
	assertEnum(t, object(t, audioProperties["codec"]), "aac", "opus", "flac")
	bitrate := object(t, audioProperties["bitrate_kbps"])
	if bitrate["minimum"] != float64(16) || bitrate["maximum"] != float64(512) {
		t.Errorf("audio bitrate schema = %#v, want 16 through 512", bitrate)
	}
}

func assertEnum(t *testing.T, schema map[string]any, want ...string) {
	t.Helper()
	values, ok := schema["enum"].([]any)
	if !ok || len(values) != len(want) {
		t.Errorf("enum = %#v, want %#v", schema["enum"], want)
		return
	}
	for index, value := range want {
		if values[index] != value {
			t.Errorf("enum = %#v, want %#v", values, want)
			return
		}
	}
}

func assertUniqueOperationIDs(t *testing.T, paths map[string]any) {
	t.Helper()
	ids := map[string]string{}
	for path, item := range paths {
		for method, operation := range object(t, item) {
			if method != "get" && method != "post" && method != "put" {
				continue
			}
			id, _ := object(t, operation)["operationId"].(string)
			if previous, ok := ids[id]; ok {
				t.Errorf("operationId %q is used by %s and %s", id, previous, path)
			}
			ids[id] = path
		}
	}
}

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value %#v is not an OpenAPI object", value)
	}
	return object
}
