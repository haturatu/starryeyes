package main

import (
	"database/sql"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateFilename(t *testing.T) {
	valid := []string{
		"clip.mp4",
		"旅行動画.mp4",
		".hidden",
		strings.Repeat("a", 255),
	}
	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := validateFilename(name); err != nil {
				t.Errorf("validateFilename(%q) = %v, want nil", name, err)
			}
		})
	}

	invalid := []string{
		"",
		".",
		"..",
		"../clip.mp4",
		"/tmp/clip.mp4",
		"clip\\copy.mp4",
		"clip\x00.mp4",
		"clip\n.mp4",
		strings.Repeat("a", 256),
	}
	for _, name := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if err := validateFilename(name); err == nil {
				t.Errorf("validateFilename(%q) = nil, want an error", name)
			}
		})
	}
}

func TestDownloadFilename(t *testing.T) {
	for _, test := range []struct {
		name     string
		filename string
		artifact string
		want     string
	}{
		{name: "replace extension", filename: "my-holiday-video.mov", artifact: "output.mp4", want: "my-holiday-video.mp4"},
		{name: "unicode", filename: "旅行動画.mp4", artifact: "output.mkv", want: "旅行動画.mkv"},
		{name: "extensionless input", filename: "recording", artifact: "output.webm", want: "recording.webm"},
		{name: "hidden input", filename: ".recording", artifact: "output.mp4", want: ".recording.mp4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := downloadFilename(Job{Filename: test.filename, Artifact: sqlNullString(test.artifact)}); got != test.want {
				t.Errorf("downloadFilename() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOutputSetsOriginalDownloadFilename(t *testing.T) {
	server, httpServer := newAPITestServer(t)
	jobID := "completed-job"
	artifact := "output.webm"
	artifactPath := filepath.Join(server.outputDir(jobID), artifact)
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("converted media"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`INSERT INTO jobs(id,state,filename,size,expected,spec,reserved,created_at,artifact) VALUES(?,?,?,?,?,?,?,?,?)`, jobID, completed, "旅行動画.mov", 1, 1, "{}", 0, time.Now().UTC(), artifact); err != nil {
		t.Fatal(err)
	}

	response, err := http.Get(httpServer.URL + "/v1/jobs/" + jobID + "/output")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("output status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("parse Content-Disposition: %v", err)
	}
	if mediaType != "attachment" || parameters["filename"] != "旅行動画.webm" {
		t.Errorf("Content-Disposition = %q (%q, %#v), want attachment filename 旅行動画.webm", response.Header.Get("Content-Disposition"), mediaType, parameters)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "converted media" {
		t.Errorf("output body = %q, want converted media", body)
	}
}

func sqlNullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}
