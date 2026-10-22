#!/bin/sh
# Install a prepared candidate only when its fixed sidecar is valid. Sidecars
# are parsed as data; this script never sources or evaluates them.

set -u

WORKDIR=${WORKDIR:-/app}
cd "$WORKDIR" || exit 1

PROGRAM=./dockerCopilot
CANDIDATE=./dockerCopilot-new
MARKER=.dockerCopilot-handoff
RESULT=.dockerCopilot-handoff-result
READY_TIMEOUT=${DOCKERCOPILOT_READY_TIMEOUT:-60}
case "$READY_TIMEOUT" in
	''|*[!0-9]*) echo "invalid DOCKERCOPILOT_READY_TIMEOUT" >&2; exit 1 ;;
esac
[ "${#READY_TIMEOUT}" -le 3 ] || { echo "DOCKERCOPILOT_READY_TIMEOUT must be between 1 and 300 seconds" >&2; exit 1; }
[ "$READY_TIMEOUT" -ge 1 ] && [ "$READY_TIMEOUT" -le 300 ] || { echo "DOCKERCOPILOT_READY_TIMEOUT must be between 1 and 300 seconds" >&2; exit 1; }

valid_uuid() {
	printf '%s\n' "$1" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
}

valid_version() {
	printf '%s\n' "$1" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?|\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}

valid_sha256() {
	printf '%s\n' "$1" | grep -Eq '^[0-9a-fA-F]{64}$'
}

read_sidecar() {
	file=$1
	mode=$2
	[ -f "$file" ] && [ ! -L "$file" ] || return 1
	[ "$(wc -c < "$file" | tr -d ' ')" -le 4096 ] || return 1
	# Initialize every accepted field; reject duplicate, unknown, malformed or
	# missing fields. A field value can never contain shell metacharacter syntax.
	schema= task_id= handoff_id= target_version= goarch= binary_sha256= result= error_code=
	seen=' '
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in *=*) ;; *) return 1 ;; esac
		key=${line%%=*}
		value=${line#*=}
		case "$value" in *=*) return 1 ;; esac
		case "$seen" in *" $key "*) return 1 ;; esac
		seen="$seen$key "
		case "$key" in
			schema) schema=$value ;;
			task_id) task_id=$value ;;
			handoff_id) handoff_id=$value ;;
			target_version) target_version=$value ;;
			goarch) goarch=$value ;;
			binary_sha256) binary_sha256=$value ;;
			result)
				[ "$mode" = result ] || return 1
				result=$value
				;;
			error_code)
				[ "$mode" = result ] || return 1
				error_code=$value
				;;
			*) return 1 ;;
		esac
	done < "$file"
	[ "$schema" = 1 ] && valid_uuid "$task_id" && valid_uuid "$handoff_id" && valid_version "$target_version" && valid_sha256 "$binary_sha256" || return 1
	case "$goarch" in amd64|arm64) ;; *) return 1 ;; esac
	if [ "$mode" = result ]; then
		case "$result" in ready) [ -z "$error_code" ] || return 1 ;;
			failed-start) printf '%s\n' "$error_code" | grep -Eq '^[a-z0-9][a-z0-9_-]{0,63}$' || return 1 ;;
			*) return 1 ;;
		esac
		[ "$seen" = ' schema task_id handoff_id target_version goarch binary_sha256 result error_code ' ] || return 1
	else
		[ "$seen" = ' schema task_id handoff_id target_version goarch binary_sha256 ' ] || return 1
	fi
	return 0
}

write_failed_result() {
	code=$1
	tmp="$RESULT.tmp.$$"
	{
		printf 'schema=1\ntask_id=%s\nhandoff_id=%s\ntarget_version=%s\ngoarch=%s\nbinary_sha256=%s\nresult=failed-start\nerror_code=%s\n' \
			"$expected_task_id" "$expected_handoff_id" "$expected_version" "$expected_goarch" "$expected_sha256" "$code"
	} > "$tmp" || return 1
	chmod 600 "$tmp" && mv -f "$tmp" "$RESULT"
}

restore_previous() {
	if [ -f "$backup" ] && [ ! -L "$backup" ]; then
		restore_tmp="${PROGRAM}.restore.$$"
		if cp -p "$backup" "$restore_tmp" && chmod +x "$restore_tmp" && mv -f "$restore_tmp" "$PROGRAM"; then
			rm -f "$backup"
			return 0
		fi
		rm -f "$restore_tmp"
		return 1
	fi
	return 1
}

