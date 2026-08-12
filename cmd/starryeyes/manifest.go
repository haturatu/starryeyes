package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"time"
)

const manifestFilename = "manifest.json"

// OutputManifest is durable metadata stored beside each completed artifact.
// It is deliberately sufficient to rebuild a lost job database.
type OutputManifest struct {
	Version              int    `json:"version"`
	JobID                string `json:"job_id"`
	OriginalFilename     string `json:"original_filename"`
	Artifact             string `json:"artifact"`
	Container            string `json:"container"`
	RequestedInputSHA256 string `json:"requested_input_sha256,omitempty"`
	InputSHA256          string `json:"input_sha256"`
	OutputSHA256         string `json:"output_sha256"`
	InputSize            int64  `json:"input_size"`
	OutputSize           int64  `json:"output_size"`
	ChunkSize            int64  `json:"chunk_size"`
	CreatedAt            string `json:"created_at"`
	CompletedAt          string `json:"completed_at"`
	Output               Output `json:"output"`
}

type backfillResult struct {
	Found, Imported, Updated, Existing, Conflicts, Invalid int
}

var errCompletedManifestConflict = errors.New("completed job conflicts with output manifest")

func (s *Server) writeOutputManifest(j Job, output Output, artifact string, completedAt time.Time) error {
	if j.CreatedAt == "" || j.ActualHash == "" {
		return errors.New("completed job metadata is incomplete")
	}
	createdAt, err := parseDatabaseTime(j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parse job creation time: %w", err)
	}
	artifactPath := filepath.Join(s.outputDir(j.ID), artifact)
	info, err := os.Stat(artifactPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("artifact is not a regular file")
	}
	outputSHA256, err := hashFile(artifactPath)
	if err != nil {
		return err
	}
	manifest := OutputManifest{
		Version:              1,
		JobID:                j.ID,
		OriginalFilename:     j.Filename,
		Artifact:             artifact,
		Container:            output.Container,
		RequestedInputSHA256: j.InputHash,
		InputSHA256:          j.ActualHash,
		OutputSHA256:         outputSHA256,
		InputSize:            j.Size,
		OutputSize:           info.Size(),
		ChunkSize:            j.ChunkSize,
		CreatedAt:            createdAt.Format(time.RFC3339Nano),
		CompletedAt:          completedAt.Format(time.RFC3339Nano),
		Output:               output,
	}
	return writeJSONAtomically(filepath.Join(s.outputDir(j.ID), manifestFilename), manifest)
}

func parseDatabaseTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}

func writeJSONAtomically(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0640); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDirectory(dir, (*os.File).Sync)
}

// syncDirectory persists the rename on filesystems that support directory
// fsync. Some FUSE filesystems report EINVAL or EOPNOTSUPP for it; the
// already-synced manifest file and rename remain valid, so those two errors
// are explicitly best-effort rather than making a completed transcode fail.
func syncDirectory(dir string, sync func(*os.File) error) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := sync(d); err != nil && !directorySyncUnsupported(err) {
		return err
	}
	return nil
}

func directorySyncUnsupported(err error) bool {
	// ENOTSUP is an alias for EOPNOTSUPP on Linux.
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EOPNOTSUPP)
}

