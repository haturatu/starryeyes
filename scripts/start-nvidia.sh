#!/usr/bin/env bash
# Start Starryeyes with the NVIDIA NVENC Compose override.
set -Eeuo pipefail

script_directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repository_directory=$(cd -- "$script_directory/.." && pwd)

cd "$repository_directory"
"$script_directory/check-nvidia-gpu.sh"
exec docker compose \
	-f compose.yaml \
	-f compose.nvidia.yaml \
	up --build "$@"