stop_candidate_bounded() {
	if [ "${shutdown_signal:-TERM}" = INT ]; then
		kill -INT "$child" 2>/dev/null || true
	else
		kill -TERM "$child" 2>/dev/null || true
	fi
	stop_attempt=0
	while kill -0 "$child" 2>/dev/null && [ "$stop_attempt" -lt 5 ]; do
		sleep 1
		stop_attempt=$((stop_attempt + 1))
	done
	if kill -0 "$child" 2>/dev/null; then
		kill -KILL "$child" 2>/dev/null || true
	fi
	wait "$child" 2>/dev/null || true
}

wait_foreground_child() {
	shutdown_status=0
	shutdown_signal=
	trap 'shutdown_status=143; shutdown_signal=TERM; kill -TERM "$child" 2>/dev/null || true' TERM
	trap 'shutdown_status=130; shutdown_signal=INT; kill -INT "$child" 2>/dev/null || true' INT
	while :; do
		wait "$child"
		status=$?
		if ! kill -0 "$child" 2>/dev/null; then break; fi
	done
	trap - TERM INT
	if [ "$shutdown_status" -ne 0 ]; then status=$shutdown_status; fi
	return "$status"
}

install_candidate() {
	failure_code=installation_failed
	[ -f "$PROGRAM" ] && [ ! -L "$PROGRAM" ] || { failure_code=current_program_missing; return 1; }
	[ -f "$CANDIDATE" ] && [ ! -L "$CANDIDATE" ] || { failure_code=candidate_missing; return 1; }
	[ "$(uname -m)" = "$goarch" ] || {
		case "$(uname -m):$goarch" in x86_64:amd64|aarch64:arm64) ;; *) failure_code=architecture_mismatch; return 1 ;; esac
	}
	actual_hash=$(sha256sum "$CANDIDATE" 2>/dev/null | awk '{print $1}') || { failure_code=digest_unavailable; return 1; }
	[ "$(printf '%s' "$actual_hash" | tr 'A-F' 'a-f')" = "$(printf '%s' "$binary_sha256" | tr 'A-F' 'a-f')" ] || { failure_code=digest_mismatch; return 1; }
	backup=".dockerCopilot-previous-$handoff_id"
	[ ! -e "$backup" ] || { failure_code=backup_exists; return 1; }
	# Discard stale results before the new process starts, so an earlier ready
	# file cannot acknowledge this handoff.
	rm -f "$RESULT" || return 1
	if ! mv "$PROGRAM" "$backup"; then return 1; fi
	if ! mv "$CANDIDATE" "$PROGRAM"; then
		mv "$backup" "$PROGRAM" || { failure_code=rollback_failed; return 1; }
		return 1
	fi
	chmod +x "$PROGRAM" || {
		restore_previous || { failure_code=rollback_failed; return 1; }
		return 1
	}

	DOCKERCOPILOT_HANDOFF_PROTOCOL=1 "$PROGRAM" &
	child=$!
	shutdown_status=0
	shutdown_signal=
	trap 'shutdown_status=143; shutdown_signal=TERM; kill -TERM "$child" 2>/dev/null || true' TERM
	trap 'shutdown_status=130; shutdown_signal=INT; kill -INT "$child" 2>/dev/null || true' INT
	started_at=$(date +%s)
	while :; do
		if [ "$shutdown_status" -ne 0 ]; then
			stop_candidate_bounded
			# A container stop is not a failed upgrade. Put the candidate back so
			# the next normal start can retry the same verified handoff.
			if mv "$PROGRAM" "$CANDIDATE" && restore_previous; then
				exit "$shutdown_status"
			fi
			echo "could not restore program while stopping for a signal" >&2
			exit 1
		fi
		if ! kill -0 "$child" 2>/dev/null; then
			wait "$child" 2>/dev/null || true
			restore_previous || { failure_code=rollback_failed; return 1; }
			write_failed_result candidate_exit || return 1
			return 0
		fi
		# read_sidecar loads globals; save marker identity and compare every field.
		if [ -f "$RESULT" ] && read_sidecar "$RESULT" result; then
			if [ "$result" = ready ] && [ "$task_id" = "$expected_task_id" ] &&
				[ "$handoff_id" = "$expected_handoff_id" ] && [ "$target_version" = "$expected_version" ] &&
				[ "$goarch" = "$expected_goarch" ] && [ "$(printf '%s' "$binary_sha256" | tr 'A-F' 'a-f')" = "$expected_sha256" ]; then
				handoff_started=1
				return 2
			fi
			if [ "$result" = failed-start ] && [ "$task_id" = "$expected_task_id" ] && [ "$handoff_id" = "$expected_handoff_id" ]; then
				if [ "$target_version" = "$expected_version" ] && [ "$goarch" = "$expected_goarch" ] &&
					[ "$(printf '%s' "$binary_sha256" | tr 'A-F' 'a-f')" = "$expected_sha256" ]; then
					failure_code=$error_code
					stop_candidate_bounded
					restore_previous || { failure_code=rollback_failed; return 1; }
					write_failed_result "$failure_code" || return 1
					return 0
				fi
			fi
		fi
		now=$(date +%s)
		if [ "$((now - started_at))" -ge "$READY_TIMEOUT" ]; then
			stop_candidate_bounded
			restore_previous || { failure_code=rollback_failed; return 1; }
			write_failed_result readiness_timeout || return 1
			return 0
		fi
		sleep 1
	done
}

