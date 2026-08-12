#!/usr/bin/env bash
# Recursively submit supported video files to a Starryeyes server.
set -uo pipefail

usage() {
	cat <<'EOF'
Usage: upload-directory.sh <server-url> <directory>

Recursively finds supported video files and submits each one to Starryeyes.

Arguments:
  server-url  Starryeyes base URL, for example http://localhost:8080
  directory   Directory to scan recursively

Environment:
  OUTPUT_PRESET  Output preset to request (default: web-1080p)

Requires: bash 4+, curl, jq, sha256sum, GNU stat, and GNU dd.
EOF
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

is_video_file() {
	local name extension
	name=${1##*/}
	[[ $name == *.* ]] || return 1
	extension=${name##*.}
	case ${extension,,} in
		3gp|avi|flv|m2ts|m4v|mkv|mov|mp4|mpeg|mpg|mts|ts|webm|wmv) return 0 ;;
		*) return 1 ;;
	esac
}

request() {
	local method=$1 url=$2 data=$3 response=$4
	shift 4
	curl --silent --show-error --output "$response" --write-out '%{http_code}' \
		--request "$method" "$@" --data-binary "$data" "$url"
}

print_response() {
	local response=$1
	if [[ -s $response ]]; then
		printf ': '
		cat "$response"
		printf '\n'
	else
		printf '\n'
	fi
}

upload_file() {
	local file=$1 name size checksum payload response code job_id chunk_size
	local offset=0 chunk=0 length chunk_file chunk_checksum

	name=$(basename -- "$file")
	size=$(stat --format='%s' -- "$file") || {
		printf 'failed to read size for %q\n' "$file" >&2
		return 1
	}
	checksum=$(sha256sum -- "$file" | awk '{print $1}') || {
		printf 'failed to hash %q\n' "$file" >&2
		return 1
	}
	payload=$(jq --null-input --compact-output \
		--arg filename "$name" \
		--argjson size "$size" \
		--arg sha256 "$checksum" \
		--arg preset "$OUTPUT_PRESET" \
		'{input: {filename: $filename, size: $size, sha256: $sha256}, output: {preset: $preset}}') || return 1

	response="$work_dir/create-response.json"
	if ! code=$(request POST "$server_url/v1/jobs" "$payload" "$response" --header 'Content-Type: application/json'); then
		printf 'create request failed for %q\n' "$file" >&2
		return 1
	fi
	if [[ $code != 201 ]]; then
		printf 'create request returned HTTP %s for %q' "$code" "$file" >&2
		print_response "$response" >&2
		return 1
	fi
	job_id=$(jq --exit-status --raw-output '.id | strings | select(length > 0)' "$response") || {
		printf 'create response has no job id for %q\n' "$file" >&2
		return 1
	}
	chunk_size=$(jq --exit-status --raw-output '.upload.chunk_size | numbers | select(. > 0)' "$response") || {
		printf 'create response has no chunk size for %q\n' "$file" >&2
		return 1
	}

	printf 'uploading %q as job %s\n' "$file" "$job_id"
	while (( offset < size )); do
		length=$chunk_size
		if (( offset + length > size )); then
			length=$((size - offset))
		fi
		chunk_file="$work_dir/chunk-$chunk"
		if ! dd if="$file" of="$chunk_file" iflag=skip_bytes,count_bytes skip="$offset" count="$length" status=none; then
			printf 'failed to read chunk %d for %q\n' "$chunk" "$file" >&2
			return 1
		fi
		chunk_checksum=$(sha256sum -- "$chunk_file" | awk '{print $1}') || return 1
		response="$work_dir/chunk-response.json"
		if ! code=$(request PUT "$server_url/v1/jobs/$job_id/chunks/$chunk" "@$chunk_file" "$response" \
			--header "X-Chunk-SHA256: $chunk_checksum" \
			--header "Content-Length: $length"); then
			printf 'chunk upload failed for %q (chunk %d)\n' "$file" "$chunk" >&2
			return 1
		fi
		if [[ $code != 200 ]]; then
			printf 'chunk upload returned HTTP %s for %q (chunk %d)' "$code" "$file" "$chunk" >&2
			print_response "$response" >&2
			return 1
		fi
		printf '  uploaded chunk %d\n' "$chunk"
		rm -f -- "$chunk_file"
		offset=$((offset + length))
		chunk=$((chunk + 1))
	done

	response="$work_dir/complete-response.json"
	if ! code=$(request POST "$server_url/v1/jobs/$job_id/complete" '' "$response" --header 'Content-Type: application/json'); then
		printf 'complete request failed for %q\n' "$file" >&2
		return 1
	fi
	if [[ $code != 202 ]]; then
		printf 'complete request returned HTTP %s for %q' "$code" "$file" >&2
		print_response "$response" >&2
		return 1
	fi
	printf 'submitted %q (job %s)\n' "$file" "$job_id"
}

[[ $# -eq 2 ]] || {
	usage >&2
	exit 2
}

server_url=${1%/}
source_dir=$2
OUTPUT_PRESET=${OUTPUT_PRESET:-web-1080p}

[[ $server_url =~ ^https?://[^/]+($|/) ]] || die "server URL must start with http:// or https://"
[[ -d $source_dir ]] || die "directory does not exist: $source_dir"
for command in curl dd find jq sha256sum stat; do
	require_command "$command"
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/starryeyes-upload.XXXXXX") || die "could not create temporary directory"
cleanup() {
	rm -rf -- "$work_dir"
}
trap cleanup EXIT HUP INT TERM

found=0
submitted=0
failed=0
while IFS= read -r -d '' file; do
	is_video_file "$file" || continue
	found=$((found + 1))
	if upload_file "$file"; then
		submitted=$((submitted + 1))
	else
		failed=$((failed + 1))
	fi
done < <(find "$source_dir" -type f -print0)

if (( found == 0 )); then
	printf 'no supported video files found under %q\n' "$source_dir" >&2
	exit 1
fi
printf 'summary: submitted=%d failed=%d\n' "$submitted" "$failed"
(( failed == 0 ))
