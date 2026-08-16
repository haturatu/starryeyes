#!/usr/bin/env bash
# Check the host prerequisites required by compose.nvidia.yaml.
set -u

failures=0

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	failures=$((failures + 1))
}

pass() {
	printf 'PASS: %s\n' "$*"
}

printf '%s\n' '--- NVIDIA driver ---'
if ! command -v nvidia-smi >/dev/null 2>&1; then
	fail 'nvidia-smi is not installed'
elif nvidia_smi_output=$(nvidia-smi 2>&1); then
	pass 'nvidia-smi can communicate with the NVIDIA driver'
	printf '%s\n' "$nvidia_smi_output"
else
	fail 'nvidia-smi cannot communicate with the NVIDIA driver'
	printf '%s\n' "$nvidia_smi_output" >&2
fi

if [[ -c /dev/nvidiactl && -c /dev/nvidia-uvm ]] && compgen -G '/dev/nvidia[0-9]*' >/dev/null; then
	pass 'NVIDIA device nodes are present'
else
	fail 'NVIDIA device nodes are missing under /dev'
fi

printf '%s\n' '--- NVIDIA Container Toolkit / CDI ---'
if ! command -v nvidia-ctk >/dev/null 2>&1; then
	fail 'nvidia-ctk is not installed'
else
	if cdi_output=$(nvidia-ctk cdi list 2>&1); then
		if grep -q 'nvidia.com/gpu' <<<"$cdi_output"; then
			pass 'NVIDIA CDI devices are available'
		else
			fail 'nvidia-ctk returned no NVIDIA GPU CDI device'
		fi
		printf '%s\n' "$cdi_output"
	else
		fail 'nvidia-ctk cdi list failed'
		printf '%s\n' "$cdi_output" >&2
	fi
fi

printf '%s\n' '--- Docker ---'
if ! command -v docker >/dev/null 2>&1; then
	fail 'docker is not installed'
elif docker info >/dev/null 2>&1; then
	pass 'Docker daemon is reachable'
else
  fail 'Docker daemon is not reachable by the current user'
fi

if (( failures > 0 )); then
	printf '\n%d prerequisite check(s) failed.\n' "$failures" >&2
	exit 1
fi

printf '\nAll NVIDIA prerequisites are ready.\n'
