package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAPIDocumentationEndpoints(t *testing.T) {
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

	response, err := http.Get(server.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var document struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode OpenAPI document: %v", err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Errorf("OpenAPI version = %q, want 3.1.0", document.OpenAPI)
	}
	if document.Info.Title != "Starryeyes API" || document.Info.Version != "1.0.0" {
		t.Errorf("API info = %#v, want Starryeyes API 1.0.0", document.Info)
	}
	for _, path := range []string{
		"/healthz",
		"/v1/capabilities",
		"/v1/jobs",
		"/v1/jobs/{id}/chunks/{chunk}",
		"/v1/jobs/{id}/complete",
		"/v1/jobs/{id}",
		"/v1/jobs/{id}/output",
	} {
		if _, ok := document.Paths[path]; !ok {
			t.Errorf("OpenAPI document is missing %s", path)
		}
	}

	health, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	var body APIHealthResponse
	if err := json.NewDecoder(health.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.StatusCode != http.StatusOK || !body.OK {
		t.Errorf("health response = status %d, body %#v; want 200 and ok=true", health.StatusCode, body)
	}
}
