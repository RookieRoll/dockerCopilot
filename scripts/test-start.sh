#!/bin/sh
# Isolated fixture test for start.sh. It uses shell programs and temporary
# directories only; no service binary, Docker daemon or persistent data path.
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
start_script=$repo_dir/start.sh
temporary_root=$(mktemp -d)
trap 'rm -rf "$temporary_root"' EXIT HUP INT TERM

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) echo "unsupported fixture host architecture: $(uname -m)" >&2; exit 1 ;;
esac

task_id=123e4567-e89b-12d3-a456-426614174000
handoff_id=123e4567-e89b-12d3-a456-426614174001

write_marker() {
  directory=$1
  digest=$2
  cat > "$directory/.dockerCopilot-handoff" <<EOF
schema=1
task_id=$task_id
handoff_id=$handoff_id
target_version=2.2.2
goarch=$arch
binary_sha256=$digest
EOF
}

run_start() {
  directory=$1
  timeout=${2:-5}
  WORKDIR=$directory DOCKERCOPILOT_READY_TIMEOUT=$timeout sh "$start_script"
}

# Success fixture: fake candidate publishes a matching ready result.
success_dir=$temporary_root/success
mkdir "$success_dir"
cat > "$success_dir/dockerCopilot" <<'EOF'
#!/bin/sh
test "$DOCKERCOPILOT_HANDOFF_PROTOCOL" = 1 || exit 31
echo old-program
EOF
cat > "$success_dir/dockerCopilot-new" <<'EOF'
#!/bin/sh
test "$DOCKERCOPILOT_HANDOFF_PROTOCOL" = 1 || exit 32
dir=$(dirname "$0")
field() { sed -n "s/^$1=//p" "$dir/.dockerCopilot-handoff"; }
{
  echo schema=1
  echo task_id=$(field task_id)
  echo handoff_id=$(field handoff_id)
  echo target_version=$(field target_version)
  echo goarch=$(field goarch)
  echo binary_sha256=$(field binary_sha256)
  echo result=ready
  echo error_code=
} > "$dir/.dockerCopilot-handoff-result.tmp"
mv "$dir/.dockerCopilot-handoff-result.tmp" "$dir/.dockerCopilot-handoff-result"
while [ ! -e "$dir/finish-candidate" ]; do sleep 0.1; done
echo new-program
sleep 2
EOF
chmod +x "$success_dir/dockerCopilot" "$success_dir/dockerCopilot-new"
success_digest=$(sha256sum "$success_dir/dockerCopilot-new" | awk '{print $1}')
write_marker "$success_dir" "$success_digest"
run_start "$success_dir" > "$success_dir/output" &
success_launcher=$!
success_ready=0
for attempt in $(seq 1 100); do
  if grep -q '^result=ready$' "$success_dir/.dockerCopilot-handoff-result" 2>/dev/null; then success_ready=1; break; fi
  sleep 0.1
done
[ "$success_ready" -eq 1 ]
touch "$success_dir/finish-candidate"
wait "$success_launcher"
grep -q new-program "$success_dir/output"
[ "$(sha256sum "$success_dir/dockerCopilot" | awk '{print $1}')" = "$success_digest" ]
[ -f "$success_dir/.dockerCopilot-previous-$handoff_id" ]
run_start "$success_dir" > "$success_dir/restart-output"
grep -q new-program "$success_dir/restart-output"
grep -q '^result=ready$' "$success_dir/.dockerCopilot-handoff-result"

# Early exit fixture: old binary is restored and the failure is recorded.
failure_dir=$temporary_root/failure
mkdir "$failure_dir"
cat > "$failure_dir/dockerCopilot" <<'EOF'
#!/bin/sh
echo old-program
EOF
cat > "$failure_dir/dockerCopilot-new" <<'EOF'
#!/bin/sh
exit 7
EOF
chmod +x "$failure_dir/dockerCopilot" "$failure_dir/dockerCopilot-new"
failure_digest=$(sha256sum "$failure_dir/dockerCopilot-new" | awk '{print $1}')
write_marker "$failure_dir" "$failure_digest"
run_start "$failure_dir" > "$failure_dir/output"
grep -q old-program "$failure_dir/output"
grep -q old-program "$failure_dir/dockerCopilot"
grep -q '^result=failed-start$' "$failure_dir/.dockerCopilot-handoff-result"
grep -q '^error_code=candidate_exit$' "$failure_dir/.dockerCopilot-handoff-result"
[ ! -e "$failure_dir/dockerCopilot-new" ]
run_start "$failure_dir" > "$failure_dir/restart-output"
grep -q '^error_code=candidate_exit$' "$failure_dir/.dockerCopilot-handoff-result"