if [ -f "$MARKER" ]; then
	if read_sidecar "$MARKER" marker; then
		# Capture identity before reading the result, whose parser also uses globals.
		expected_task_id=$task_id
		expected_handoff_id=$handoff_id
		expected_version=$target_version
		expected_goarch=$goarch
		expected_sha256=$(printf '%s' "$binary_sha256" | tr 'A-F' 'a-f')
		already_ready=0
		if [ -f "$RESULT" ] && read_sidecar "$RESULT" result && [ "$result" = ready ] &&
			[ "$task_id" = "$expected_task_id" ] && [ "$handoff_id" = "$expected_handoff_id" ] &&
			[ "$target_version" = "$expected_version" ] && [ "$goarch" = "$expected_goarch" ] &&
			[ "$(printf '%s' "$binary_sha256" | tr 'A-F' 'a-f')" = "$expected_sha256" ] &&
			[ -f "$PROGRAM" ] && [ ! -L "$PROGRAM" ]; then
			current_hash=$(sha256sum "$PROGRAM" 2>/dev/null | awk '{print $1}') || current_hash=
			[ "$(printf '%s' "$current_hash" | tr 'A-F' 'a-f')" = "$expected_sha256" ] && already_ready=1
		fi
		already_failed=0
		if [ "$already_ready" -eq 0 ] && [ -f "$RESULT" ] && read_sidecar "$RESULT" result && [ "$result" = failed-start ] &&
			[ "$task_id" = "$expected_task_id" ] && [ "$handoff_id" = "$expected_handoff_id" ] &&
			[ "$target_version" = "$expected_version" ] && [ "$goarch" = "$expected_goarch" ] &&
			[ "$(printf '%s' "$binary_sha256" | tr 'A-F' 'a-f')" = "$expected_sha256" ]; then
			already_failed=1
		fi
		task_id=$expected_task_id
		handoff_id=$expected_handoff_id
		target_version=$expected_version
		goarch=$expected_goarch
		binary_sha256=$expected_sha256
		handoff_started=0
		if [ "$already_ready" -eq 1 ] || [ "$already_failed" -eq 1 ]; then
			install_status=0
		else
			install_candidate
			install_status=$?
		fi
		if [ "$install_status" -eq 2 ]; then
			wait_foreground_child
			status=$?
			exit "$status"
		elif [ "$install_status" -ne 0 ]; then
			if ! write_failed_result "${failure_code:-installation_failed}"; then
				echo "could not record failed program handoff" >&2
			fi
			[ "${failure_code:-}" != rollback_failed ] || {
				echo "program handoff rollback failed; refusing to start an unknown binary" >&2
				exit 1
			}
			# Do not retry a malformed or mismatched candidate on every restart.
			[ -f "$CANDIDATE" ] && mv "$CANDIDATE" "${CANDIDATE}.rejected-$expected_handoff_id" 2>/dev/null || true
		fi
	fi
fi

if [ ! -x "$PROGRAM" ]; then
	echo "dockerCopilot executable is unavailable" >&2
	exit 1
fi

# Once ready, preserve normal container signal/exit behavior by keeping the
# service as the foreground child and forwarding termination signals.
	DOCKERCOPILOT_HANDOFF_PROTOCOL=1 "$PROGRAM" &
child=$!
wait_foreground_child
status=$?
exit "$status"