func runBackfill(cfg Config) error {
	for _, dir := range []string{cfg.Data, filepath.Join(cfg.Data, "spool"), cfg.Output} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(cfg.Data, "jobs.sqlite?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	server := &Server{db: db, cfg: cfg, log: slog.Default(), sem: make(chan struct{}, cfg.Active)}
	if err = server.schema(); err != nil {
		return err
	}
	result, err := server.backfillOutputManifests()
	server.log.Info("output manifest backfill complete", "found", result.Found, "imported", result.Imported, "updated", result.Updated, "existing", result.Existing, "conflicts", result.Conflicts, "invalid", result.Invalid)
	return err
}

func (s *Server) backfillOutputManifests() (backfillResult, error) {
	entries, err := os.ReadDir(s.cfg.Output)
	if err != nil {
		return backfillResult{}, err
	}
	var result backfillResult
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifestPath := filepath.Join(s.cfg.Output, entry.Name(), manifestFilename)
		if _, err := os.Stat(manifestPath); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			result.Invalid++
			s.log.Error("read output manifest", "path", manifestPath, "error", err)
			continue
		}
		result.Found++
		manifest, err := readOutputManifest(manifestPath)
		if err == nil {
			err = validateManifestArtifact(s.cfg.Output, entry.Name(), manifest)
		}
		if err != nil {
			result.Invalid++
			s.log.Error("skip invalid output manifest", "path", manifestPath, "error", err)
			continue
		}
		status, err := s.importOutputManifest(manifest)
		if err != nil {
			if errors.Is(err, errCompletedManifestConflict) {
				result.Conflicts++
				s.log.Error("completed job conflicts with output manifest", "path", manifestPath, "error", err)
				continue
			}
			result.Invalid++
			s.log.Error("import output manifest", "path", manifestPath, "error", err)
			continue
		}
		switch status {
		case "imported":
			result.Imported++
		case "updated":
			result.Updated++
		case "existing_identical":
			result.Existing++
		}
	}
	if result.Invalid > 0 || result.Conflicts > 0 {
		return result, fmt.Errorf("backfill completed with %d invalid manifest(s) and %d completed-job conflict(s)", result.Invalid, result.Conflicts)
	}
	return result, nil
}

func readOutputManifest(path string) (OutputManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return OutputManifest{}, err
	}
	var manifest OutputManifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		return OutputManifest{}, err
	}
	return manifest, nil
}

