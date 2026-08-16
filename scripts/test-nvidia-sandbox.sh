#!/usr/bin/env bash
# Compare direct CUDA/NVENC access with the Starryeyes Landlock/seccomp worker.
set -Eeuo pipefail

container_name=${1:-starryeyes-starryeyes-1}
failures=0
if docker info >/dev/null 2>&1; then
	Docker=(docker)
elif (( EUID == 0 )); then
	Docker=(docker)
else
	Docker=(sudo docker)
fi

docker_exec() {
	"${Docker[@]}" exec "$container_name" "$@"
}

run_check() {
	local name=$1
	shift
	echo "--- $name ---"
	if "$@"; then
		echo "PASS: $name"
	else
		local status=$?
		echo "FAIL: $name (exit $status)" >&2
		failures=$((failures + 1))
	fi
}

if ! "${Docker[@]}" inspect "$container_name" >/dev/null 2>&1; then
	echo "container not found: $container_name" >&2
	exit 2
fi

echo '--- container NVIDIA device inventory ---'
docker_exec sh -ceu '
	find /dev -maxdepth 2 \( -name "nvidia*" -o -name "renderD*" \) -ls 2>&1 || true
	printf "\n/dev/char NVIDIA links:\n"
	find /dev/char -maxdepth 1 -type l -ls 2>&1 || true
	printf "\n/proc/driver/nvidia:\n"
	find /proc/driver/nvidia -maxdepth 3 -ls 2>&1 || true
	printf "\n/sys/class NVIDIA entries:\n"
	find /sys/class -maxdepth 2 -iname "*nvidia*" -ls 2>&1 || true
	printf "\n/run NVIDIA entries:\n"
	find /run -maxdepth 3 -iname "*nvidia*" -ls 2>&1 || true
'

run_check 'NVIDIA FFmpeg capabilities' docker_exec sh -ceu '
	ffmpeg -hide_banner -hwaccels 2>&1 | grep -Eq "^[[:space:]]*cuda[[:space:]]*$"
	ffmpeg -hide_banner -encoders 2>&1 | grep -Eq "h264_nvenc|hevc_nvenc|av1_nvenc"
	ffmpeg -hide_banner -filters 2>&1 | grep -Eq "scale_cuda"
'

run_check 'direct nvidia-smi' docker_exec nvidia-smi

run_check 'sandboxed nvidia-smi' docker_exec sh -ceu '
	set -- /usr/local/bin/sandbox-exec --profile cpu --input /dev/null
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
	exec "$@" -- nvidia-smi
'

run_check 'direct CUDA init' docker_exec ffmpeg -hide_banner -loglevel error \
	-init_hw_device cuda \
	-f lavfi -i testsrc2=size=1280x720:rate=30 \
	-t 1 -f null -

run_check 'sandboxed CUDA init' docker_exec sh -ceu '
	set -- /usr/local/bin/sandbox-exec --profile cpu --input /dev/null
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
	exec "$@" -- ffmpeg -hide_banner -loglevel error \
		-init_hw_device cuda \
		-f lavfi -i testsrc2=size=1280x720:rate=30 \
		-t 1 -f null -
'

run_check 'direct NVENC' docker_exec ffmpeg -hide_banner -loglevel error \
	-f lavfi -i testsrc2=size=1280x720:rate=30 \
	-c:v h264_nvenc -t 3 -f null -

run_check 'sandboxed NVENC' docker_exec sh -ceu '
	set -- /usr/local/bin/sandbox-exec --profile cpu --input /dev/null
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
	exec "$@" -- ffmpeg -hide_banner -loglevel error \
		-f lavfi -i testsrc2=size=1280x720:rate=30 \
		-c:v h264_nvenc -t 3 -f null -
'

run_check 'sandboxed CUDA decode/scale/NVENC pipeline' docker_exec sh -ceu '
	input=/tmp/nvidia-pipeline-input.mp4
	trap "rm -f \"\$input\"" EXIT
	ffmpeg -hide_banner -loglevel error -y \
		-f lavfi -i testsrc2=size=1280x720:rate=30 \
		-c:v h264_nvenc -pix_fmt yuv420p -an -t 1 "$input"
	set -- /usr/local/bin/sandbox-exec --profile cpu --input "$input"
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
	exec "$@" -- ffmpeg -hide_banner -loglevel error \
		-hwaccel cuda -hwaccel_output_format cuda -i "$input" \
		-vf scale_cuda=640:360:force_original_aspect_ratio=decrease:force_divisible_by=2:format=yuv420p \
		-c:v h264_nvenc -an -t 1 -f null -
'

if (( failures > 0 )); then
	echo "$failures NVIDIA sandbox check(s) failed." >&2
	exit 1
fi

echo 'NVIDIA sandbox checks passed.'
