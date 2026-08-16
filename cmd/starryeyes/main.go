// Linux-only media ingest/worker daemon. It deliberately exposes no FFmpeg argv.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"log/slog"
	"mime"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	pending     = "PENDING"
	admitted    = "ADMITTED"
	uploading   = "UPLOADING"
	finalizing  = "FINALIZING"
	staged      = "STAGED"
	probing     = "PROBING"
	validated   = "VALIDATED"
	queued      = "QUEUED"
	starting    = "STARTING"
	transcoding = "TRANSCODING"
	completed   = "COMPLETED"
	failed      = "FAILED"
	expired     = "EXPIRED"
)

type Config struct {
	Data, Output, Listen, Cgroup    string
	VAAPIDevice                     string
	Capacity, Chunk                 int64
	Active                          int
	RequireCgroup, RequireLandlock  bool
	MaxWidth, MaxHeight, MaxStreams int
	MaxDuration                     float64
	UploadRetention                 time.Duration
}
type Server struct {
	db          *sql.DB
	cfg         Config
	log         *slog.Logger
	sem         chan struct{}
	scheduleMu  sync.Mutex
	admissionMu sync.Mutex
	// A handler taking both locks must acquire the lifecycle lock first.
	lifecycleLocks sync.Map
	chunkLocks     sync.Map
}
type Job struct {
	ID, State, Filename, Spec, InputHash, ActualHash string
	ProbeJSON                                        string
	Size, Received, Reserved, ChunkSize              int64
	Chunks, Expected                                 int
	Error, Artifact, ExpiresAt                       sql.NullString
	CreatedAt                                        string
}
type Probe struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []ProbeStream `json:"streams"`
}
type ProbeStream struct {
	CodecType    string            `json:"codec_type"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	CodecName    string            `json:"codec_name"`
	PixFmt       string            `json:"pix_fmt"`
	AvgFrameRate string            `json:"avg_frame_rate"`
	Rotation     int               `json:"rotation"`
	Tags         map[string]string `json:"tags"`
	SideDataList []struct {
		Rotation int `json:"rotation"`
	} `json:"side_data_list"`
}

func main() {
	logger, err := loggerFromEnv()
	if err != nil {
		logger = bootstrapLogger()
		logger.Error("startup failed", "component", "startup", "event", "service.startup_failed", "error", err)
		os.Exit(1)
	}
	if err := run(logger); err != nil {
		logger.Error("startup failed", "component", "startup", "event", "service.startup_failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	backfill := len(os.Args) == 2 && os.Args[1] == "backfill"
	if len(os.Args) > 1 && !backfill {
		return fmt.Errorf("usage: %s [backfill]", os.Args[0])
	}
	c, e := configFromEnv()
	if e != nil {
		return e
	}
	if backfill {
		return runBackfill(c, logger)
	}
	if c.Chunk < 1<<20 || c.Active < 1 {
		return errors.New("invalid config")
	}
	if c.RequireLandlock {
		for _, p := range []string{"/usr/local/bin/sandbox-exec", "/usr/local/bin/cgroup-exec"} {
			if _, e := exec.LookPath(p); e != nil {
				return fmt.Errorf("required Landlock component %s: %w", p, e)
			}
		}
		if e := exec.Command("/usr/local/bin/sandbox-exec", "--check").Run(); e != nil {
			return fmt.Errorf("Landlock ABI >=4 is required: %w", e)
		}
	}
	for _, d := range []string{c.Data, filepath.Join(c.Data, "spool"), c.Output} {
		if e := os.MkdirAll(d, 0750); e != nil {
			return e
		}
	}
	db, e := sql.Open("sqlite", filepath.Join(c.Data, "jobs.sqlite?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"))
	if e != nil {
		return e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &Server{db: db, cfg: c, log: logger, sem: make(chan struct{}, c.Active)}
	if e = s.schema(); e != nil {
		return e
	}
	if e = s.recover(); e != nil {
		return e
	}
	go s.runUploadGC()
	h := http.MaxBytesHandler(newRouter(s), c.Chunk+8192)
	srv := &http.Server{Addr: c.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute, WriteTimeout: 30 * time.Second}
	s.component("startup").Info("service listening", "event", "service.started", "address", c.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
func (s *Server) schema() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS capacity(id INTEGER PRIMARY KEY CHECK(id=1), total INTEGER NOT NULL,reserved INTEGER NOT NULL DEFAULT 0); INSERT OR IGNORE INTO capacity(id,total,reserved) VALUES(1,0,0); UPDATE capacity SET total=? WHERE id=1 AND reserved=0; CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,client_request_id TEXT UNIQUE,request_hash TEXT,state TEXT NOT NULL,filename TEXT NOT NULL,size INTEGER NOT NULL,received INTEGER NOT NULL DEFAULT 0,chunks INTEGER NOT NULL DEFAULT 0,expected INTEGER NOT NULL,chunk_size INTEGER NOT NULL,spec TEXT NOT NULL,input_hash TEXT,actual_hash TEXT,reserved INTEGER NOT NULL,error TEXT,artifact TEXT,probe_json TEXT,created_at TEXT NOT NULL,last_activity_at TEXT,expires_at TEXT,started_at TEXT,finished_at TEXT); CREATE TABLE IF NOT EXISTS reservations(job_id TEXT PRIMARY KEY,input_bytes INTEGER NOT NULL,output_bytes INTEGER NOT NULL,safety_bytes INTEGER NOT NULL,total INTEGER NOT NULL,FOREIGN KEY(job_id) REFERENCES jobs(id)); CREATE TABLE IF NOT EXISTS chunks(job_id TEXT NOT NULL,number INTEGER NOT NULL,bytes INTEGER NOT NULL,sha256 TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(job_id,number),FOREIGN KEY(job_id) REFERENCES jobs(id) ON DELETE CASCADE); CREATE INDEX IF NOT EXISTS jobs_state ON jobs(state); CREATE INDEX IF NOT EXISTS jobs_upload_expiry ON jobs(expires_at) WHERE state IN ('PENDING','ADMITTED','UPLOADING');`, s.cfg.Capacity)
	return e
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	out(w, http.StatusOK, APIHealthResponse{OK: true})
}
func (s *Server) cap(w http.ResponseWriter, r *http.Request) {
	out(w, http.StatusOK, APICapabilitiesResponse{
		Containers:    []string{"mp4", "mkv", "webm"},
		VideoCodecs:   []string{"h264", "hevc", "av1", "vp9"},
		VideoEncoders: []string{"auto", "software", "vaapi", "nvenc"},
		AudioCodecs:   []string{"aac", "opus", "flac"},
		Presets:       []string{"web-1080p", "archive-av1"},
		Limits: APILimits{
			ChunkSize:  s.cfg.Chunk,
			MaxWidth:   s.cfg.MaxWidth,
			MaxHeight:  s.cfg.MaxHeight,
			MaxStreams: s.cfg.MaxStreams,
		},
	})
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if !validIdempotencyKey(key) {
		bad(w, http.StatusBadRequest, "valid Idempotency-Key header required")
		return
	}
	var q Request
	if e := json.NewDecoder(r.Body).Decode(&q); e != nil {
		bad(w, 400, "invalid JSON")
		return
	}
	sp, e := normalize(q, s.cfg)
	if e != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(e, errUnsupportedEncoderCodec) {
			status = http.StatusBadRequest
		}
		bad(w, status, e.Error())
		return
	}
	q.Input.SHA256 = strings.ToLower(q.Input.SHA256)
	spec, _ := json.Marshal(sp)
	identity, _ := json.Marshal(struct {
		Input  Input  `json:"input"`
		Output Output `json:"output"`
	}{Input: q.Input, Output: sp})
	requestHash := sha256.Sum256(identity)
	now := time.Now().UTC()
	expiresAt := uploadExpiry(now, s.cfg.UploadRetention)
	tx, e := s.db.Begin()
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	defer tx.Rollback()
	var existing Job
	var existingHash string
	e = tx.QueryRow(`SELECT id,state,filename,size,received,chunks,expected,chunk_size,spec,COALESCE(input_hash,''),COALESCE(actual_hash,''),reserved,error,artifact,created_at,expires_at,COALESCE(request_hash,'') FROM jobs WHERE client_request_id=?`, key).Scan(&existing.ID, &existing.State, &existing.Filename, &existing.Size, &existing.Received, &existing.Chunks, &existing.Expected, &existing.ChunkSize, &existing.Spec, &existing.InputHash, &existing.ActualHash, &existing.Reserved, &existing.Error, &existing.Artifact, &existing.CreatedAt, &existing.ExpiresAt, &existingHash)
	if e == nil {
		if existingHash != hex.EncodeToString(requestHash[:]) {
			bad(w, http.StatusConflict, "Idempotency-Key already used with a different request")
			return
		}
		// An idempotent replay discovers the existing workflow but is not
		// upload activity, so it deliberately does not extend retention.
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			internalError(w, r, http.StatusInternalServerError, e)
			return
		}
		s.component("admission").Debug("idempotent job replay", "event", "job.replayed", "job_id", existing.ID)
		out(w, http.StatusCreated, s.createJobResponse(existing))
		return
	}
	if e != sql.ErrNoRows {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	jobID := id()
	expected := int((q.Input.Size + s.cfg.Chunk - 1) / s.cfg.Chunk)
	_, e = tx.Exec(`INSERT INTO jobs(id,client_request_id,request_hash,state,filename,size,expected,chunk_size,spec,input_hash,reserved,created_at,last_activity_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, jobID, key, hex.EncodeToString(requestHash[:]), pending, q.Input.Filename, q.Input.Size, expected, s.cfg.Chunk, string(spec), q.Input.SHA256, 0, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiresAt)
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	s.component("admission").Info("job created", "event", "job.created", "job_id", jobID, "size_bytes", q.Input.Size)
	go s.admitPending()
	out(w, http.StatusCreated, APICreateJobResponse{
		ID:               jobID,
		State:            pending,
		ReservationBytes: 0,
		ExpiresAt:        &expiresAt,
	})
}

func (s *Server) listChunks(w http.ResponseWriter, r *http.Request) {
	j, e := s.job(r.PathValue("id"))
	if e == sql.ErrNoRows {
		bad(w, http.StatusNotFound, "job not found")
		return
	}
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	rows, e := s.db.Query(`SELECT number,bytes,sha256 FROM chunks WHERE job_id=? AND state='VERIFIED' ORDER BY number`, j.ID)
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	defer rows.Close()
	chunks := make([]APIVerifiedChunk, 0, j.Chunks)
	for rows.Next() {
		var chunk APIVerifiedChunk
		if e = rows.Scan(&chunk.Number, &chunk.Size, &chunk.SHA256); e != nil {
			internalError(w, r, http.StatusInternalServerError, e)
			return
		}
		chunks = append(chunks, chunk)
	}
	if e = rows.Err(); e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	out(w, http.StatusOK, APIChunksResponse{ChunkSize: j.ChunkSize, Expected: j.Expected, Chunks: chunks})
}
func (s *Server) chunk(w http.ResponseWriter, r *http.Request) {
	jid := r.PathValue("id")
	n, e := strconv.Atoi(r.PathValue("chunk"))
	if e != nil || n < 0 {
		bad(w, 400, "invalid chunk")
		return
	}
	lifecycle := s.lifecycleLock(jid)
	lifecycle.RLock()
	defer lifecycle.RUnlock()
	chunk := s.chunkLock(jid, n)
	chunk.Lock()
	defer chunk.Unlock()
	j, e := s.job(jid)
	if e == sql.ErrNoRows {
		bad(w, 404, "job not found")
		return
	}
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	if n >= j.Expected {
		bad(w, 416, "chunk outside input")
		return
	}
	want := chunkBytes(j.Size, j.ChunkSize, n)
	sum := strings.ToLower(r.Header.Get("X-Chunk-SHA256"))
	if r.ContentLength != want || len(sum) != 64 {
		bad(w, 400, "exact Content-Length and X-Chunk-SHA256 required")
		return
	}
	var have string
	e = s.db.QueryRow(`SELECT sha256 FROM chunks WHERE job_id=? AND number=? AND state='VERIFIED'`, jid, n).Scan(&have)
	if e == nil {
		if have == sum {
			now := time.Now().UTC()
			if _, updateErr := s.db.Exec(`UPDATE jobs SET last_activity_at=?,expires_at=? WHERE id=? AND state IN (?,?)`, now.Format(time.RFC3339Nano), uploadExpiry(now, s.cfg.UploadRetention), jid, admitted, uploading); updateErr != nil {
				internalError(w, r, http.StatusInternalServerError, updateErr)
				return
			}
			s.component("upload").Debug("chunk already verified", "event", "upload.chunk.already_present", "job_id", jid, "chunk", n)
			out(w, http.StatusOK, APIChunkResponse{Chunk: n, Status: "already_present"})
			return
		}
		bad(w, 409, "chunk already present with different checksum")
		return
	}
	if e != sql.ErrNoRows {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	if j.State != admitted && j.State != uploading {
		bad(w, 409, "job is not accepting upload")
		return
	}
	f, e := os.OpenFile(filepath.Join(s.dir(jid), "input.part"), os.O_RDWR, 0)
	if e != nil {
		bad(w, 409, "input is finalizing")
		return
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var got int64
	off := int64(n) * j.ChunkSize
	for {
		z, er := r.Body.Read(buf)
		if z > 0 {
			if _, x := unix.Pwrite(int(f.Fd()), buf[:z], off+got); x != nil {
				internalError(w, r, http.StatusInsufficientStorage, x)
				return
			}
			h.Write(buf[:z])
			got += int64(z)
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			bad(w, 400, er.Error())
			return
		}
	}
	if got != want || hex.EncodeToString(h.Sum(nil)) != sum {
		bad(w, 422, "chunk checksum mismatch")
		return
	}
	if e = unix.Fdatasync(int(f.Fd())); e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	tx, e := s.db.Begin()
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	defer tx.Rollback()
	_, e = tx.Exec(`INSERT INTO chunks(job_id,number,bytes,sha256,state) VALUES(?,?,?,?, 'VERIFIED')`, jid, n, want, sum)
	var updated int64
	if e == nil {
		var x sql.Result
		now := time.Now().UTC()
		x, e = tx.Exec(`UPDATE jobs SET state=?,received=received+?,chunks=chunks+1,last_activity_at=?,expires_at=? WHERE id=? AND state IN (?,?)`, uploading, want, now.Format(time.RFC3339Nano), uploadExpiry(now, s.cfg.UploadRetention), jid, admitted, uploading)
		if e == nil {
			updated, _ = x.RowsAffected()
		}
	}
	if e == nil && updated != 1 {
		bad(w, 409, "job is not accepting upload")
		return
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	s.component("upload").Debug("chunk accepted", "event", "upload.chunk.accepted", "job_id", jid, "chunk", n, "bytes", want)
	out(w, http.StatusOK, APIChunkResponse{Chunk: n, Bytes: want, Status: "verified"})
}
func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	jid := r.PathValue("id")
	mu := s.lifecycleLock(jid)
	mu.Lock()
	defer mu.Unlock()
	tx, e := s.db.Begin()
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	x, e := tx.Exec(`UPDATE jobs SET state=?,last_activity_at=?,expires_at=NULL WHERE id=? AND state IN (?,?) AND received=size AND chunks=expected`, finalizing, time.Now().UTC().Format(time.RFC3339Nano), jid, admitted, uploading)
	if e == nil {
		e = tx.Commit()
	} else {
		tx.Rollback()
	}
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	n, _ := x.RowsAffected()
	if n != 1 {
		j, z := s.job(jid)
		if z == nil && processingOrCompletedState(j.State) {
			out(w, http.StatusAccepted, APIJobStateResponse{ID: jid, State: j.State})
			return
		}
		bad(w, 409, "upload incomplete or job not completable")
		return
	}
	go s.finalize(jid)
	s.component("upload").Info("upload completed", "event", "upload.completed", "job_id", jid)
	out(w, http.StatusAccepted, APIJobStateResponse{ID: jid, State: finalizing})
}
func (s *Server) finalize(jid string) {
	mu := s.lifecycleLock(jid)
	mu.Lock()
	defer mu.Unlock()
	part, in := filepath.Join(s.dir(jid), "input.part"), filepath.Join(s.dir(jid), "input")
	if _, e := os.Stat(in); e != nil {
		if e = os.Rename(part, in); e != nil {
			s.fail(jid, e)
			return
		}
	}
	j, e := s.job(jid)
	if e != nil {
		return
	}
	h, e := hashFile(in)
	if e != nil {
		s.fail(jid, e)
		return
	}
	if j.InputHash != "" && j.InputHash != h {
		s.fail(jid, errors.New("whole input SHA-256 mismatch"))
		return
	}
	if _, e = s.db.Exec(`UPDATE jobs SET state=?,actual_hash=? WHERE id=? AND state=?`, staged, h, jid, finalizing); e != nil {
		s.fail(jid, e)
		return
	}
	s.component("upload").Info("upload finalized", "event", "upload.finalized", "job_id", jid)
	s.probeAndQueue(jid, in)
}
func (s *Server) probeAndQueue(jid, in string) {
	if _, e := s.db.Exec(`UPDATE jobs SET state=? WHERE id=? AND state=?`, probing, jid, staged); e != nil {
		s.component("db").Error("update job state", "event", "job.state_update_failed", "job_id", jid, "state", probing, "error", e)
		return
	}
	probeLog := s.component("ffprobe").With("job_id", jid)
	probeLog.Info("probe started", "event", "probe.started")
	started := time.Now()
	var stderr boundedBuffer
	stderr.limit = maxCapturedProcessStderr
	cmd := s.probeCmd(in)
	cmd.Stderr = &stderr
	b, e := cmd.Output()
	if e != nil {
		probeLog.Error("probe failed", "event", "probe.failed", "exit_code", commandExitCode(e), "stderr", stderr.String(), "stderr_truncated", stderr.truncated, "error", e)
		s.fail(jid, fmt.Errorf("ffprobe: %w: %s", e, stderr.String()))
		return
	}
	var p Probe
	if e = json.Unmarshal(b, &p); e != nil {
		probeLog.Error("probe output invalid", "event", "probe.failed", "stderr", stderr.String(), "stderr_truncated", stderr.truncated, "error", e)
		s.fail(jid, e)
		return
	}
	if e = s.validateProbe(p); e != nil {
		probeLog.Warn("probe validation failed", "event", "probe.rejected", "error", e)
		s.fail(jid, e)
		return
	}
	probeLog.Info("probe completed", "event", "probe.completed", "duration_ms", time.Since(started).Milliseconds(), "stream_count", len(p.Streams))
	pb, _ := json.Marshal(p)
	if _, e = s.db.Exec(`UPDATE jobs SET state=?,probe_json=? WHERE id=? AND state=?`, validated, string(pb), jid, probing); e != nil {
		s.component("db").Error("store probe result", "event", "probe.persist_failed", "job_id", jid, "error", e)
		return
	}
	if _, e = s.db.Exec(`UPDATE jobs SET state=? WHERE id=? AND state=?`, queued, jid, validated); e != nil {
		s.component("db").Error("queue job", "event", "job.queue_failed", "job_id", jid, "error", e)
		return
	}
	s.component("scheduler").Info("job queued", "event", "job.queued", "job_id", jid)
	s.schedule()
}
func (s *Server) validateProbe(p Probe) error {
	if len(p.Streams) == 0 || len(p.Streams) > s.cfg.MaxStreams {
		return errors.New("invalid stream count")
	}
	d, e := strconv.ParseFloat(p.Format.Duration, 64)
	if e != nil || d <= 0 || d > s.cfg.MaxDuration {
		return errors.New("invalid duration")
	}
	for _, v := range p.Streams {
		if v.Width > s.cfg.MaxWidth || v.Height > s.cfg.MaxHeight {
			return errors.New("input resolution exceeds limit")
		}
	}
	return nil
}
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	j, e := s.job(r.PathValue("id"))
	if e == sql.ErrNoRows {
		bad(w, 404, "job not found")
		return
	}
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	out(w, http.StatusOK, APIJobResponse{
		ID:               j.ID,
		State:            j.State,
		Filename:         j.Filename,
		Size:             j.Size,
		BytesReceived:    j.Received,
		ChunksReceived:   j.Chunks,
		ChunksExpected:   j.Expected,
		ReservationBytes: j.Reserved,
		Upload:           s.uploadInstructions(j),
		InputSHA256:      j.ActualHash,
		Error:            nullable(j.Error),
		OutputURL:        artifactURL(j),
		ExpiresAt:        nullable(j.ExpiresAt),
	})
}
func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	j, e := s.job(r.PathValue("id"))
	if e == sql.ErrNoRows || (e == nil && (j.State != completed || !j.Artifact.Valid)) {
		bad(w, 404, "output not available")
		return
	}
	if e != nil {
		internalError(w, r, http.StatusInternalServerError, e)
		return
	}
	name := filepath.Base(j.Artifact.String)
	if name != j.Artifact.String {
		internalError(w, r, http.StatusInternalServerError, errors.New("invalid artifact key"))
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": downloadFilename(j),
	}))
	http.ServeFile(w, r, filepath.Join(s.outputDir(j.ID), name))
}

// admitPending reserves spool capacity and creates the input file for queued
// jobs in FIFO order. A job remains PENDING until its preallocated input file
// exists, so clients never receive upload instructions prematurely.
func (s *Server) admitPending() {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()

	for {
		j, e := s.oldestPendingJob()
		if e == sql.ErrNoRows {
			return
		}
		if e != nil {
			s.component("admission").Error("find pending job", "event", "admission.scan_failed", "error", e)
			return
		}
		if j.Reserved == 0 {
			if e := s.reservePending(j); e != nil {
				if errors.Is(e, errCapacityUnavailable) {
					return
				}
				s.fail(j.ID, e)
				continue
			}
			j.Reserved = j.Size
		}
		if e := s.prepareInput(j); e != nil {
			s.fail(j.ID, fmt.Errorf("spool preallocation: %w", e))
			continue
		}
		now := time.Now().UTC()
		result, e := s.db.Exec(`UPDATE jobs SET state=?,last_activity_at=?,expires_at=? WHERE id=? AND state=? AND reserved=?`, admitted, now.Format(time.RFC3339Nano), uploadExpiry(now, s.cfg.UploadRetention), j.ID, pending, j.Reserved)
		if e != nil {
			s.fail(j.ID, e)
			continue
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			continue
		}
		s.component("admission").Info("job admitted for upload", "event", "job.admitted", "job_id", j.ID, "reserved_bytes", j.Reserved)
	}
}

var errCapacityUnavailable = errors.New("spool capacity temporarily unavailable")

func (s *Server) oldestPendingJob() (Job, error) {
	var j Job
	e := s.db.QueryRow(`SELECT id,size,spec,reserved FROM jobs WHERE state=? AND julianday(expires_at)>julianday(?) ORDER BY created_at,id LIMIT 1`, pending, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&j.ID, &j.Size, &j.Spec, &j.Reserved)
	return j, e
}

func (s *Server) reservePending(j Job) error {
	var o Output
	if e := json.Unmarshal([]byte(j.Spec), &o); e != nil {
		return fmt.Errorf("decode output specification: %w", e)
	}
	outEstimate, safety, reserved := reservation(j.Size, o)
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	result, e := tx.Exec(`UPDATE capacity SET reserved=reserved+? WHERE id=1 AND reserved+?<=total`, reserved, reserved)
	if e != nil {
		return e
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errCapacityUnavailable
	}
	result, e = tx.Exec(`UPDATE jobs SET reserved=? WHERE id=? AND state=? AND reserved=0`, reserved, j.ID, pending)
	if e != nil {
		return e
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("pending job changed during admission")
	}
	if _, e = tx.Exec(`INSERT INTO reservations(job_id,input_bytes,output_bytes,safety_bytes,total) VALUES(?,?,?,?,?)`, j.ID, j.Size, outEstimate, safety, reserved); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Server) prepareInput(j Job) error {
	dir := s.dir(j.ID)
	if e := os.MkdirAll(dir, 0750); e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(dir, "input.part"), os.O_CREATE|os.O_RDWR, 0640)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if info.Size() != j.Size {
		if e = f.Truncate(0); e != nil {
			return e
		}
		if e = unix.Fallocate(int(f.Fd()), 0, 0, j.Size); e != nil {
			return e
		}
	}
	return nil
}

func (s *Server) job(id string) (Job, error) {
	var j Job
	e := s.db.QueryRow(`SELECT id,state,filename,size,received,chunks,expected,chunk_size,spec,COALESCE(probe_json,''),COALESCE(input_hash,''),COALESCE(actual_hash,''),reserved,error,artifact,created_at,expires_at FROM jobs WHERE id=?`, id).Scan(&j.ID, &j.State, &j.Filename, &j.Size, &j.Received, &j.Chunks, &j.Expected, &j.ChunkSize, &j.Spec, &j.ProbeJSON, &j.InputHash, &j.ActualHash, &j.Reserved, &j.Error, &j.Artifact, &j.CreatedAt, &j.ExpiresAt)
	return j, e
}
func (s *Server) recover() error {
	if e := s.expireUploads(time.Now().UTC()); e != nil {
		return e
	}
	if e := s.reconcileResumableUploads(); e != nil {
		return e
	}
	if _, e := s.db.Exec(`UPDATE jobs SET state=? WHERE state IN (?,?)`, queued, starting, transcoding); e != nil {
		return e
	}
	rows, e := s.db.Query(`SELECT id FROM jobs WHERE state=?`, finalizing)
	if e != nil {
		return e
	}
	var finalizingJobs []string
	for rows.Next() {
		var jobID string
		if e = rows.Scan(&jobID); e != nil {
			rows.Close()
			return e
		}
		finalizingJobs = append(finalizingJobs, jobID)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, jobID := range finalizingJobs {
		go s.finalize(jobID)
	}
	go s.admitPending()
	s.schedule()
	return nil
}

func (s *Server) runUploadGC() {
	interval := s.cfg.UploadRetention / 24
	if interval < time.Minute {
		interval = time.Minute
	}
	if interval > time.Hour {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for now := range ticker.C {
		if e := s.expireUploads(now.UTC()); e != nil {
			s.component("gc").Error("expire abandoned uploads", "event", "gc.expire_failed", "error", e)
		}
	}
}

func (s *Server) expireUploads(now time.Time) error {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	rows, e := s.db.Query(`SELECT id FROM jobs WHERE state IN (?,?,?) AND expires_at IS NOT NULL AND julianday(expires_at)<=julianday(?) ORDER BY expires_at`, pending, admitted, uploading, now.Format(time.RFC3339Nano))
	if e != nil {
		return e
	}
	var candidates []string
	for rows.Next() {
		var jobID string
		if e = rows.Scan(&jobID); e != nil {
			rows.Close()
			return e
		}
		candidates = append(candidates, jobID)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, jobID := range candidates {
		if e = s.expireUpload(jobID, now); e != nil {
			return e
		}
	}
	if len(candidates) > 0 {
		go s.admitPending()
	}
	return nil
}

func (s *Server) expireUpload(jobID string, now time.Time) error {
	mu := s.lifecycleLock(jobID)
	mu.Lock()
	defer mu.Unlock()
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var reserved int64
	if e = tx.QueryRow(`SELECT reserved FROM jobs WHERE id=? AND state IN (?,?,?) AND julianday(expires_at)<=julianday(?)`, jobID, pending, admitted, uploading, now.Format(time.RFC3339Nano)).Scan(&reserved); e == sql.ErrNoRows {
		return nil
	} else if e != nil {
		return e
	}
	result, e := tx.Exec(`UPDATE jobs SET state=?,reserved=0,error=?,expires_at=NULL,finished_at=? WHERE id=? AND state IN (?,?,?) AND julianday(expires_at)<=julianday(?)`, expired, "upload expired after inactivity", now.Format(time.RFC3339Nano), jobID, pending, admitted, uploading, now.Format(time.RFC3339Nano))
	if e != nil {
		return e
	}
	changed, e := result.RowsAffected()
	if e != nil || changed == 0 {
		return e
	}
	if _, e = tx.Exec(`DELETE FROM chunks WHERE job_id=?`, jobID); e != nil {
		return e
	}
	if _, e = tx.Exec(`DELETE FROM reservations WHERE job_id=?`, jobID); e != nil {
		return e
	}
	if reserved > 0 {
		if _, e = tx.Exec(`UPDATE capacity SET reserved=MAX(reserved-?,0) WHERE id=1`, reserved); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if e = os.RemoveAll(s.dir(jobID)); e != nil {
		s.component("storage").Error("remove expired upload spool", "event", "storage.cleanup_failed", "job_id", jobID, "error", e)
	}
	s.component("gc").Info("upload expired", "event", "upload.expired", "job_id", jobID)
	return nil
}

func (s *Server) reconcileResumableUploads() error {
	rows, e := s.db.Query(`SELECT id,size FROM jobs WHERE state IN (?,?) ORDER BY created_at`, admitted, uploading)
	if e != nil {
		return e
	}
	type resumableUpload struct {
		id   string
		size int64
	}
	var uploads []resumableUpload
	for rows.Next() {
		var upload resumableUpload
		if e = rows.Scan(&upload.id, &upload.size); e != nil {
			rows.Close()
			return e
		}
		uploads = append(uploads, upload)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return e
	}
	rows.Close()
	for _, upload := range uploads {
		info, statErr := os.Stat(filepath.Join(s.dir(upload.id), "input.part"))
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != upload.size {
			detail := "spool input is missing or has an unexpected size"
			if statErr != nil {
				detail = fmt.Sprintf("spool input unavailable: %v", statErr)
			}
			if e = s.failUpload(upload.id, errors.New(detail)); e != nil {
				return e
			}
		}
	}
	return nil
}

func (s *Server) failUpload(jobID string, cause error) error {
	mu := s.lifecycleLock(jobID)
	mu.Lock()
	defer mu.Unlock()
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var reserved int64
	if e = tx.QueryRow(`SELECT reserved FROM jobs WHERE id=? AND state IN (?,?)`, jobID, admitted, uploading).Scan(&reserved); e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE jobs SET state=?,reserved=0,error=?,expires_at=NULL,finished_at=? WHERE id=?`, failed, cause.Error(), time.Now().UTC().Format(time.RFC3339Nano), jobID); e != nil {
		return e
	}
	if _, e = tx.Exec(`DELETE FROM chunks WHERE job_id=?`, jobID); e != nil {
		return e
	}
	if _, e = tx.Exec(`DELETE FROM reservations WHERE job_id=?`, jobID); e != nil {
		return e
	}
	if reserved > 0 {
		if _, e = tx.Exec(`UPDATE capacity SET reserved=MAX(reserved-?,0) WHERE id=1`, reserved); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if e = os.RemoveAll(s.dir(jobID)); e != nil {
		s.component("storage").Error("remove invalid resumable upload spool", "event", "storage.cleanup_failed", "job_id", jobID, "error", e)
	}
	s.component("upload").Error("resumable upload failed integrity check", "event", "upload.integrity_failed", "job_id", jobID, "error", cause)
	return nil
}
func (s *Server) schedule() {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	s.component("scheduler").Debug("scheduler scan", "event", "scheduler.scan", "active", len(s.sem), "capacity", cap(s.sem))
	for len(s.sem) < cap(s.sem) {
		var jid string
		if e := s.db.QueryRow(`SELECT id FROM jobs WHERE state=? ORDER BY created_at LIMIT 1`, queued).Scan(&jid); e != nil {
			return
		}
		x, e := s.db.Exec(`UPDATE jobs SET state=?,started_at=? WHERE id=? AND state=?`, starting, time.Now().UTC(), jid, queued)
		if e != nil {
			return
		}
		n, _ := x.RowsAffected()
		if n == 0 {
			continue
		}
		s.sem <- struct{}{}
		go func() { defer func() { <-s.sem; s.schedule() }(); s.run(jid) }()
	}
}
func (s *Server) run(jid string) {
	j, e := s.job(jid)
	if e != nil {
		s.component("db").Error("load queued job", "event", "job.load_failed", "job_id", jid, "error", e)
		return
	}
	cg, e := s.makeCgroup(jid)
	if e != nil {
		s.fail(jid, e)
		return
	}
	defer cg.cleanup()
	if _, e = s.db.Exec(`UPDATE jobs SET state=? WHERE id=?`, transcoding, jid); e != nil {
		s.component("db").Error("update job state", "event", "job.state_update_failed", "job_id", jid, "state", transcoding, "error", e)
		return
	}
	workerLog := s.component("worker").With("job_id", jid)
	workerLog.Info("transcode started", "event", "transcode.started")
	started := time.Now()
	var o Output
	if e = json.Unmarshal([]byte(j.Spec), &o); e != nil {
		s.fail(jid, e)
		return
	}
	dir := s.outputDir(jid)
	if e = os.MkdirAll(dir, 0750); e != nil {
		s.fail(jid, e)
		return
	}
	artifact := "output." + o.Container
	var plan *AutoCompressionPlan
	if o.Video.Quality.Mode == "auto" {
		plan, e = autoPlanForJob(j, o)
		if e != nil {
			s.fail(jid, fmt.Errorf("auto compression plan: %w", e))
			return
		}
	}
	var encoders []videoEncoder
	if plan != nil && plan.Copy {
		encoders = []videoEncoder{{mode: "copy", name: "copy"}}
	} else {
		encoders, e = s.videoEncoders(o.Video)
		if e != nil {
			s.fail(jid, e)
			return
		}
	}
	var transcodeErr error
	var lastEncoder videoEncoder
	var lastExitCode int
	var lastStderr string
	var lastStderrTruncated bool
	for index, selected := range encoders {
		lastEncoder = selected
		ffmpegLog := s.component("ffmpeg").With("job_id", jid, "encoder", selected.name, "encoder_type", selected.mode)
		ffmpegLog.Info("encoder selected", "event", "encoder.selected")
		cmd, e := s.ffmpegCmdWithPlan(cg, j, o, artifact, selected, plan)
		if e != nil {
			s.fail(jid, fmt.Errorf("build ffmpeg command: %w", e))
			return
		}
		var stderr boundedBuffer
		stderr.limit = maxCapturedProcessStderr
		cmd.Stderr = &stderr
		e = cmd.Run()
		if e == nil {
			transcodeErr = nil
			break
		}
		lastExitCode = commandExitCode(e)
		lastStderr = stderr.String()
		lastStderrTruncated = stderr.truncated
		transcodeErr = fmt.Errorf("%s: %w: %s", selected.name, e, truncate(stderr.String(), 1000))
		if !selected.hardware || index+1 >= len(encoders) {
			break
		}
		ffmpegLog.Warn("hardware encoder failed; trying fallback", "event", "encoder.fallback", "fallback_encoder", encoders[index+1].name, "exit_code", lastExitCode, "stderr", lastStderr, "stderr_truncated", lastStderrTruncated, "error", transcodeErr)
		if removeErr := os.Remove(filepath.Join(dir, artifact)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.fail(jid, fmt.Errorf("remove failed hardware output: %w", removeErr))
			return
		}
	}
	if transcodeErr != nil {
		s.component("ffmpeg").With("job_id", jid, "encoder", lastEncoder.name, "encoder_type", lastEncoder.mode).Error("transcode failed", "event", "transcode.failed", "exit_code", lastExitCode, "stderr", lastStderr, "stderr_truncated", lastStderrTruncated, "error", transcodeErr)
		s.fail(jid, fmt.Errorf("transcode: %w", transcodeErr))
		return
	}
	completedAt := time.Now().UTC()
	if e := s.writeOutputManifest(j, o, artifact, completedAt); e != nil {
		s.fail(jid, fmt.Errorf("write output manifest: %w", e))
		return
	}
	if _, e := s.db.Exec(`UPDATE jobs SET state=?,artifact=?,finished_at=? WHERE id=?`, completed, artifact, completedAt, jid); e != nil {
		s.fail(jid, fmt.Errorf("record completed output: %w", e))
		return
	}
	s.release(j)
	workerLog.Info("transcode completed", "event", "transcode.completed", "encoder", lastEncoder.name, "duration_ms", time.Since(started).Milliseconds())
	workerLog.Info("job completed", "event", "job.completed", "encoder", lastEncoder.name, "duration_ms", time.Since(started).Milliseconds())
}
func (s *Server) fail(jid string, e error) {
	j, x := s.job(jid)
	if x == nil {
		s.release(j)
	}
	s.db.Exec(`UPDATE jobs SET state=?,error=?,finished_at=? WHERE id=? AND state NOT IN (?,?)`, failed, e.Error(), time.Now().UTC(), jid, completed, failed)
	s.component("worker").Error("job failed", "event", "job.failed", "job_id", jid, "error", e)
}
func (s *Server) release(j Job) {
	if j.Reserved > 0 {
		x, e := s.db.Exec(`UPDATE jobs SET reserved=0 WHERE id=? AND reserved=?`, j.ID, j.Reserved)
		if e != nil {
			return
		}
		n, _ := x.RowsAffected()
		if n == 1 {
			s.db.Exec(`UPDATE capacity SET reserved=MAX(reserved-?,0) WHERE id=1`, j.Reserved)
			s.db.Exec(`DELETE FROM reservations WHERE job_id=?`, j.ID)
			go s.admitPending()
		}
	}
}
func (s *Server) probeCmd(in string) *exec.Cmd {
	return s.sandbox(nil, in, "", nil, []string{"/usr/bin/ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe", "-show_format", "-show_streams", "-of", "json", in})
}
func (s *Server) ffmpegCmd(c cgroup, j Job, o Output, artifact string, selected videoEncoder) (*exec.Cmd, error) {
	var plan *AutoCompressionPlan
	if o.Video.Quality.Mode == "auto" {
		plan, _ = autoPlanForJob(j, o)
		if plan == nil {
			plan = fallbackAutoCompressionPlan(o)
		}
	}
	return s.ffmpegCmdWithPlan(c, j, o, artifact, selected, plan)
}
func (s *Server) ffmpegCmdWithPlan(c cgroup, j Job, o Output, artifact string, selected videoEncoder, plan *AutoCompressionPlan) (*exec.Cmd, error) {
	quality := 0
	if plan == nil {
		var err error
		quality, err = crf(o.Video)
		if err != nil {
			return nil, err
		}
	}
	audioCodec := ""
	if plan == nil || !plan.Copy {
		var err error
		audioCodec, err = audioEncoder(o.Audio.Codec)
		if err != nil {
			return nil, err
		}
	}
	in := filepath.Join(s.dir(j.ID), "input")
	outDir := s.outputDir(j.ID)
	a := []string{"/usr/bin/ffmpeg", "-y", "-nostdin", "-hide_banner", "-v", "error", "-protocol_whitelist", "file,pipe"}
	if selected.mode == "vaapi" {
		a = append(a, "-vaapi_device", selected.devices[0])
	}
	audioMap := "0:a?"
	if plan != nil {
		// Automatic compression has a total bitrate budget. Keeping only the
		// first audio stream makes that budget predictable for multi-track input.
		audioMap = "0:a:0?"
	}
	a = append(a, "-i", in, "-map", "0:v:0?", "-map", audioMap, "-c:v", selected.name)
	resolution := o.Video.Resolution
	if plan != nil {
		resolution = plan.Resolution
	} else if resolution.Mode == "auto" {
		resolution = Resolution{Mode: "source"}
	}
	filter := ""
	if plan == nil || !plan.Copy {
		filter = scale(resolution)
	}
	if selected.mode == "vaapi" {
		if filter != "" {
			filter += ","
		}
		filter += "format=nv12,hwupload"
	}
	if filter != "" {
		a = append(a, "-vf", filter)
	}
	if plan != nil && plan.Copy {
		a = append(a, "-c:v", "copy")
	} else if plan != nil {
		if selected.mode == "vaapi" {
			a = append(a, "-rc_mode", "VBR")
		} else if selected.mode == "nvenc" {
			a = append(a, "-rc", "vbr")
		}
		a = append(a, "-b:v", bitrateArg(plan.VideoBitrate), "-maxrate", bitrateArg(plan.VideoMaxrate), "-bufsize", bitrateArg(plan.VideoMaxrate*2))
		if selected.mode != "vaapi" {
			a = append(a, "-pix_fmt", "yuv420p")
		}
	} else {
		switch selected.mode {
		case "vaapi":
			a = append(a, "-qp", strconv.Itoa(quality))
		case "nvenc":
			a = append(a, "-rc", "vbr", "-cq", strconv.Itoa(quality), "-pix_fmt", "yuv420p")
		default:
			a = append(a, "-crf", strconv.Itoa(quality), "-pix_fmt", "yuv420p")
		}
	}
	if plan != nil && plan.Copy {
		a = append(a, "-c:a", "copy")
	} else {
		audioKbps := o.Audio.BitrateKbps
		if plan != nil {
			audioKbps = plan.AudioBitrateKbps
		}
		a = append(a, "-c:a", audioCodec, "-b:a", strconv.Itoa(audioKbps)+"k")
	}
	if o.Container == "mp4" {
		a = append(a, "-movflags", "+faststart")
	}
	a = append(a, filepath.Join(outDir, artifact))
	return s.sandbox(&c, in, outDir, selected.devices, a), nil
}
func (s *Server) sandbox(c *cgroup, in, output string, gpuDevices, program []string) *exec.Cmd {
	args := []string{"--cgroup", func() string {
		if c != nil {
			return c.path
		}
		return ""
	}(), "--", "/usr/local/bin/sandbox-exec", "--profile", "cpu", "--input", in}
	if output != "" {
		args = append(args, "--output", output)
	}
	for _, device := range gpuDevices {
		args = append(args, "--gpu-device", device)
	}
	args = append(args, "--")
	args = append(args, program...)
	return exec.Command("/usr/local/bin/cgroup-exec", args...)
}
func (s *Server) dir(id string) string       { return filepath.Join(s.cfg.Data, "spool", id) }
func (s *Server) outputDir(id string) string { return filepath.Join(s.cfg.Output, id) }
func chunkBytes(size, chunkSize int64, n int) int64 {
	v := size - int64(n)*chunkSize
	if v < chunkSize {
		return v
	}
	return chunkSize
}
func (s *Server) lifecycleLock(jobID string) *sync.RWMutex {
	v, _ := s.lifecycleLocks.LoadOrStore(jobID, &sync.RWMutex{})
	return v.(*sync.RWMutex)
}
func (s *Server) chunkLock(jobID string, number int) *sync.Mutex {
	k := jobID + ":" + strconv.Itoa(number)
	v, _ := s.chunkLocks.LoadOrStore(k, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type cgroup struct{ path string }

type videoEncoder struct {
	mode, name string
	devices    []string
	hardware   bool
}

var (
	errUnsupportedVideoCodec   = errors.New("unsupported video codec")
	errUnsupportedAudioCodec   = errors.New("unsupported audio codec")
	errUnsupportedEncoderCodec = errors.New("unsupported video encoder for codec")
)

func (s *Server) videoEncoders(video Video) ([]videoEncoder, error) {
	softwareName, err := softwareEncoder(video.Codec)
	if err != nil {
		return nil, err
	}
	software := videoEncoder{mode: "software", name: softwareName}
	switch video.Encoder {
	case "software":
		return []videoEncoder{software}, nil
	case "vaapi":
		device, ok := s.vaapiDevice()
		if !ok {
			return nil, errors.New("VA-API encoder requested but VAAPI_DEVICE is not an accessible render node")
		}
		return []videoEncoder{{mode: "vaapi", name: video.Codec + "_vaapi", devices: []string{device}, hardware: true}}, nil
	case "nvenc":
		name, ok := nvencEncoder(video.Codec)
		if !ok {
			return nil, fmt.Errorf("%w: NVENC does not support %s", errUnsupportedEncoderCodec, video.Codec)
		}
		devices := nvidiaDevices()
		if len(devices) == 0 {
			return nil, errors.New("NVENC encoder requested but NVIDIA device nodes are unavailable")
		}
		return []videoEncoder{{mode: "nvenc", name: name, devices: devices, hardware: true}}, nil
	case "auto":
		candidates := make([]videoEncoder, 0, 3)
		if name, ok := nvencEncoder(video.Codec); ok {
			if devices := nvidiaDevices(); len(devices) > 0 {
				candidates = append(candidates, videoEncoder{mode: "nvenc", name: name, devices: devices, hardware: true})
			}
		}
		if device, ok := s.vaapiDevice(); ok {
			candidates = append(candidates, videoEncoder{mode: "vaapi", name: video.Codec + "_vaapi", devices: []string{device}, hardware: true})
		}
		return append(candidates, software), nil
	default:
		return nil, errors.New("unsupported video encoder")
	}
}

func nvencEncoder(codec string) (string, bool) {
	switch codec {
	case "h264", "hevc", "av1":
		return codec + "_nvenc", true
	default:
		return "", false
	}
}

func (s *Server) vaapiDevice() (string, bool) {
	if !vaapiRenderNode(s.cfg.VAAPIDevice) {
		return "", false
	}
	return s.cfg.VAAPIDevice, charDevice(s.cfg.VAAPIDevice)
}

func vaapiRenderNode(path string) bool {
	if filepath.Dir(path) != "/dev/dri" {
		return false
	}
	name := strings.TrimPrefix(filepath.Base(path), "renderD")
	if name == filepath.Base(path) || name == "" {
		return false
	}
	for _, char := range name {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func nvidiaDevices() []string {
	paths := []string{"/dev/nvidiactl", "/dev/nvidia-modeset", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"}
	indexed, _ := filepath.Glob("/dev/nvidia[0-9]*")
	paths = append(paths, indexed...)
	caps, _ := filepath.Glob("/dev/nvidia-caps/nvidia-cap*")
	paths = append(paths, caps...)
	// Recent NVIDIA CDI configurations may also expose a DRM render node.
	// CUDA/libcuda can use that node during device discovery, so pass the
	// existing render nodes through to the nested Landlock sandbox as well.
	renderNodes, _ := filepath.Glob("/dev/dri/renderD*")
	paths = append(paths, renderNodes...)
	devices := make([]string, 0, len(paths))
	hasGPU := false
	for _, path := range paths {
		if !charDevice(path) {
			continue
		}
		if strings.HasPrefix(filepath.Base(path), "nvidia") && len(filepath.Base(path)) > len("nvidia") && filepath.Base(path)[len("nvidia")] >= '0' && filepath.Base(path)[len("nvidia")] <= '9' {
			hasGPU = true
		}
		devices = append(devices, path)
	}
	if !hasGPU {
		return nil
	}
	return devices
}

func charDevice(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (s *Server) makeCgroup(id string) (cgroup, error) {
	if s.cfg.Cgroup == "" {
		if s.cfg.RequireCgroup {
			return cgroup{}, errors.New("CGROUP_ROOT required")
		}
		return cgroup{}, nil
	}
	p := filepath.Join(s.cfg.Cgroup, "job-"+id)
	if e := os.Mkdir(p, 0755); e != nil {
		return cgroup{}, e
	}
	for k, v := range map[string]string{"memory.max": "8589934592", "memory.swap.max": "0", "pids.max": "64", "cpu.weight": "100", "cpu.max": "200000 100000"} {
		if e := os.WriteFile(filepath.Join(p, k), []byte(v), 0644); e != nil {
			return cgroup{}, e
		}
	}
	return cgroup{p}, nil
}
func (c cgroup) cleanup() {
	if c.path == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(c.path, "cgroup.kill"), []byte("1"), 0644)
	for i := 0; i < 50; i++ {
		b, e := os.ReadFile(filepath.Join(c.path, "cgroup.events"))
		if e == nil && strings.Contains(string(b), "populated 0") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.Remove(c.path)
}
func normalize(r Request, c Config) (Output, error) {
	if r.Input.Size <= 0 || r.Input.Size > c.Capacity || validateFilename(r.Input.Filename) != nil {
		return Output{}, errors.New("invalid input")
	}
	if r.Input.SHA256 != "" && !validSHA256(r.Input.SHA256) {
		return Output{}, errors.New("input sha256 must be hex SHA-256")
	}
	base := Output{Container: "mp4", Video: Video{Codec: "h264", Encoder: "auto", Quality: Quality{Mode: "auto"}, Resolution: Resolution{Mode: "auto"}}, Audio: Audio{Codec: "aac", BitrateKbps: autoDefaultAudioKbps}}
	if r.Output.Preset != "" {
		switch r.Output.Preset {
		case "web-1080p":
			base.Video.Quality = Quality{Mode: "quality", Value: 72}
			base.Video.Resolution = Resolution{Mode: "fit", Width: 1920, Height: 1080}
			base.Audio.BitrateKbps = 160
		case "archive-av1":
			base = Output{Container: "mkv", Video: Video{Codec: "av1", Encoder: "auto", Quality: Quality{Mode: "quality", Value: 80}, Resolution: Resolution{Mode: "source"}}, Audio: Audio{Codec: "opus", BitrateKbps: 192}}
		default:
			return base, errors.New("unknown preset")
		}
	}
	o := merge(base, r.Output)
	if !compatible(o) {
		return o, errors.New("incompatible container/codec combination")
	}
	switch o.Video.Encoder {
	case "auto", "software", "vaapi":
		// These encoders are valid for every supported video codec.
	case "nvenc":
		if _, ok := nvencEncoder(o.Video.Codec); !ok {
			return o, fmt.Errorf("%w: NVENC does not support %s", errUnsupportedEncoderCodec, o.Video.Codec)
		}
	default:
		return o, errors.New("unsupported video encoder")
	}
	if o.Audio.BitrateKbps < 16 || o.Audio.BitrateKbps > 512 {
		return o, errors.New("audio bitrate out of range")
	}
	switch o.Video.Quality.Mode {
	case "auto":
		// The automatic rate-control plan is resolved after ffprobe.
	case "quality":
		if o.Video.Quality.Value < 0 || o.Video.Quality.Value > 100 {
			return o, errors.New("quality out of range")
		}
	case "crf":
		minCRF, maxCRF, err := crfRange(o.Video.Codec)
		if err != nil {
			return o, err
		}
		if o.Video.Quality.CRF < minCRF || o.Video.Quality.CRF > maxCRF {
			return o, errors.New("CRF out of range")
		}
	default:
		return o, errors.New("unsupported quality mode")
	}
	resol := o.Video.Resolution
	switch resol.Mode {
	case "auto":
		// The automatic resolution is resolved after ffprobe.
	case "source":
		// The source resolution does not need dimensions.
	case "fit":
		if resol.Width < 2 || resol.Height < 2 || resol.Width > c.MaxWidth || resol.Height > c.MaxHeight {
			return o, errors.New("resolution out of range")
		}
	default:
		return o, errors.New("unsupported resolution mode")
	}
	return o, nil
}
func merge(b, x Output) Output {
	if x.Container != "" {
		b.Container = x.Container
	}
	if x.Video.Codec != "" {
		b.Video.Codec = x.Video.Codec
	}
	if x.Video.Encoder != "" {
		b.Video.Encoder = x.Video.Encoder
	}
	if x.Video.Quality.Mode != "" {
		b.Video.Quality.Mode = x.Video.Quality.Mode
		// Zero is valid for both public quality and CRF, so the selected
		// mode makes the corresponding scalar an explicit override.
		switch x.Video.Quality.Mode {
		case "quality":
			b.Video.Quality.Value = x.Video.Quality.Value
		case "crf":
			b.Video.Quality.CRF = x.Video.Quality.CRF
		}
	}
	if x.Video.Resolution.Mode != "" {
		b.Video.Resolution.Mode = x.Video.Resolution.Mode
	}
	if x.Video.Resolution.Width != 0 {
		b.Video.Resolution.Width = x.Video.Resolution.Width
	}
	if x.Video.Resolution.Height != 0 {
		b.Video.Resolution.Height = x.Video.Resolution.Height
	}
	if x.Video.Resolution.Upscale != nil {
		b.Video.Resolution.Upscale = x.Video.Resolution.Upscale
	}
	if x.Audio.Codec != "" {
		b.Audio.Codec = x.Audio.Codec
	}
	if x.Audio.BitrateKbps != 0 {
		b.Audio.BitrateKbps = x.Audio.BitrateKbps
	}
	b.Preset = x.Preset
	return b
}
func compatible(o Output) bool {
	m := map[string]map[string]map[string]bool{"mp4": {"v": {"h264": true, "hevc": true, "av1": true}, "a": {"aac": true}}, "webm": {"v": {"vp9": true, "av1": true}, "a": {"opus": true}}, "mkv": {"v": {"h264": true, "hevc": true, "av1": true, "vp9": true}, "a": {"aac": true, "opus": true, "flac": true}}}
	f, ok := m[o.Container]
	return ok && f["v"][o.Video.Codec] && f["a"][o.Audio.Codec]
}
func scale(r Resolution) string {
	if r.Mode != "fit" {
		return ""
	}
	up := ""
	if r.Upscale == nil || !*r.Upscale {
		up = ":force_original_aspect_ratio=decrease"
	}
	return fmt.Sprintf("scale=%d:%d%s:force_divisible_by=2", r.Width, r.Height, up)
}
func crfRange(codec string) (int, int, error) {
	switch codec {
	case "av1", "vp9":
		return 0, 63, nil
	case "h264", "hevc":
		return 0, 51, nil
	default:
		return 0, 0, fmt.Errorf("%w: %s", errUnsupportedVideoCodec, codec)
	}
}
func crf(v Video) (int, error) {
	_, max, err := crfRange(v.Codec)
	if err != nil {
		return 0, err
	}
	if v.Quality.Mode == "crf" {
		return v.Quality.CRF, nil
	}
	return max - (v.Quality.Value * max / 100), nil
}
func softwareEncoder(codec string) (string, error) {
	switch codec {
	case "h264":
		return "libx264", nil
	case "hevc":
		return "libx265", nil
	case "vp9":
		return "libvpx-vp9", nil
	case "av1":
		return "libsvtav1", nil
	default:
		return "", fmt.Errorf("%w: %s", errUnsupportedVideoCodec, codec)
	}
}
func audioEncoder(codec string) (string, error) {
	switch codec {
	case "aac":
		return "aac", nil
	case "opus":
		return "libopus", nil
	case "flac":
		return "flac", nil
	default:
		return "", fmt.Errorf("%w: %s", errUnsupportedAudioCodec, codec)
	}
}
func reservation(n int64, o Output) (int64, int64, int64) {
	factor := int64(60)
	if o.Preset == "archive-av1" {
		factor = 100
	}
	out := n * factor / 100
	safety := out / 10
	return out, safety, n
}
func hashFile(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func id() string { b := make([]byte, 16); rand.Read(b); return hex.EncodeToString(b) }
func validateFilename(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) || filepath.Base(name) != name || strings.Contains(name, `\`) {
		return errors.New("invalid filename")
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) {
			return errors.New("invalid filename")
		}
	}
	return nil
}
func downloadFilename(j Job) string {
	base := strings.TrimSuffix(j.Filename, filepath.Ext(j.Filename))
	if base == "" {
		base = j.Filename
	}
	return base + filepath.Ext(j.Artifact.String)
}
func nullable(v sql.NullString) *string {
	if v.Valid {
		return &v.String
	}
	return nil
}
func artifactURL(j Job) *string {
	if j.State == completed && j.Artifact.Valid {
		url := "/v1/jobs/" + j.ID + "/output"
		return &url
	}
	return nil
}
func (s *Server) uploadInstructions(j Job) *APIUploadInstructions {
	if j.State != admitted && j.State != uploading {
		return nil
	}
	return &APIUploadInstructions{Mode: "chunked", ChunkSize: j.ChunkSize, RequiredHeader: "X-Chunk-SHA256"}
}

func (s *Server) createJobResponse(j Job) APICreateJobResponse {
	return APICreateJobResponse{ID: j.ID, State: j.State, ReservationBytes: j.Reserved, Upload: s.uploadInstructions(j), ExpiresAt: nullable(j.ExpiresAt)}
}

func processingOrCompletedState(state string) bool {
	switch state {
	case finalizing, staged, probing, validated, queued, starting, transcoding, completed:
		return true
	default:
		return false
	}
}

func validIdempotencyKey(key string) bool {
	if len(key) < 16 || len(key) > 128 {
		return false
	}
	for index, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || (index > 0 && strings.ContainsRune("._:-", r)) {
			continue
		}
		return false
	}
	return true
}

func uploadExpiry(now time.Time, retention time.Duration) string {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	return now.Add(retention).Format(time.RFC3339Nano)
}
func truncate(x string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(x) > n {
		return x[:n]
	}
	return x
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func configFromEnv() (Config, error) {
	dataDir := env("DATA_DIR", "/var/lib/starryeyes")
	outputDir := os.Getenv("OUTPUT_DIR")
	if e := validateOutputDir(dataDir, outputDir); e != nil {
		return Config{}, e
	}
	retention, e := time.ParseDuration(env("UPLOAD_RETENTION", "168h"))
	if e != nil || retention <= 0 {
		return Config{}, errors.New("UPLOAD_RETENTION must be a positive Go duration")
	}
	return Config{Data: dataDir, Output: outputDir, Listen: env("LISTEN_ADDR", ":8080"), Capacity: envI("SPOOL_CAPACITY_BYTES", 2<<40), Chunk: envI("CHUNK_SIZE_BYTES", 64<<20), Active: int(envI("MAX_ACTIVE_TRANSCODES", 2)), Cgroup: os.Getenv("CGROUP_ROOT"), VAAPIDevice: env("VAAPI_DEVICE", "/dev/dri/renderD128"), RequireCgroup: envB("REQUIRE_CGROUP", true), RequireLandlock: envB("REQUIRE_LANDLOCK", true), MaxWidth: int(envI("MAX_WIDTH", 7680)), MaxHeight: int(envI("MAX_HEIGHT", 4320)), MaxStreams: int(envI("MAX_STREAMS", 64)), MaxDuration: float64(envI("MAX_DURATION_SECONDS", 86400)), UploadRetention: retention}, nil
}
func validateOutputDir(dataDir, outputDir string) error {
	if outputDir == "" {
		return errors.New("OUTPUT_DIR is required")
	}
	if !filepath.IsAbs(outputDir) {
		return errors.New("OUTPUT_DIR must be an absolute path")
	}
	dataPath, e := filepath.Abs(dataDir)
	if e != nil {
		return fmt.Errorf("resolve DATA_DIR: %w", e)
	}
	outputPath := filepath.Clean(outputDir)
	rel, e := filepath.Rel(dataPath, outputPath)
	if e != nil {
		return fmt.Errorf("compare DATA_DIR and OUTPUT_DIR: %w", e)
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("OUTPUT_DIR must be outside DATA_DIR")
	}
	return nil
}
func envI(k string, d int64) int64 {
	if v, e := strconv.ParseInt(os.Getenv(k), 10, 64); e == nil && v > 0 {
		return v
	}
	return d
}
func envB(k string, d bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	return v == "1" || strings.EqualFold(v, "true")
}
func out[T any](w http.ResponseWriter, n int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(n)
	json.NewEncoder(w).Encode(v)
}
func bad(w http.ResponseWriter, n int, m string) { out(w, n, APIError{Error: m}) }
