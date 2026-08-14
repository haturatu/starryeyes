#!/usr/bin/env bash
set -euo pipefail

launcher=${1:?usage: $0 /path/to/sandbox-exec}
cat_bin=${CAT_BIN:-/usr/bin/cat}

if [[ ! -x "$launcher" ]]; then
  echo "sandbox launcher is not executable: $launcher" >&2
  exit 2
fi

# CPU-only workers must not gain sysfs access.
if "$launcher" --profile cpu --input /dev/null -- "$cat_bin" /sys/kernel/osrelease >/dev/null 2>&1; then
  echo "FAIL: CPU-only sandbox can read sysfs" >&2
  exit 1
fi
echo "PASS: CPU-only sandbox rejects sysfs reads"

gpu_device=${VAAPI_DEVICE:-}
if [[ -z "$gpu_device" ]]; then
  for candidate in /dev/dri/renderD*; do
    if [[ -c "$candidate" ]]; then
      gpu_device=$candidate
      break
    fi
  done
fi

if [[ -z "$gpu_device" || ! -c "$gpu_device" ]]; then
  echo "SKIP: no DRM render node available for GPU sysfs test"
  exit 0
fi

sysfs_vendor="/sys/class/drm/$(basename "$gpu_device")/device/vendor"
if ! "$launcher" --profile cpu --input /dev/null --gpu-device "$gpu_device" -- "$cat_bin" "$sysfs_vendor" >/dev/null 2>&1; then
  echo "FAIL: GPU sandbox cannot read $sysfs_vendor" >&2
  exit 1
fi
echo "PASS: GPU sandbox allows read-only GPU sysfs"
