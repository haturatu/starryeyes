#!/usr/bin/env bash
# Prepare the NVIDIA kernel module for the running kernel.
set -Eeuo pipefail

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

if (( EUID != 0 )); then
	die 'run this script as root, for example: sudo bash scripts/setup-nvidia-driver.sh'
fi

command -v apt-get >/dev/null 2>&1 || die 'apt-get is required'
command -v update-initramfs >/dev/null 2>&1 || die 'update-initramfs is required'

kernel_release=$(uname -r)
printf 'Preparing NVIDIA driver for kernel %s\n' "$kernel_release"

apt-get update

header_package="linux-headers-${kernel_release}"
if apt-cache policy "$header_package" 2>/dev/null |
	awk '$1 == "Candidate:" && $2 != "(none)" { found = 1 } END { exit !found }'; then
	printf 'Installing headers matching the running kernel: %s\n' "$header_package"
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		dkms "$header_package" nvidia-kernel-dkms
	dkms autoinstall -k "$kernel_release"
else
	cat <<EOF
Headers for the running kernel are not available from the configured repositories:
  $header_package

Installing the current Devuan kernel and header meta-packages instead. A reboot
into the newly installed kernel will be required before the NVIDIA module can load.
EOF
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		dkms linux-image-amd64 linux-headers-amd64 nvidia-kernel-dkms

	# DKMS otherwise also tries the currently running kernel, which may be an
	# older kernel whose headers are no longer available in the repository.
	header_kernels=()
	for kernel_directory in /lib/modules/*; do
		if [[ -d "$kernel_directory/build" ]]; then
			header_kernels+=("${kernel_directory##*/}")
		fi
	done
	((${#header_kernels[@]} > 0)) || die 'no installed kernel headers were found after package installation'
	target_kernel=$(printf '%s\n' "${header_kernels[@]}" | sort -V | tail -n 1)
	printf 'Building NVIDIA module for kernel with available headers: %s\n' "$target_kernel"
	dkms autoinstall -k "$target_kernel"
fi

nouveau_blacklist=/etc/modprobe.d/starryeyes-blacklist-nouveau.conf
temporary_file=$(mktemp)
cleanup() {
	rm -f -- "$temporary_file"
}
trap cleanup EXIT

cat >"$temporary_file" <<'EOF'
# The proprietary NVIDIA module is required for NVENC and NVIDIA Container Toolkit.
blacklist nouveau
options nouveau modeset=0
EOF

if [[ ! -f $nouveau_blacklist ]] || ! cmp -s "$temporary_file" "$nouveau_blacklist"; then
	install -o root -g root -m 0644 "$temporary_file" "$nouveau_blacklist"
fi

update-initramfs -u -k all

cat <<EOF
NVIDIA kernel module preparation completed.
The nouveau blacklist was written to $nouveau_blacklist.
Reboot before running setup-nvidia-container-toolkit.sh. If the running kernel
did not have matching headers, select the newly installed kernel at boot:

  sudo reboot
EOF
