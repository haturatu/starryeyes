FROM golang:1.24-bookworm AS build
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

FROM debian:bookworm-slim
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
 && mkdir -p /var/lib/starryeyes/spool /var/lib/starryeyes/output && chown -R starryeyes:starryeyes /var/lib/starryeyes
USER starryeyes
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/starryeyes"]
