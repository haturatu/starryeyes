package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerate(t *testing.T) {
	outputDir := t.TempDir()
	if err := generate(outputDir); err != nil {
		t.Fatal(err)
	}

	openAPI, err := os.ReadFile(filepath.Join(outputDir, "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		OpenAPI string `json:"openapi"`
		Servers []struct {
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"servers"`
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPI, &document); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Errorf("OpenAPI version = %q, want 3.1.0", document.OpenAPI)
	}
	if len(document.Servers) != 1 || document.Servers[0].URL != exampleServerURL || document.Servers[0].Description != exampleServerDescription {
		t.Errorf("servers = %#v, want the documented local example", document.Servers)
	}
	if _, ok := document.Paths["/v1/jobs"]; !ok {
		t.Error("OpenAPI document is missing /v1/jobs")
	}

	index, err := os.ReadFile(filepath.Join(outputDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(index) != indexHTML {
		t.Error("index.html does not contain the expected Scalar site")
	}
}
