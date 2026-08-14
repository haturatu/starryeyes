#!/usr/bin/env bash
# Recursively and resumably submit supported video files to Starryeyes.
set -uo pipefail
umask 077

usage() {
	cat <<'EOF'
Usage: upload-directory.sh <server-url> <directory>

Recursively finds supported video files and resumes or submits each one.

Arguments:
  server-url  Starryeyes base URL, for example http://localhost:8080
  directory   Directory to scan recursively

Environment:
  OUTPUT_PRESET             Output preset (default: web-1080p)
  UPLOAD_PARALLELISM        Concurrent chunk uploads per file (default: 4)
  ADMISSION_POLL_SECONDS    Capacity-admission poll interval (default: 10)
  JOB_POLL_SECONDS          Processing-status poll interval (default: 10)
  STARRYEYES_STATE_DIR      Persistent upload state directory
                            (default: $XDG_STATE_HOME/starryeyes/uploads or
                            $HOME/.local/state/starryeyes/uploads)

	Requires: Bash 4+, curl, flock, jq, sha256sum, GNU dd/stat/realpath/sync,
	          and find.
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
	local -a arguments=(
		--silent --show-error --output "$response" --write-out '%{http_code}'
		--retry 3 --retry-delay 1 --retry-all-errors --request "$method"
	)
	if [[ -n $data ]]; then
		arguments+=(--data-binary "$data")
	fi
	curl "${arguments[@]}" "$@" "$url"
}

print_response() {
	local response=$1
	if [[ -s $response ]]; then
		printf ': '
		tr '\n' ' ' <"$response"
		printf '\n'
	else
		printf '\n'
	fi
}

new_idempotency_key() {
	if [[ -r /proc/sys/kernel/random/uuid ]]; then
		tr -d '\n' </proc/sys/kernel/random/uuid
	elif command -v uuidgen >/dev/null 2>&1; then
		uuidgen
	else
		printf '%s:%s:%s:%s\n' "$(date +%s%N)" "$$" "$RANDOM" "$RANDOM" | sha256sum | awk '{print $1}'
	fi
}

write_state() {
	local state_file=$1 file=$2 filename=$3 size=$4 checksum=$5 idempotency_key=$6 job_id=$7
	local temporary
	temporary=$(mktemp "$state_dir/.upload-state.XXXXXX") || return 1
	chmod 0600 "$temporary" || {
		rm -f -- "$temporary"
		return 1
	}
	if ! jq --null-input \
		--arg server "$server_url" \
		--arg job_id "$job_id" \
		--arg idempotency_key "$idempotency_key" \
		--arg path "$file" \
		--arg filename "$filename" \
		--argjson size "$size" \
		--arg sha256 "$checksum" \
		--arg preset "$OUTPUT_PRESET" \
		'{version: 1, server: $server, job_id: (if $job_id == "" then null else $job_id end), idempotency_key: $idempotency_key, path: $path, filename: $filename, size: $size, sha256: $sha256, output: {preset: $preset, video: {quality: {mode: "crf", crf: 18}}}}' \
		>"$temporary"; then
		rm -f -- "$temporary"
		return 1
	fi
	sync -f "$temporary" || {
		rm -f -- "$temporary"
		return 1
	}
	mv -f -- "$temporary" "$state_file" || return 1
	sync -f "$state_dir"
}

load_or_create_state() {
	local state_file=$1 file=$2 filename=$3 size=$4 checksum=$5
	local idempotency_key job_id
	if [[ -e $state_file ]]; then
		if ! jq --exit-status \
			--arg server "$server_url" --arg path "$file" --arg filename "$filename" \
			--argjson size "$size" --arg sha256 "$checksum" --arg preset "$OUTPUT_PRESET" \
			'.version == 1 and .server == $server and .path == $path and .filename == $filename and .size == $size and .sha256 == $sha256 and .output.preset == $preset and .output.video.quality.mode == "crf" and .output.video.quality.crf == 18 and (.idempotency_key | type == "string" and length >= 16)' \
			"$state_file" >/dev/null; then
			printf 'invalid upload state %q; inspect or remove it before retrying\n' "$state_file" >&2
			return 1
		fi
		return 0
	fi
	idempotency_key=$(new_idempotency_key) || return 1
	job_id=""
	write_state "$state_file" "$file" "$filename" "$size" "$checksum" "$idempotency_key" "$job_id"
}

create_or_discover_job() {
	local file=$1 filename=$2 size=$3 checksum=$4 state_file=$5
	local idempotency_key payload response code job_id
	idempotency_key=$(jq --exit-status --raw-output '.idempotency_key' "$state_file") || return 1
	payload=$(jq --null-input --compact-output \
		--arg filename "$filename" --argjson size "$size" --arg sha256 "$checksum" --arg preset "$OUTPUT_PRESET" \
		'{input: {filename: $filename, size: $size, sha256: $sha256}, output: {preset: $preset, video: {quality: {mode: "crf", crf: 18}}}}') || return 1
	response="$work_dir/create-response.json"
	if ! code=$(request POST "$server_url/v1/jobs" "$payload" "$response" \
		--header 'Content-Type: application/json' --header "Idempotency-Key: $idempotency_key"); then
		printf 'create/discovery request failed for %q; persistent state kept at %s\n' "$file" "$state_file" >&2
		return 1
	fi
	if [[ $code != 201 ]]; then
		printf 'create/discovery request returned HTTP %s for %q' "$code" "$file" >&2
		print_response "$response" >&2
		return 1
	fi
	job_id=$(jq --exit-status --raw-output '.id | strings | select(length > 0)' "$response") || {
		printf 'create response has no job id for %q\n' "$file" >&2
		return 1
	}
	write_state "$state_file" "$file" "$filename" "$size" "$checksum" "$idempotency_key" "$job_id" || return 1
	printf '%s\n' "$job_id"
}