# A stale ready result must not bypass digest validation.
invalid_dir=$temporary_root/invalid
mkdir "$invalid_dir"
cat > "$invalid_dir/dockerCopilot" <<'EOF'
#!/bin/sh
echo old-program
EOF
cat > "$invalid_dir/dockerCopilot-new" <<'EOF'
#!/bin/sh
echo unverified-candidate
EOF
chmod +x "$invalid_dir/dockerCopilot" "$invalid_dir/dockerCopilot-new"
zero_digest=0000000000000000000000000000000000000000000000000000000000000000
write_marker "$invalid_dir" "$zero_digest"
cat > "$invalid_dir/.dockerCopilot-handoff-result" <<EOF
schema=1
task_id=$task_id
handoff_id=$handoff_id
target_version=2.2.2
goarch=$arch
binary_sha256=$zero_digest
result=ready
error_code=
EOF
run_start "$invalid_dir" > "$invalid_dir/output"
grep -q old-program "$invalid_dir/output"
grep -q '^result=failed-start$' "$invalid_dir/.dockerCopilot-handoff-result"
grep -q '^error_code=digest_mismatch$' "$invalid_dir/.dockerCopilot-handoff-result"
[ ! -e "$invalid_dir/dockerCopilot-new" ]

# A same-task ready result with a different digest cannot bypass verification.
different_digest_dir=$temporary_root/different-digest
mkdir "$different_digest_dir"
printf '#!/bin/sh\necho old-program\n' > "$different_digest_dir/dockerCopilot"
chmod +x "$different_digest_dir/dockerCopilot"
expected_digest=$(sha256sum "$different_digest_dir/dockerCopilot" | awk '{print $1}')
write_marker "$different_digest_dir" "$expected_digest"
cat > "$different_digest_dir/.dockerCopilot-handoff-result" <<EOF
schema=1
task_id=$task_id
handoff_id=$handoff_id
target_version=2.2.2
goarch=$arch
binary_sha256=0000000000000000000000000000000000000000000000000000000000000000
result=ready
error_code=
EOF
run_start "$different_digest_dir" > "$different_digest_dir/output"
grep -q '^result=failed-start$' "$different_digest_dir/.dockerCopilot-handoff-result"
grep -q '^error_code=candidate_missing$' "$different_digest_dir/.dockerCopilot-handoff-result"

# A same-task failed result with the wrong binary digest is ignored, but it
# must not bypass the bounded readiness deadline.
wrong_failure_dir=$temporary_root/wrong-failure-digest
mkdir "$wrong_failure_dir"
printf '#!/bin/sh\necho old-program\n' > "$wrong_failure_dir/dockerCopilot"
cat > "$wrong_failure_dir/dockerCopilot-new" <<'EOF'
#!/bin/sh
dir=$(dirname "$0")
trap 'exit 0' TERM INT
cat > "$dir/.dockerCopilot-handoff-result.tmp" <<RESULT
schema=1
task_id=123e4567-e89b-12d3-a456-426614174000
handoff_id=123e4567-e89b-12d3-a456-426614174001
target_version=2.2.2
goarch=__ARCH__
binary_sha256=0000000000000000000000000000000000000000000000000000000000000000
result=failed-start
error_code=candidate_exit
RESULT
sed "s/__ARCH__/$arch/" "$dir/.dockerCopilot-handoff-result.tmp" > "$dir/.dockerCopilot-handoff-result.next"
mv "$dir/.dockerCopilot-handoff-result.next" "$dir/.dockerCopilot-handoff-result"
while :; do sleep 0.1; done
EOF
sed -i "s/__ARCH__/$arch/" "$wrong_failure_dir/dockerCopilot-new"
chmod +x "$wrong_failure_dir/dockerCopilot" "$wrong_failure_dir/dockerCopilot-new"
wrong_failure_expected=$(sha256sum "$wrong_failure_dir/dockerCopilot-new" | awk '{print $1}')
write_marker "$wrong_failure_dir" "$wrong_failure_expected"
cat > "$wrong_failure_dir/.dockerCopilot-handoff-result" <<EOF
schema=1
task_id=$task_id
handoff_id=$handoff_id
target_version=2.2.2
goarch=$arch
binary_sha256=0000000000000000000000000000000000000000000000000000000000000000
result=failed-start
error_code=candidate_exit
EOF
run_start "$wrong_failure_dir" 1 > "$wrong_failure_dir/output"
grep -q '^result=failed-start$' "$wrong_failure_dir/.dockerCopilot-handoff-result"
grep -q '^error_code=readiness_timeout$' "$wrong_failure_dir/.dockerCopilot-handoff-result"
grep -q "^binary_sha256=$wrong_failure_expected$" "$wrong_failure_dir/.dockerCopilot-handoff-result"
[ -x "$wrong_failure_dir/dockerCopilot" ]
[ ! -e "$wrong_failure_dir/dockerCopilot-new" ]

