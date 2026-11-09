#!/usr/bin/env bash
# Isolated proc fixtures and one disposable relay-shaped process; fake Docker only.
set -euo pipefail
cd "$(dirname "$0")/../.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/braid-relay-guard.XXXXXX")
child=''
cleanup() {
  if [[ -n "$child" ]]; then kill "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; fi
  rm -rf -- "$tmp"
}
trap cleanup EXIT
source deploy/e2e-process.sh
mysql='test@tcp(127.0.0.1:45307)/test'
redis='127.0.0.1:45380'
proc="$tmp/proc"
mkdir -p "$proc/self/ns" "$proc/123/ns"
ln -s 'net:[test]' "$proc/self/ns/net"
ln -s 'net:[test]' "$proc/123/ns/net"
printf 'relay\n' > "$proc/123/comm"
check_blocked() {
  if e2e_guard_relays "$mysql" "$redis" "$proc" > "$tmp/out" 2> "$tmp/err"; then
    echo "guard unexpectedly passed: $1" >&2; exit 1
  fi
  ! grep -Fq 'fixture-secret' "$tmp/err" || { echo 'configuration leaked' >&2; exit 1; }
}
printf 'BRAID_MYSQL_DSN=fixture-secret@tcp(127.0.0.1:45307)/test\0BRAID_REDIS_QUEUE_ADDR=127.0.0.1:9999\0' > "$proc/123/environ"
check_blocked mysql
printf 'BRAID_MYSQL_DSN=test@tcp(127.0.0.1:9999)/test\0BRAID_REDIS_QUEUE_ADDR=localhost:45380\0' > "$proc/123/environ"
check_blocked redis
printf 'BRAID_MYSQL_DSN=test@tcp(127.0.0.1:9999)/test\0BRAID_REDIS_QUEUE_ADDR=127.0.0.1:9999\0' > "$proc/123/environ"
e2e_guard_relays "$mysql" "$redis" "$proc"
printf 'OTHER=value\0' > "$proc/123/environ"
check_blocked unknown
rm "$proc/123/environ"
check_blocked unreadable
rm "$proc/123/ns/net"
check_blocked namespace
ln -s 'net:[different]' "$proc/123/ns/net"
e2e_guard_relays "$mysql" "$redis" "$proc"
rm "$proc/123/ns/net"
ln -s 'net:[test]' "$proc/123/ns/net"
printf 'go\n' > "$proc/123/comm"
printf '/usr/bin/go\0run\0./cmd/relay\0' > "$proc/123/cmdline"
printf 'BRAID_MYSQL_DSN=test@tcp(127.0.0.1:45307)/test\0BRAID_REDIS_QUEUE_ADDR=127.0.0.1:9999\0' > "$proc/123/environ"
check_blocked go-run
printf '/usr/bin/go\0test\0./cmd/relay\0' > "$proc/123/cmdline"
e2e_guard_relays "$mysql" "$redis" "$proc"
rm "$proc/123/cmdline"
check_blocked go-command-unreadable
: > "$proc/123/cmdline"
check_blocked go-command-empty

mkdir -p "$tmp/deploy" "$tmp/bin"
cp deploy/e2e-up.sh deploy/e2e-process.sh "$tmp/deploy/"
sed -i 's/13307/45307/g; s/16380/45380/g' "$tmp/deploy/e2e-up.sh"
printf '#!/usr/bin/env bash\necho called >> "$BRAID_FAKE_DOCKER_CALLS"\nexit 42\n' > "$tmp/bin/docker"
chmod +x "$tmp/bin/docker"
BRAID_MYSQL_DSN="$mysql" BRAID_REDIS_QUEUE_ADDR="$redis" python3 - "$tmp/ready" <<'PY' &
import ctypes, pathlib, signal, sys
ctypes.CDLL(None).prctl(15, b'relay', 0, 0, 0)
pathlib.Path(sys.argv[1]).touch()
signal.pause()
PY
child=$!
for _ in $(seq 1 50); do [[ -f "$tmp/ready" ]] && break; sleep .1; done
[[ -f "$tmp/ready" ]] || { echo 'disposable relay did not start' >&2; exit 1; }
# Observe the real disposable child, but exclude unrelated host processes whose
# /proc permissions and namespaces are outside this test's control.
mkdir -p "$tmp/realproc/self/ns"
ln -s "$(readlink /proc/self/ns/net)" "$tmp/realproc/self/ns/net"
ln -s "/proc/$child" "$tmp/realproc/$child"
sed -i 's|e2e_guard_relays "$E2E_MYSQL" "$E2E_REDIS"|e2e_guard_relays "$E2E_MYSQL" "$E2E_REDIS" "'"$tmp"'/realproc"|' "$tmp/deploy/e2e-up.sh"
if BRAID_FAKE_DOCKER_CALLS="$tmp/docker-calls" PATH="$tmp/bin:$PATH" bash "$tmp/deploy/e2e-up.sh" > "$tmp/out" 2> "$tmp/err"; then
  echo 'up unexpectedly succeeded with a conflicting relay' >&2; exit 1
fi
grep -Fq "relay 进程 $child" "$tmp/err" || { cat "$tmp/err" >&2; exit 1; }
[[ ! -e "$tmp/docker-calls" ]] || { echo 'Docker ran despite conflicting relay' >&2; exit 1; }
kill -0 "$child" || { echo 'guard stopped the disposable relay' >&2; exit 1; }
echo 'PASS: endpoint, unknown identity, namespace, go-run, and real-process startup guards'
