#!/usr/bin/env bash
# Diagnose CUDA initialization under the production sandbox and under
# Landlock-only confinement. Use with compose.nvidia-debug.yaml.
set -Eeuo pipefail

container_name=${1:-starryeyes-starryeyes-1}
trace_output=${2:-cuda-sandbox.strace}
if docker info >/dev/null 2>&1; then
	Docker=(docker)
else
	Docker=(sudo docker)
fi

docker_exec() {
	"${Docker[@]}" exec "$container_name" "$@"
}

if ! "${Docker[@]}" inspect "$container_name" >/dev/null 2>&1; then
	echo "container not found: $container_name" >&2
	exit 2
fi

run_cuda() {
	local launcher=$1
	docker_exec sh -ceu '
		set -- "$1" --profile cpu --input /dev/null
		for p in \
			/dev/nvidiactl \
			/dev/nvidia-modeset \
			/dev/nvidia-uvm \
			/dev/nvidia-uvm-tools \
			/dev/nvidia[0-9]* \
			/dev/nvidia-caps/nvidia-cap* \
			/dev/dri/renderD*
		do
			test -c "$p" && set -- "$@" --gpu-device "$p"
		done
		exec "$@" -- ffmpeg -hide_banner -loglevel error -init_hw_device cuda \
			-f lavfi -i testsrc2=size=1280x720:rate=30 -t 1 -f null -
	' sh "$launcher"
}

trace_cuda() {
	local launcher=$1
	local remote_trace=$2
	docker_exec sh -ceu '
		launcher=$1
		trace_file=$2
		rm -f "$trace_file"
		set -- "$launcher" --profile cpu --input /dev/null
		for p in \
			/dev/nvidiactl \
			/dev/nvidia-modeset \
			/dev/nvidia-uvm \
			/dev/nvidia-uvm-tools \
			/dev/nvidia[0-9]* \
			/dev/nvidia-caps/nvidia-cap* \
			/dev/dri/renderD*
		do
			test -c "$p" && set -- "$@" --gpu-device "$p"
		done
	strace -f -yy -s 256 -e trace=%file,%network,ioctl -o "$trace_file" "$@" -- \
		ffmpeg -hide_banner -loglevel error -init_hw_device cuda \
		-f lavfi -i testsrc2=size=1280x720:rate=30 -t 1 -f null - || true
	test -s "$trace_file"
	' sh "$launcher" "$remote_trace"
}

copy_trace() {
	local remote_trace=$1
	local local_trace=$2
	if docker_exec test -s "$remote_trace"; then
		# /tmp is a tmpfs mount in the service container.  docker cp does not
		# archive files from that mount with all Docker versions, so stream it
		# through docker exec instead.
		docker_exec cat "$remote_trace" > "$local_trace"
		echo "trace copied to $local_trace"
		echo '--- denied or NVIDIA-related trace entries ---'
		rg -n ' = -1 (EACCES|EPERM)|/proc/self/task/.*/comm|/proc/filesystems|/sys/module/nvidia|/proc/driver/nvidia|socket\(AF_|connect\(.* = -1' "$local_trace" | head -200 || true
	else
		echo "trace was not produced: $remote_trace" >&2
	fi
}

echo '--- Landlock-only CUDA init ---'
if run_cuda /usr/local/bin/sandbox-exec-landlock-only; then
	echo 'PASS: Landlock-only CUDA init'
	landlock_status=0
else
	landlock_status=$?
	echo "FAIL: Landlock-only CUDA init (exit $landlock_status)" >&2
fi

echo '--- Landlock-only CUDA init trace ---'
trace_cuda /usr/local/bin/sandbox-exec-landlock-only /tmp/cuda-landlock-only.strace || true
copy_trace /tmp/cuda-landlock-only.strace "${trace_output%.strace}-landlock-only.strace"

echo '--- production sandbox CUDA init with strace ---'
production_status=0
if run_cuda /usr/local/bin/sandbox-exec; then
	echo 'PASS: production sandbox CUDA init'
else
	production_status=$?
	echo "FAIL: production sandbox CUDA init (exit $production_status)" >&2
fi
trace_cuda /usr/local/bin/sandbox-exec /tmp/cuda-sandbox.strace || true
copy_trace /tmp/cuda-sandbox.strace "$trace_output"

if (( landlock_status != 0 )); then
	echo 'Landlock-only failed; the remaining issue is filesystem/device access.' >&2
	exit 1
fi

if (( production_status != 0 )); then
	echo 'Landlock-only passed but production sandbox failed; inspect the production trace for seccomp interaction.' >&2
	exit 1
fi

echo 'Landlock-only and production sandbox CUDA initialization passed.'
