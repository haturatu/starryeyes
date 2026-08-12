# starryeyes

`starryeyes` is a Linux media conversion service with resumable HTTP uploads, durable job metadata, admission control, and sandboxed FFmpeg workers.

The public API accepts a validated, high-level output specification. It never accepts arbitrary FFmpeg arguments or shell commands.

## API documentation

The live server publishes OpenAPI at `/openapi.json` and interactive documentation at `/docs`. The same API definition is generated during CI and published at [haturatu.github.io/starryeyes](https://haturatu.github.io/starryeyes/) after changes reach `main`.

The static documentation deliberately uses `http://localhost:8080` as its example server. Replace it in the UI with the URL of your own self-hosted Starryeyes instance before making requests.

## Quick start with Docker Compose

The Compose service uses distinct host paths for local job metadata/input spool and completed output. Copy `.env.example` to `.env` and set `OUTPUT_DIR_HOST`; it is the only required Compose variable. Every other entry is optional and has an explicit fallback in `compose.yaml`. Docker Compose automatically reads `.env` next to `compose.yaml`; `DATA_DIR_HOST` and `OUTPUT_DIR_HOST` are used only as host-side bind-mount sources and are not passed into the container.

```sh
sudo install -d -o 10001 -g 999 -m 0750 /var/lib/starryeyes
cp .env.example .env
# Edit .env if your output path differs.
docker compose config
docker compose up --build
```

### rclone FUSE output storage

When `OUTPUT_DIR_HOST` is an rclone FUSE mount, Docker must be able to traverse the mount as the daemon user. Enable `user_allow_other` once in `/etc/fuse.conf`, then mount a generic remote and create the bind-mount source before starting Compose:

```sh
# /etc/fuse.conf: enable this once.
sudo sh -c 'echo user_allow_other >> /etc/fuse.conf'

# Replace remote:media-output with your own rclone remote and bucket/path.
rclone mount remote:media-output /mnt/remote-output \
  --allow-other \
  --vfs-cache-mode writes

mkdir -p /mnt/remote-output/starryeyes
```

Set `OUTPUT_DIR_HOST=/mnt/remote-output/starryeyes` in `.env`. Without `--allow-other`, the rclone-mount owner may access the path while the Docker daemon cannot, causing bind-mount creation or startup failures.

The service runs without `privileged`, drops all Linux capabilities, uses a read-only container root filesystem, and mounts only `/tmp` as writable tmpfs. The worker applies Landlock, seccomp, and cgroup limits before executing FFmpeg. There is no unconfined fallback.

Inspect the service and data from the host or container:

```sh
docker compose ps
docker compose exec starryeyes ls -la /var/lib/starryeyes
sudo ls -la /var/lib/starryeyes/spool
sudo ls -la "$(sed -n 's/^OUTPUT_DIR_HOST=//p' .env)"
```

Upload a sample file with the included client:

```sh
go run ./cmd/demo-client --file sample.mp4
```

## Recursive directory upload

Use [`scripts/upload-directory.sh`](scripts/upload-directory.sh) to submit every supported video file below a directory. It uploads files sequentially, requests the `web-1080p` preset by default, and prints the job ID for each submitted file. When a server queues a job for local spool capacity, the script polls it until upload is admitted; set `ADMISSION_POLL_SECONDS` to change the default 10-second interval.

```sh
scripts/upload-directory.sh http://localhost:8080 /path/to/videos
```

The script recognises `3gp`, `avi`, `flv`, `m2ts`, `m4v`, `mkv`, `mov`, `mp4`, `mpeg`, `mpg`, `mts`, `ts`, `webm`, and `wmv` extensions case-insensitively. It requires Bash 4+, `curl`, `jq`, `sha256sum`, GNU `stat`, and GNU `dd`. Set `OUTPUT_PRESET=archive-av1` to request that preset instead.

## Data layout

`DATA_DIR` stores durable metadata and the local input spool. `OUTPUT_DIR` is required, must be an absolute path outside `DATA_DIR`, and stores completed artifacts. The Compose configuration mounts `OUTPUT_DIR_HOST` at `/mnt/output` and sets `OUTPUT_DIR=/mnt/output`.

```text
/var/lib/starryeyes/
├── jobs.sqlite
└── spool/<job-id>/input

/mnt/output/
└── <job-id>/output.<container>
```

The output artifact is available through:

```text
GET /v1/jobs/<job-id>/output
```

## Upload and job lifecycle

1. `POST /v1/jobs` always creates a `PENDING` job. It does not return a capacity-exhaustion error.
2. The server admits pending jobs in FIFO order when local input-spool capacity is available. `GET /v1/jobs/<job-id>` changes to `ADMITTED` and includes `upload` instructions only after the input file has been preallocated.
3. `PUT /v1/jobs/<job-id>/chunks/<number>` uploads a fixed-size chunk with `X-Chunk-SHA256`.
4. Repeating a chunk with the same checksum is idempotent; a different checksum returns `409 Conflict`.
5. `POST /v1/jobs/<job-id>/complete` atomically finalizes the upload.
6. The server computes the whole-file hash, runs sandboxed `ffprobe`, validates media limits, and queues the job.
7. The worker runs FFmpeg with a normalized, allowlisted JobSpec.

Job states include `PENDING`, `ADMITTED`, `UPLOADING`, `FINALIZING`, `STAGED`, `PROBING`, `VALIDATED`, `QUEUED`, `TRANSCODING`, `COMPLETED`, and `FAILED`.

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
OUTPUT_DIR=/mnt/output             # required, absolute, and outside DATA_DIR
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
go run ./cmd/gen-docs public
cp .env.example .env
docker compose config
docker compose up --build
```
