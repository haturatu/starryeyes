# starryeyes

`starryeyes` is a Linux media conversion service with resumable HTTP uploads, durable job metadata, admission control, and sandboxed FFmpeg workers.

The public API accepts a validated, high-level output specification. It never accepts arbitrary FFmpeg arguments or shell commands.

## Quick start with Docker Compose

The Compose service is named `starryeyes` and uses a host bind mount at `/var/lib/starryeyes`:

```sh
sudo install -d -o 10001 -g 999 -m 0750 /var/lib/starryeyes
docker compose up --build
```

The service runs without `privileged`, drops all Linux capabilities, uses a read-only container root filesystem, and mounts only `/tmp` as writable tmpfs. The worker applies Landlock, seccomp, and cgroup limits before executing FFmpeg. There is no unconfined fallback.

Inspect the service and data from the host or container:

```sh
docker compose ps
docker compose exec starryeyes ls -la /var/lib/starryeyes
sudo ls -la /var/lib/starryeyes/spool
sudo ls -la /var/lib/starryeyes/output
```

Upload a sample file with the included client:

```sh
go run ./cmd/demo-client --file sample.mp4
```

## Data layout

The default data directory is `/var/lib/starryeyes`.

```text
/var/lib/starryeyes/
├── jobs.sqlite
├── spool/<job-id>/input
└── output/<job-id>/output.<container>
```

The output artifact is available through:

```text
GET /v1/jobs/<job-id>/output
```

## Upload and job lifecycle

1. `POST /v1/jobs` creates a job and reserves input/output capacity.
2. `PUT /v1/jobs/<job-id>/chunks/<number>` uploads a fixed-size chunk with `X-Chunk-SHA256`.
3. Repeating a chunk with the same checksum is idempotent; a different checksum returns `409 Conflict`.
4. `POST /v1/jobs/<job-id>/complete` atomically finalizes the upload.
5. The server computes the whole-file hash, runs sandboxed `ffprobe`, validates media limits, and queues the job.
6. The worker runs FFmpeg with a normalized, allowlisted JobSpec.

Job states include `CREATED`, `UPLOADING`, `FINALIZING`, `STAGED`, `PROBING`, `VALIDATED`, `QUEUED`, `TRANSCODING`, `COMPLETED`, and `FAILED`.

## Security model

- Landlock ABI 4 or newer is required at startup. Runtime libraries are read/execute-only, input is read-only, and only the job output directory and `/tmp` receive write/create rights.
- Seccomp denies sockets, mount and namespace changes, ptrace, BPF, and other high-risk operations. FFmpeg and ffprobe additionally use `-protocol_whitelist file,pipe`.
- cgroup v2 limits memory, swap, PIDs, CPU weight, and CPU quota per job. Production systemd deployment uses delegated cgroups via [deploy/starryeyes.service](deploy/starryeyes.service).
- The Docker container has no added capabilities, no privileged mode, `no-new-privileges`, a read-only root filesystem, and a dedicated bind-mounted data directory.

Landlock controls access to the existing filesystem rather than creating a replacement root filesystem. Docker, seccomp, DAC, and host MAC policy remain part of the defense-in-depth boundary.

## Configuration

Important environment variables include:

```text
DATA_DIR=/var/lib/starryeyes
CHUNK_SIZE_BYTES=67108864
MAX_ACTIVE_TRANSCODES=2
SPOOL_CAPACITY_BYTES=21474836480
REQUIRE_LANDLOCK=true
REQUIRE_CGROUP=true       # production; Compose sets false because it has no delegation
CGROUP_ROOT=/sys/fs/cgroup/starryeyes.service
```

## Validation

```sh
go test ./...
go vet ./...
docker compose config
docker compose up --build
```