get_job() {
	local job_id=$1 response=$2 code
	if ! code=$(request GET "$server_url/v1/jobs/$job_id" '' "$response"); then
		return 1
	fi
	[[ $code == 200 ]]
}

upload_one_chunk() {
	local file=$1 job_id=$2 number=$3 chunk_size=$4 total_size=$5
	local offset length chunk_file checksum response code
	offset=$((number * chunk_size))
	length=$chunk_size
	if (( offset + length > total_size )); then
		length=$((total_size - offset))
	fi
	chunk_file="$work_dir/chunk-$number-$BASHPID"
	if ! dd if="$file" of="$chunk_file" iflag=skip_bytes,count_bytes skip="$offset" count="$length" status=none; then
		printf 'failed to read chunk %d for %q\n' "$number" "$file" >&2
		return 1
	fi
	checksum=$(sha256sum -- "$chunk_file" | awk '{print $1}') || {
		rm -f -- "$chunk_file"
		return 1
	}
	response="$work_dir/chunk-response-$number-$BASHPID.json"
	if ! code=$(request PUT "$server_url/v1/jobs/$job_id/chunks/$number" "@$chunk_file" "$response" \
		--header 'Content-Type: application/octet-stream' \
		--header "X-Chunk-SHA256: $checksum" \
		--header "Content-Length: $length"); then
		printf 'chunk upload failed for %q (chunk %d)\n' "$file" "$number" >&2
		rm -f -- "$chunk_file"
		return 1
	fi
	rm -f -- "$chunk_file"
	if [[ $code != 200 ]]; then
		printf 'chunk upload returned HTTP %s for %q (chunk %d)' "$code" "$file" "$number" >&2
		print_response "$response" >&2
		return 1
	fi
	printf '  verified chunk %d\n' "$number"
}

upload_missing_chunks() {
	local file=$1 job_id=$2 total_size=$3
	local response code chunk_size expected server_count number failed pid
	response="$work_dir/chunks-response.json"
	if ! code=$(request GET "$server_url/v1/jobs/$job_id/chunks" '' "$response"); then
		printf 'verified-chunk request failed for %q\n' "$file" >&2
		return 1
	fi
	if [[ $code != 200 ]]; then
		printf 'verified-chunk request returned HTTP %s for %q' "$code" "$file" >&2
		print_response "$response" >&2
		return 1
	fi
	chunk_size=$(jq --exit-status --raw-output '.chunk_size | numbers | select(. > 0)' "$response") || return 1
	expected=$(jq --exit-status --raw-output '.expected | numbers | select(. > 0)' "$response") || return 1
	if (( expected != (total_size + chunk_size - 1) / chunk_size )); then
		printf 'server chunk geometry does not match %q\n' "$file" >&2
		return 1
	fi
	declare -A verified=()
	while IFS= read -r number; do
		[[ $number =~ ^[0-9]+$ ]] || return 1
		verified[$number]=1
	done < <(jq --raw-output '.chunks[].number' "$response")
	server_count=${#verified[@]}
	printf 'resuming %q as job %s: %d/%d chunks already verified\n' "$file" "$job_id" "$server_count" "$expected"

	local -a pids=()
	failed=0
	for ((number = 0; number < expected; number++)); do
		if [[ -n ${verified[$number]+present} ]]; then
			continue
		fi
		upload_one_chunk "$file" "$job_id" "$number" "$chunk_size" "$total_size" &
		pids+=("$!")
		if (( ${#pids[@]} >= UPLOAD_PARALLELISM )); then
			for pid in "${pids[@]}"; do
				wait "$pid" || failed=1
			done
			pids=()
			(( failed == 0 )) || return 1
		fi
	done
	for pid in "${pids[@]}"; do
		wait "$pid" || failed=1
	done
	(( failed == 0 ))
}

complete_job() {
	local file=$1 job_id=$2 response code
	response="$work_dir/complete-response.json"
	if ! code=$(request POST "$server_url/v1/jobs/$job_id/complete" '' "$response" --header 'Content-Type: application/json'); then
		printf 'complete request failed for %q; rerun to resume safely\n' "$file" >&2
		return 1
	fi
	if [[ $code == 202 ]]; then
		return 0
	fi
	if [[ $code == 409 ]]; then
		printf 'server reports upload incomplete for %q; refreshing verified chunks\n' "$file" >&2
		return 3
	fi
	printf 'complete request returned HTTP %s for %q' "$code" "$file" >&2
	print_response "$response" >&2
	return 1
}

resume_workflow() {
	local file=$1 job_id=$2 state_file=$3 size=$4
	local response state error complete_status
	response="$work_dir/job-response.json"
	while :; do
		if ! get_job "$job_id" "$response"; then
			printf 'job status request failed for %q (job %s)\n' "$file" "$job_id" >&2
			return 1
		fi
		state=$(jq --exit-status --raw-output '.state | strings | select(length > 0)' "$response") || return 1
		case $state in
			PENDING)
				printf 'waiting for spool capacity for %q (job %s)\n' "$file" "$job_id"
				sleep "$ADMISSION_POLL_SECONDS"
				;;
			ADMITTED|UPLOADING)
				upload_missing_chunks "$file" "$job_id" "$size" || return 1
				complete_job "$file" "$job_id"
				complete_status=$?
				if (( complete_status == 1 )); then
					return 1
				fi
				if (( complete_status == 3 )); then
					sleep 1
				fi
				;;
			FINALIZING|STAGED|PROBING|VALIDATED|QUEUED|STARTING|TRANSCODING)
				printf 'waiting for job %s: %s\n' "$job_id" "$state"
				sleep "$JOB_POLL_SECONDS"
				;;
			COMPLETED)
				printf 'already completed %q (job %s); skipping upload\n' "$file" "$job_id"
				return 0
				;;
			EXPIRED)
				error=$(jq --raw-output '.error // "upload expired"' "$response")
				rm -f -- "$state_file"
				printf 'job %s expired for %q: %s; state removed so the next run can create a new workflow\n' "$job_id" "$file" "$error" >&2
				return 1
				;;
			FAILED)
				error=$(jq --raw-output '.error // "job failed"' "$response")
				printf 'job %s failed for %q: %s (state retained at %s)\n' "$job_id" "$file" "$error" "$state_file" >&2
				return 1
				;;
			*)
				printf 'job %s has unknown state %q for %q\n' "$job_id" "$state" "$file" >&2
				return 1
				;;
		esac
	done
}