# Force the candidate-to-program rename to fail after moving the old program;
# the original executable must be restored before the launcher continues.
replace_dir=$temporary_root/replace-failure
mkdir -p "$replace_dir/fake-bin"
printf '#!/bin/sh\necho old-program\n' > "$replace_dir/dockerCopilot"
printf '#!/bin/sh\necho new-program\n' > "$replace_dir/dockerCopilot-new"
chmod +x "$replace_dir/dockerCopilot" "$replace_dir/dockerCopilot-new"
replace_digest=$(sha256sum "$replace_dir/dockerCopilot-new" | awk '{print $1}')
write_marker "$replace_dir" "$replace_digest"
real_mv=$(command -v mv)
cat > "$replace_dir/fake-bin/mv" <<EOF
#!/bin/sh
if [ "\$1" = ./dockerCopilot-new ] && [ "\$2" = ./dockerCopilot ]; then exit 1; fi
exec "$real_mv" "\$@"
EOF
chmod +x "$replace_dir/fake-bin/mv"
PATH="$replace_dir/fake-bin:$PATH" WORKDIR="$replace_dir" DOCKERCOPILOT_READY_TIMEOUT=5 sh "$start_script" > "$replace_dir/output"
grep -q old-program "$replace_dir/output"
grep -q old-program "$replace_dir/dockerCopilot"
grep -q '^error_code=installation_failed$' "$replace_dir/.dockerCopilot-handoff-result"

# TERM reaches the foreground child; the launcher waits for its graceful exit.
signal_dir=$temporary_root/signal
mkdir "$signal_dir"
printf '#!/bin/sh\ntest "$DOCKERCOPILOT_HANDOFF_PROTOCOL" = 1 || exit 33\ndir=$(dirname "$0")\ntouch "$dir/started"\ntrap '\''touch "$dir/term-seen"; exit 0'\'' TERM\nwhile :; do sleep 0.1; done\n' > "$signal_dir/dockerCopilot"
chmod +x "$signal_dir/dockerCopilot"
WORKDIR="$signal_dir" sh "$start_script" > "$signal_dir/output" &
signal_launcher=$!
for attempt in $(seq 1 100); do
  [ -f "$signal_dir/started" ] && break
  sleep 0.1
done
[ -f "$signal_dir/started" ]
kill -TERM "$signal_launcher"
signal_status=0
wait "$signal_launcher" || signal_status=$?
[ "$signal_status" -eq 143 ]
[ -f "$signal_dir/term-seen" ]

# A candidate that ignores TERM is killed after the bounded grace period;
# the old executable is restored and the failure remains explicit. Git Bash
# emulates signals for Windows processes and cannot model POSIX SIGKILL here.
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) echo "skipping POSIX SIGKILL fixture under $(uname -s)" ;;
  *)
ignore_term_dir=$temporary_root/ignore-term
mkdir "$ignore_term_dir"
printf '#!/bin/sh\necho old-program\n' > "$ignore_term_dir/dockerCopilot"
cat > "$ignore_term_dir/dockerCopilot-new" <<'EOF'
#!/bin/sh
trap '' TERM
while :; do sleep 0.1; done
EOF
chmod +x "$ignore_term_dir/dockerCopilot" "$ignore_term_dir/dockerCopilot-new"
ignore_term_digest=$(sha256sum "$ignore_term_dir/dockerCopilot-new" | awk '{print $1}')
write_marker "$ignore_term_dir" "$ignore_term_digest"
run_start "$ignore_term_dir" 1 > "$ignore_term_dir/output"
grep -q old-program "$ignore_term_dir/output"
grep -q '^error_code=readiness_timeout$' "$ignore_term_dir/.dockerCopilot-handoff-result"
[ -x "$ignore_term_dir/dockerCopilot" ]
[ ! -e "$ignore_term_dir/dockerCopilot-new" ]
  ;;
esac

# Timeout values are bounded and never interpreted as shell code.
for invalid_timeout in 0 301 '1;touch-pwned'; do
  if WORKDIR="$success_dir" DOCKERCOPILOT_READY_TIMEOUT="$invalid_timeout" sh "$start_script" >/dev/null 2>&1; then
    echo "invalid readiness timeout was accepted: $invalid_timeout" >&2
    exit 1
  fi
done
[ ! -e "$success_dir/pwned" ]

# Release archive smoke check mirrors latest.yml's exact root-layout and
# checksum commands without running a Go cross-build or creating a release.
package_dir=$temporary_root/package/linux/amd64
mkdir -p "$package_dir"
printf 'fixture executable\n' > "$package_dir/dockerCopilot-new"
tar -czf "$temporary_root/dockerCopilot-amd64.tar.gz" -C "$package_dir" dockerCopilot-new
sha256sum "$temporary_root/dockerCopilot-amd64.tar.gz" > "$temporary_root/dockerCopilot-amd64.tar.gz.sha256"
[ "$(tar -tzf "$temporary_root/dockerCopilot-amd64.tar.gz")" = dockerCopilot-new ]
(cd "$temporary_root" && sha256sum -c dockerCopilot-amd64.tar.gz.sha256 >/dev/null)

echo "start.sh isolated handoff fixtures passed"
