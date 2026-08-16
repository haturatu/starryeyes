#!/usr/bin/env bash
# Start Starryeyes with the NVIDIA NVENC Compose override.
set -Eeuo pipefail

script_directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repository_directory=$(cd -- "$script_directory/.." && pwd)

cd "$repository_directory"
"$script_directory/check-nvidia-gpu.sh"
if (( EUID == 0 )); then
	docker_command=(docker)
else
	docker_command=(sudo docker)
fi
exec "${docker_command[@]}" compose \
	-f compose.yaml \
	-f compose.nvidia.yaml \
	up --build "$@"