upload_file() (
	local original=$1 file filename size checksum session_id state_file job_id lock_fd
	file=$(realpath -- "$original") || return 1
	filename=$(basename -- "$file")
	size=$(stat --format='%s' -- "$file") || return 1
	checksum=$(sha256sum -- "$file" | awk '{print $1}') || return 1
	session_id=$(printf '%s\0%s\0%s\0%s\0%s\0%s\0%s' "$server_url" "$file" "$size" "$checksum" "$OUTPUT_PRESET" crf 18 | sha256sum | awk '{print $1}') || return 1
	state_file="$state_dir/$session_id.json"
	exec {lock_fd}>"$state_file.lock" || return 1
	flock "$lock_fd" || return 1
	load_or_create_state "$state_file" "$file" "$filename" "$size" "$checksum" || return 1
	job_id=$(create_or_discover_job "$file" "$filename" "$size" "$checksum" "$state_file") || return 1
	resume_workflow "$file" "$job_id" "$state_file" "$size"
)

[[ $# -eq 2 ]] || {
	usage >&2
	exit 2
}

server_url=${1%/}
source_dir=$2
OUTPUT_PRESET=${OUTPUT_PRESET:-web-1080p}
UPLOAD_PARALLELISM=${UPLOAD_PARALLELISM:-4}
ADMISSION_POLL_SECONDS=${ADMISSION_POLL_SECONDS:-10}
JOB_POLL_SECONDS=${JOB_POLL_SECONDS:-10}

[[ $server_url =~ ^https?://[^/]+($|/) ]] || die "server URL must start with http:// or https://"
[[ -d $source_dir ]] || die "directory does not exist: $source_dir"
[[ $UPLOAD_PARALLELISM =~ ^[1-9][0-9]*$ ]] || die "UPLOAD_PARALLELISM must be a positive integer"
[[ $ADMISSION_POLL_SECONDS =~ ^[1-9][0-9]*$ ]] || die "ADMISSION_POLL_SECONDS must be a positive integer"
[[ $JOB_POLL_SECONDS =~ ^[1-9][0-9]*$ ]] || die "JOB_POLL_SECONDS must be a positive integer"
for command in curl dd find flock jq realpath sha256sum stat sync; do
	require_command "$command"
done

if [[ -n ${STARRYEYES_STATE_DIR:-} ]]; then
	state_dir=$STARRYEYES_STATE_DIR
elif [[ -n ${XDG_STATE_HOME:-} ]]; then
	state_dir=$XDG_STATE_HOME/starryeyes/uploads
elif [[ -n ${HOME:-} ]]; then
	state_dir=$HOME/.local/state/starryeyes/uploads
else
	die "STARRYEYES_STATE_DIR is required when neither XDG_STATE_HOME nor HOME is set"
fi
mkdir -p -- "$state_dir" || die "could not create state directory: $state_dir"
chmod 0700 "$state_dir" || die "could not secure state directory: $state_dir"

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
printf 'summary: completed=%d failed=%d\n' "$submitted" "$failed"
(( failed == 0 ))
