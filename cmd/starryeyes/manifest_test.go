package main

import (
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOutputManifestBackfillsLostDatabase(t *testing.T) {
	outputDir := t.TempDir()
	jobID := "48047daf0e72bd37dd856118a8eed24b"
	artifact := "output.mp4"
	artifactPath := filepath.Join(outputDir, jobID, artifact)
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("converted media"), 0640); err != nil {
		t.Fatal(err)
	}

	writer := newManifestTestServer(t, t.TempDir(), outputDir)
	createdAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	job := Job{
		ID:         jobID,
		Filename:   "旅行動画.mov",
		InputHash:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActualHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Size:       2 << 20,
		CreatedAt:  createdAt,
	}
	output := Output{Container: "mp4", Video: Video{Codec: "h264", Quality: Quality{Mode: "quality", Value: 72}, Resolution: Resolution{Mode: "source"}}, Audio: Audio{Codec: "aac", BitrateKbps: 160}}
	if err := writer.writeOutputManifest(job, output, artifact, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	manifest, err := readOutputManifest(filepath.Join(outputDir, jobID, manifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.JobID != jobID || manifest.OutputSHA256 == "" || manifest.OutputSize != int64(len("converted media")) || manifest.Output.Container != "mp4" {
		t.Errorf("manifest = %#v, want completed output metadata", manifest)
	}

	recovery := newManifestTestServer(t, t.TempDir(), outputDir)
	result, err := recovery.backfillOutputManifests()
	if err != nil {
		t.Fatal(err)
	}
	if result != (backfillResult{Found: 1, Imported: 1}) {
		t.Errorf("backfill result = %#v, want one imported manifest", result)
	}
	restored, err := recovery.job(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != completed || restored.Filename != job.Filename || restored.Size != job.Size || restored.Received != job.Size || restored.Chunks != 2 || restored.Expected != 2 || restored.InputHash != job.InputHash || restored.ActualHash != job.ActualHash || !restored.Artifact.Valid || restored.Artifact.String != artifact {
		t.Errorf("restored job = %#v, want completed manifest data", restored)
	}

	result, err = recovery.backfillOutputManifests()
	if err != nil {
		t.Fatal(err)
	}
	if result != (backfillResult{Found: 1, Existing: 1}) {
		t.Errorf("second backfill result = %#v, want idempotent existing job", result)
	}
}

func TestBackfillRejectsModifiedArtifact(t *testing.T) {
	outputDir := t.TempDir()
	jobID := "a8047daf0e72bd37dd856118a8eed24b"
	artifactPath := filepath.Join(outputDir, jobID, "output.mp4")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("original output"), 0640); err != nil {
		t.Fatal(err)
	}
	writer := newManifestTestServer(t, t.TempDir(), outputDir)
	job := Job{ID: jobID, Filename: "clip.mp4", ActualHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 1 << 20, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	output := Output{Container: "mp4", Video: Video{Codec: "h264", Quality: Quality{Mode: "quality", Value: 72}, Resolution: Resolution{Mode: "source"}}, Audio: Audio{Codec: "aac", BitrateKbps: 160}}
	if err := writer.writeOutputManifest(job, output, "output.mp4", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("modified output"), 0640); err != nil {
		t.Fatal(err)
	}

	recovery := newManifestTestServer(t, t.TempDir(), outputDir)
	result, err := recovery.backfillOutputManifests()
	if err == nil {
		t.Fatal("backfill accepted an artifact whose checksum no longer matches")
	}
	if result != (backfillResult{Found: 1, Invalid: 1}) {
		t.Errorf("backfill result = %#v, want one invalid manifest", result)
	}
	if _, err := recovery.job(jobID); err != sql.ErrNoRows {
		t.Errorf("tampered artifact restored a job: %v", err)
	}
}

func TestJobCreatedAtCanBeStoredInManifest(t *testing.T) {
	server := newManifestTestServer(t, t.TempDir(), t.TempDir())
	createdAt := time.Now().UTC()
	if _, err := server.db.Exec(`INSERT INTO jobs(id,state,filename,size,expected,spec,reserved,created_at) VALUES(?,?,?,?,?,?,?,?)`, "job", pending, "clip.mp4", 1, 1, `{}`, 0, createdAt); err != nil {
		t.Fatal(err)
	}
	job, err := server.job("job")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseDatabaseTime(job.CreatedAt)
	if err != nil {
		t.Fatalf("parse job CreatedAt %q: %v", job.CreatedAt, err)
	}
	if parsed.IsZero() {
		t.Error("parsed job CreatedAt is zero")
	}
}

func TestRunBackfillRecreatesDatabase(t *testing.T) {
	outputDir := t.TempDir()
	jobID := "c8047daf0e72bd37dd856118a8eed24b"
	artifactPath := filepath.Join(outputDir, jobID, "output.mp4")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("converted media"), 0640); err != nil {
		t.Fatal(err)
	}
	writer := newManifestTestServer(t, t.TempDir(), outputDir)
	job := Job{ID: jobID, Filename: "clip.mp4", ActualHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 1 << 20, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	output := Output{Container: "mp4", Video: Video{Codec: "h264", Quality: Quality{Mode: "quality", Value: 72}, Resolution: Resolution{Mode: "source"}}, Audio: Audio{Codec: "aac", BitrateKbps: 160}}
	if err := writer.writeOutputManifest(job, output, "output.mp4", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	if err := runBackfill(Config{Data: dataDir, Output: outputDir, Capacity: 8 << 20, Chunk: 1 << 20, Active: 1}); err != nil {
		t.Fatal(err)
	}
	recovered := newManifestTestServer(t, dataDir, outputDir)
	if restored, err := recovered.job(jobID); err != nil || restored.State != completed {
		t.Errorf("backfill command restored %#v, %v; want completed job", restored, err)
	}
}

func newManifestTestServer(t *testing.T, dataDir, outputDir string) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "jobs.sqlite?_pragma=foreign_keys(ON)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server := &Server{
		db:  db,
		cfg: Config{Data: dataDir, Output: outputDir, Capacity: 8 << 20, Chunk: 1 << 20, Active: 1},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sem: make(chan struct{}, 1),
	}
	if err := server.schema(); err != nil {
		t.Fatal(err)
	}
	return server
}
