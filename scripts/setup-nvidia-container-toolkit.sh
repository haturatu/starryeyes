#!/usr/bin/env bash
# Install and configure NVIDIA Container Toolkit and CDI for Docker.
set -Eeuo pipefail

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

if (( EUID != 0 )); then
	die 'run this script as root, for example: sudo bash scripts/setup-nvidia-container-toolkit.sh'
fi

command -v apt-get >/dev/null 2>&1 || die 'apt-get is required'
command -v install >/dev/null 2>&1 || die 'install is required'

command -v nvidia-smi >/dev/null 2>&1 || die 'nvidia-smi is not installed; configure the NVIDIA driver first'
nvidia-smi >/dev/null 2>&1 || die 'nvidia-smi cannot communicate with the NVIDIA driver; reboot and fix the driver before continuing'

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
	ca-certificates curl gnupg2

command -v curl >/dev/null 2>&1 || die 'curl was not installed'
command -v gpg >/dev/null 2>&1 || die 'gpg was not installed'

keyring_path=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
repository_path=/etc/apt/sources.list.d/nvidia-container-toolkit.list
temporary_key=$(mktemp)
temporary_repository=$(mktemp)
cleanup() {
	rm -f -- "$temporary_key" "$temporary_repository"
}
trap cleanup EXIT

curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey |
	gpg --dearmor >"$temporary_key"
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list |
	sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#' \
		>"$temporary_repository"

install -d -m 0755 /usr/share/keyrings /etc/apt/sources.list.d
install -o root -g root -m 0644 "$temporary_key" "$keyring_path"
install -o root -g root -m 0644 "$temporary_repository" "$repository_path"

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nvidia-container-toolkit

command -v nvidia-ctk >/dev/null 2>&1 || die 'nvidia-ctk was not installed'
nvidia-ctk runtime configure --runtime=docker

if command -v service >/dev/null 2>&1; then
	service docker restart
elif [[ -x /etc/init.d/docker ]]; then
	/etc/init.d/docker restart
else
	die 'cannot find a Docker service restart command; restart dockerd manually'
fi

install -d -m 0755 /etc/cdi
nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml

printf '\nAvailable NVIDIA CDI devices:\n'
nvidia-ctk cdi list
