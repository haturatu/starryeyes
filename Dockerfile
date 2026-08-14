FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/starryeyes ./cmd/starryeyes
# Compile the Landlock/seccomp and cgroup launchers in the build stage so no
# toolchain ships in the runtime image.
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libseccomp-dev \
 && gcc -O2 -Wall -Wextra -o /out/sandbox-exec cmd/sandbox-exec/sandbox-exec.c -lseccomp \
 && gcc -O2 -Wall -Wextra -o /out/cgroup-exec cmd/cgroup-exec/cgroup-exec.c \
 && rm -rf /var/lib/apt/lists/*

FROM debian:bookworm-slim AS runtime-base
# Use the requested IPA ICSCoE mirror for Debian archive and updates. Use its
# HTTP endpoint during bootstrap: ca-certificates is itself installed by this
# apt invocation. Do not rewrite the distinct debian-security URI.
RUN sed -i '/^URIs: http:\/\/deb.debian.org\/debian$/s|http://deb.debian.org/debian|http://ftp.udx.icscoe.jp/Linux/debian|' /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg libseccomp2 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/starryeyes /usr/local/bin/starryeyes
COPY --from=build /out/sandbox-exec /usr/local/bin/sandbox-exec
COPY --from=build /out/cgroup-exec /usr/local/bin/cgroup-exec
RUN useradd --system --uid 10001 --create-home starryeyes \
 && mkdir -p /var/lib/starryeyes/spool && chown -R starryeyes:starryeyes /var/lib/starryeyes
USER starryeyes
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/starryeyes"]

# VA-API is opt-in so the default and NVIDIA images do not carry Mesa/Intel
# userspace drivers. Install both current Intel and legacy Intel drivers as
# well as the Mesa driver used by AMD GPUs; the host render node is supplied
# by compose.vaapi.yaml.
FROM runtime-base AS runtime-vaapi
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      mesa-va-drivers \
      intel-media-va-driver \
      i965-va-driver \
 && rm -rf /var/lib/apt/lists/*
USER starryeyes

# NVIDIA userspace driver libraries are injected by the NVIDIA Container
# Toolkit from the host. Keep this target separate so Compose can select the
# intended hardware contract without installing a driver in the image.
FROM runtime-base AS runtime-nvidia
