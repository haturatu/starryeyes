package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateJobIsIdempotent(t *testing.T) {
	_, server := newAPITestServer(t)
	key := "resume-workflow-00000001"
	payload := []byte(`{"input":{"filename":"clip.mp4","size":1024},"output":{"preset":"web-1080p"}}`)
	first := createJobWithKey(t, server.URL, key, payload, http.StatusCreated)
	second := createJobWithKey(t, server.URL, key, payload, http.StatusCreated)
	if first.ID != second.ID {
		t.Fatalf("idempotent create IDs = %q and %q, want the same job", first.ID, second.ID)
	}

	conflict := []byte(`{"input":{"filename":"different.mp4","size":1024},"output":{"preset":"web-1080p"}}`)
	createJobWithKey(t, server.URL, key, conflict, http.StatusConflict)
}

func TestListVerifiedChunksSupportsResume(t *testing.T) {
	service, server := newAPITestServer(t)
	key := "resume-workflow-00000002"
	payload := []byte(`{"input":{"filename":"clip.mp4","size":4},"output":{"preset":"web-1080p"}}`)
	created := createJobWithKey(t, server.URL, key, payload, http.StatusCreated)
	job := waitForJobState(t, service, created.ID, admitted)
	data := []byte("test")
	digest := sha256.Sum256(data)
	request, err := http.NewRequest(http.MethodPut, server.URL+"/v1/jobs/"+job.ID+"/chunks/0", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = int64(len(data))
	request.Header.Set("X-Chunk-SHA256", hex.EncodeToString(digest[:]))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d, want 200", response.StatusCode)
	}

	response, err = http.Get(server.URL + "/v1/jobs/" + job.ID + "/chunks")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var chunks APIChunksResponse
	if err = json.NewDecoder(response.Body).Decode(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks.ChunkSize != job.ChunkSize || chunks.Expected != 1 || len(chunks.Chunks) != 1 || chunks.Chunks[0].Number != 0 || chunks.Chunks[0].Size != 4 || chunks.Chunks[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Errorf("chunks response = %#v, want uploaded chunk metadata", chunks)
	}
}

func TestCompleteIsIdempotentAfterCompletion(t *testing.T) {
	service, server := newAPITestServer(t)
	jobID := "48047daf0e72bd37dd856118a8eed24b"
	if _, err := service.db.Exec(`INSERT INTO jobs(id,state,filename,size,received,chunks,expected,chunk_size,spec,reserved,created_at,finished_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, jobID, completed, "clip.mp4", 4, 4, 1, 1, 4, `{}`, 0, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(server.URL+"/v1/jobs/"+jobID+"/complete", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate complete status = %d, want 202", response.StatusCode)
	}
	var state APIJobStateResponse
	if err = json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.State != completed {
		t.Errorf("duplicate complete state = %q, want COMPLETED", state.State)
	}
}

func TestExpireUploadReleasesCapacityAndSpool(t *testing.T) {
	service, _ := newAPITestServer(t)
	jobID := "58047daf0e72bd37dd856118a8eed24b"
	spoolDir := service.dir(jobID)
	if err := os.MkdirAll(spoolDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spoolDir, "input.part"), []byte("test"), 0640); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := service.db.Exec(`INSERT INTO jobs(id,state,filename,size,received,chunks,expected,chunk_size,spec,reserved,created_at,last_activity_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, jobID, uploading, "clip.mp4", 4, 4, 1, 1, 4, `{}`, 4, past, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`INSERT INTO reservations(job_id,input_bytes,output_bytes,safety_bytes,total) VALUES(?,?,?,?,?)`, jobID, 4, 0, 0, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`INSERT INTO chunks(job_id,number,bytes,sha256,state) VALUES(?,?,?,?,?)`, jobID, 0, 4, "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "VERIFIED"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`UPDATE capacity SET reserved=4 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := service.expireUploads(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	job, err := service.job(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != expired || job.Reserved != 0 || job.ExpiresAt.Valid {
		t.Errorf("expired job = %#v, want EXPIRED without reservation", job)
	}
	var capacity, chunks int64
	if err := service.db.QueryRow(`SELECT reserved FROM capacity WHERE id=1`).Scan(&capacity); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE job_id=?`, jobID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if capacity != 0 || chunks != 0 {
		t.Errorf("after expiry capacity=%d chunks=%d, want zero", capacity, chunks)
	}
	if _, err := os.Stat(spoolDir); !os.IsNotExist(err) {
		t.Errorf("expired spool still exists: %v", err)
	}
}

func TestRecoverRejectsInvalidResumableSpool(t *testing.T) {
	service, _ := newAPITestServer(t)
	jobID := "68047daf0e72bd37dd856118a8eed24b"
	spoolDir := service.dir(jobID)
	if err := os.MkdirAll(spoolDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spoolDir, "input.part"), []byte("short"), 0640); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := service.db.Exec(`INSERT INTO jobs(id,state,filename,size,expected,chunk_size,spec,reserved,created_at,last_activity_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, jobID, admitted, "clip.mp4", 8, 1, 8, `{}`, 8, now, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`INSERT INTO reservations(job_id,input_bytes,output_bytes,safety_bytes,total) VALUES(?,?,?,?,?)`, jobID, 8, 0, 0, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`UPDATE capacity SET reserved=8 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	service.reconcileResumableUploads()
	job, err := service.job(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != failed || job.Reserved != 0 {
		t.Errorf("reconciled job = %#v, want FAILED without reservation", job)
	}
	var capacity int64
	if err := service.db.QueryRow(`SELECT reserved FROM capacity WHERE id=1`).Scan(&capacity); err != nil || capacity != 0 {
		t.Errorf("capacity after reconcile = %d, %v; want 0", capacity, err)
	}
}

func createJobWithKey(t *testing.T, baseURL, key string, payload []byte, wantStatus int) APICreateJobResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/jobs", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("create status = %d, want %d", response.StatusCode, wantStatus)
	}
	var created APICreateJobResponse
	if wantStatus == http.StatusCreated {
		if err = json.NewDecoder(response.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
	}
	return created
}