func validateManifestArtifact(outputRoot, directory string, manifest OutputManifest) error {
	if manifest.Version != 1 || !validJobID(manifest.JobID) || manifest.JobID != directory {
		return errors.New("invalid manifest identity")
	}
	if validateFilename(manifest.OriginalFilename) != nil || manifest.Artifact == "" || filepath.Base(manifest.Artifact) != manifest.Artifact {
		return errors.New("invalid manifest filename or artifact")
	}
	if manifest.InputSize <= 0 || manifest.OutputSize < 0 || manifest.ChunkSize <= 0 || !validSHA256(manifest.InputSHA256) || !validSHA256(manifest.OutputSHA256) {
		return errors.New("invalid manifest sizes or checksums")
	}
	if manifest.RequestedInputSHA256 != "" && !validSHA256(manifest.RequestedInputSHA256) {
		return errors.New("invalid requested input checksum")
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return fmt.Errorf("invalid created_at: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CompletedAt); err != nil {
		return fmt.Errorf("invalid completed_at: %w", err)
	}
	if manifest.Container != manifest.Output.Container || !compatible(manifest.Output) || filepath.Ext(manifest.Artifact) != "."+manifest.Container {
		return errors.New("invalid output specification")
	}
	artifactPath := filepath.Join(outputRoot, directory, manifest.Artifact)
	info, err := os.Stat(artifactPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != manifest.OutputSize {
		return errors.New("artifact size does not match manifest")
	}
	checksum, err := hashFile(artifactPath)
	if err != nil {
		return err
	}
	if checksum != manifest.OutputSHA256 {
		return errors.New("artifact checksum does not match manifest")
	}
	return nil
}

func (s *Server) importOutputManifest(manifest OutputManifest) (string, error) {
	spec, err := json.Marshal(manifest.Output)
	if err != nil {
		return "", err
	}
	expected := int((manifest.InputSize + manifest.ChunkSize - 1) / manifest.ChunkSize)
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var existing manifestJob
	err = tx.QueryRow(`SELECT state,filename,size,received,chunks,expected,chunk_size,spec,COALESCE(input_hash,''),COALESCE(actual_hash,''),reserved,COALESCE(error,''),COALESCE(artifact,''),created_at,COALESCE(finished_at,'') FROM jobs WHERE id=?`, manifest.JobID).Scan(&existing.State, &existing.Filename, &existing.Size, &existing.Received, &existing.Chunks, &existing.Expected, &existing.ChunkSize, &existing.Spec, &existing.InputHash, &existing.ActualHash, &existing.Reserved, &existing.Error, &existing.Artifact, &existing.CreatedAt, &existing.FinishedAt)
	if err == nil && existing.State == completed {
		if !existing.matchesManifest(manifest, expected) {
			return "existing_conflict", errCompletedManifestConflict
		}
		return "existing_identical", tx.Commit()
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if existing.State == "" {
		_, err = tx.Exec(`INSERT INTO jobs(id,state,filename,size,received,chunks,expected,chunk_size,spec,input_hash,actual_hash,reserved,artifact,created_at,finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, manifest.JobID, completed, manifest.OriginalFilename, manifest.InputSize, manifest.InputSize, expected, expected, manifest.ChunkSize, string(spec), manifest.RequestedInputSHA256, manifest.InputSHA256, 0, manifest.Artifact, manifest.CreatedAt, manifest.CompletedAt)
		if err != nil {
			return "", err
		}
		return "imported", tx.Commit()
	}
	if _, err = tx.Exec(`DELETE FROM reservations WHERE job_id=?`, manifest.JobID); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`DELETE FROM chunks WHERE job_id=?`, manifest.JobID); err != nil {
		return "", err
	}
	if existing.Reserved > 0 {
		if _, err = tx.Exec(`UPDATE capacity SET reserved=MAX(reserved-?,0) WHERE id=1`, existing.Reserved); err != nil {
			return "", err
		}
	}
	_, err = tx.Exec(`UPDATE jobs SET state=?,filename=?,size=?,received=?,chunks=?,expected=?,chunk_size=?,spec=?,input_hash=?,actual_hash=?,reserved=0,error=NULL,artifact=?,expires_at=NULL,created_at=?,finished_at=? WHERE id=?`, completed, manifest.OriginalFilename, manifest.InputSize, manifest.InputSize, expected, expected, manifest.ChunkSize, string(spec), manifest.RequestedInputSHA256, manifest.InputSHA256, manifest.Artifact, manifest.CreatedAt, manifest.CompletedAt, manifest.JobID)
	if err != nil {
		return "", err
	}
	return "updated", tx.Commit()
}

type manifestJob struct {
	State, Filename, Spec, InputHash, ActualHash, Error, Artifact, CreatedAt, FinishedAt string
	Size, Received, Reserved                                                             int64
	ChunkSize                                                                            int64
	Chunks, Expected                                                                     int
}

func (j manifestJob) matchesManifest(manifest OutputManifest, expected int) bool {
	if j.Filename != manifest.OriginalFilename || j.Size != manifest.InputSize || j.Received != manifest.InputSize || j.Chunks != expected || j.Expected != expected || j.ChunkSize != manifest.ChunkSize || j.InputHash != manifest.RequestedInputSHA256 || j.ActualHash != manifest.InputSHA256 || j.Reserved != 0 || j.Error != "" || j.Artifact != manifest.Artifact {
		return false
	}
	var output Output
	if err := json.Unmarshal([]byte(j.Spec), &output); err != nil || !reflect.DeepEqual(output, manifest.Output) {
		return false
	}
	createdAt, err := parseDatabaseTime(j.CreatedAt)
	if err != nil {
		return false
	}
	manifestCreatedAt, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
	if err != nil || !createdAt.Equal(manifestCreatedAt) {
		return false
	}
	finishedAt, err := parseDatabaseTime(j.FinishedAt)
	if err != nil {
		return false
	}
	manifestFinishedAt, err := time.Parse(time.RFC3339Nano, manifest.CompletedAt)
	return err == nil && finishedAt.Equal(manifestFinishedAt)
}

func validJobID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
