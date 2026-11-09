#!/usr/bin/env bash
# Exercise e2e-down against disposable process groups and a fake docker command.
set -euo pipefail
cd "$(dirname "$0")/../.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/braid-e2e-down.XXXXXX")
relay_pid='' worker_pid='' outsider_pid=''
cleanup() {
  for pid in "$relay_pid" "$worker_pid" "$outsider_pid"; do
    [[ -z "$pid" ]] || kill -TERM -- "-$pid" 2>/dev/null || true
  done
  sleep .2
  for pid in "$relay_pid" "$worker_pid" "$outsider_pid"; do
    [[ -z "$pid" ]] || kill -KILL -- "-$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT
mkdir -p "$tmp/deploy" "$tmp/log/e2e" "$tmp/bin"
cp deploy/e2e-up.sh deploy/e2e-down.sh deploy/e2e-process.sh "$tmp/deploy/"
# Keep this cleanup test independent of real host relays. Modify only the
# disposable copy; production always inspects /proc.
mkdir -p "$tmp/proc/self/ns"
ln -s 'net:[test]' "$tmp/proc/self/ns/net"
sed -i 's|e2e_guard_relays "$E2E_MYSQL" "$E2E_REDIS"|e2e_guard_relays "$E2E_MYSQL" "$E2E_REDIS" "'"$tmp"'/proc"|' "$tmp/deploy/e2e-up.sh"
source deploy/e2e-process.sh
boot_id=$(cat /proc/sys/kernel/random/boot_id)
printf '#!/usr/bin/env bash\n[[ -z "${BRAID_FAKE_DOCKER_CALLS:-}" ]] || echo called >> "$BRAID_FAKE_DOCKER_CALLS"\nexit "${BRAID_FAKE_DOCKER_EXIT:-0}"\n' > "$tmp/bin/docker"
chmod +x "$tmp/bin/docker"
real_ps=$(command -v ps)
printf '#!/usr/bin/env bash\nif [[ "${BRAID_FAIL_PS:-}" == 1 && "$1" == -eo ]]; then exit 2; fi\nexec %q "$@"\n' "$real_ps" > "$tmp/bin/ps"
chmod +x "$tmp/bin/ps"

# Relay's child is in the same process group; its parent reaps it on TERM.
setsid bash -c 'trap '\''wait "$child" 2>/dev/null || true; exit 0'\'' TERM; sleep 120 & child=$!; wait "$child"' cmd/relay &
relay_pid=$!
# A TERM-resistant group checks KILL; its comm also tests spaces/parentheses in /proc stat.
setsid python3 -c 'import ctypes, pathlib, signal, sys; ctypes.CDLL(None).prctl(15, b"w ) x name", 0, 0, 0); signal.signal(signal.SIGTERM, signal.SIG_IGN); pathlib.Path(sys.argv[2]).touch(); signal.pause()' cmd/worker "$tmp/worker-ready" &
worker_pid=$!
# A stale component PID file must not kill a different process group.
setsid bash -c 'trap '\''wait "$child" 2>/dev/null || true; exit 0'\'' TERM; python3 -c "import signal; signal.pause()" cmd/reaper & child=$!; wait "$child"' cmd/api &
outsider_pid=$!
printf '%s\n' "$relay_pid" > "$tmp/log/e2e/relay.pid"
printf '%s\n' "$worker_pid" > "$tmp/log/e2e/worker.pid"
printf '%s\n' "$outsider_pid" > "$tmp/log/e2e/api.pid"
for _ in $(seq 1 50); do
  relay_child=$(pgrep -P "$relay_pid" | head -1 || true)
  outsider_child=$(pgrep -P "$outsider_pid" | head -1 || true)
  [[ -n "$relay_child" && -n "$outsider_child" ]] && break
  sleep .1
done
[[ -n "${relay_child:-}" ]] || { echo 'relay child did not start' >&2; exit 1; }
[[ -n "${outsider_child:-}" ]] || { echo 'unrelated child did not start' >&2; exit 1; }
for _ in $(seq 1 50); do
  tr '\0' ' ' < "/proc/$outsider_child/cmdline" | grep -Fq 'cmd/reaper' && break
  sleep .1
done
tr '\0' ' ' < "/proc/$outsider_child/cmdline" | grep -Fq 'cmd/reaper' || { echo 'non-leader command not ready' >&2; exit 1; }
[[ "$(ps -o pgid= -p "$outsider_child" | tr -d '[:space:]')" == "$outsider_pid" ]] || { echo 'non-leader group mismatch' >&2; exit 1; }
printf '%s\n' "$outsider_child" > "$tmp/log/e2e/reaper.pid"
for _ in $(seq 1 50); do
  [[ -f "$tmp/worker-ready" ]] && break
  sleep .1
done
[[ -f "$tmp/worker-ready" ]] || { echo 'worker did not install TERM handler' >&2; exit 1; }
for pair in "relay:$relay_pid" "worker:$worker_pid" "reaper:$outsider_child"; do
  comp=${pair%%:*} pid=${pair#*:}
  printf '%s %s\n' "$boot_id" "$(e2e_starttime "$pid")" > "$tmp/log/e2e/$comp.identity"
done
# Matching component and group leader but wrong starttime simulates a reused PID.
printf '%s %s\n' "$boot_id" "$(( $(e2e_starttime "$outsider_pid") + 1 ))" > "$tmp/log/e2e/api.identity"
if PATH="$tmp/bin:$PATH" bash "$tmp/deploy/e2e-down.sh" > "$tmp/out" 2> "$tmp/err"; then
  echo 'cleanup incorrectly succeeded despite identity mismatch' >&2; exit 1
fi
for pid in "$relay_pid" "$relay_child" "$worker_pid"; do
  state=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d '[:space:]' || true)
  [[ -z "$state" || "$state" == Z* ]] || { echo "owned process $pid still running: $state" >&2; exit 1; }
done
kill -0 "$outsider_pid" 2>/dev/null || { echo 'unrelated process was stopped' >&2; exit 1; }
kill -0 "$outsider_child" 2>/dev/null || { echo 'non-leader process was stopped' >&2; exit 1; }
grep -Fq '跳过 api' "$tmp/err" || { echo 'missing ownership warning' >&2; exit 1; }
grep -Fq 'reaper(pid ' "$tmp/err" || { echo 'missing non-leader group status' >&2; exit 1; }
grep -Fq '身份未确认的 e2e 进程已跳过' "$tmp/err" || { echo 'missing incomplete cleanup warning' >&2; exit 1; }
# A prior boot or a legacy PID file without identity is never trusted either.
printf '%s %s\n' 'different-boot' "$(e2e_starttime "$outsider_pid")" > "$tmp/log/e2e/api.identity"
if PATH="$tmp/bin:$PATH" bash "$tmp/deploy/e2e-down.sh" > "$tmp/out" 2> "$tmp/err"; then
  echo 'cleanup incorrectly succeeded despite boot mismatch' >&2; exit 1
fi
kill -0 "$outsider_pid" 2>/dev/null || { echo 'different-boot process was stopped' >&2; exit 1; }
grep -Fq '跳过 api' "$tmp/err" || { echo 'missing boot mismatch warning' >&2; exit 1; }
rm "$tmp/log/e2e/api.identity"
if PATH="$tmp/bin:$PATH" bash "$tmp/deploy/e2e-down.sh" > "$tmp/out" 2> "$tmp/err"; then
  echo 'cleanup incorrectly succeeded without legacy identity' >&2; exit 1
fi
kill -0 "$outsider_pid" 2>/dev/null || { echo 'legacy process was stopped' >&2; exit 1; }
grep -Fq '跳过 api' "$tmp/err" || { echo 'missing legacy identity warning' >&2; exit 1; }
rm -f "$tmp/log/e2e/"*.pid "$tmp/log/e2e/"*.identity
if BRAID_FAKE_DOCKER_EXIT=42 PATH="$tmp/bin:$PATH" bash "$tmp/deploy/e2e-down.sh" > "$tmp/out" 2> "$tmp/err"; then
  echo 'cleanup incorrectly succeeded despite compose failure' >&2; exit 1
else
  [[ "$?" -eq 42 ]] || { echo 'compose failure exit code not preserved' >&2; exit 1; }
fi
grep -Fq '数据面停止失败' "$tmp/err" || { echo 'missing compose failure warning' >&2; exit 1; }
# Repeated shutdown of a truly absent PID and group succeeds.
setsid sleep .1 & dead_pid=$!
wait "$dead_pid"
printf '%s\n' "$dead_pid" > "$tmp/log/e2e/relay.pid"
PATH="$tmp/bin:$PATH" bash "$tmp/deploy/e2e-down.sh" > "$tmp/out" 2> "$tmp/err"
grep -Fq '已停止' "$tmp/err" || { echo 'missing already stopped message' >&2; exit 1; }
# Create a group with a live member after its leader exits; both are our children.
python3 - "$tmp" <<'PY'
import os, pathlib, signal, subprocess, sys
tmp = pathlib.Path(sys.argv[1])
leader = subprocess.Popen(['sleep', '120'], preexec_fn=os.setpgrp)
member = subprocess.Popen(['sleep', '120'], preexec_fn=lambda: os.setpgid(0, leader.pid))
try:
    leader.terminate()
    leader.wait(timeout=5)
    (tmp / 'log/e2e/relay.pid').write_text(str(leader.pid) + '\n')
    env = dict(os.environ, PATH=str(tmp / 'bin') + ':' + os.environ['PATH'],
               BRAID_FAKE_DOCKER_CALLS=str(tmp / 'docker-calls'))
    down = subprocess.run(['bash', str(tmp / 'deploy/e2e-down.sh')], env=env, capture_output=True, text=True)
    assert down.returncode != 0 and '跳过 relay' in down.stderr
    assert member.poll() is None, 'unidentified member was stopped'
    (tmp / 'docker-calls').unlink(missing_ok=True)
    up = subprocess.run(['bash', str(tmp / 'deploy/e2e-up.sh')], env=env, capture_output=True, text=True)
    assert up.returncode != 0 and '进程组仍在跑' in up.stderr
    assert not (tmp / 'docker-calls').exists(), 'up started the data plane'
    env['BRAID_FAIL_PS'] = '1'
    down = subprocess.run(['bash', str(tmp / 'deploy/e2e-down.sh')], env=env, capture_output=True, text=True)
    assert down.returncode != 0 and member.poll() is None, 'failed inspection was treated as success'
    print('PASS: orphaned group blocks down/up; failed inspection stays nonzero')
finally:
    if member.poll() is None:
        os.killpg(leader.pid, signal.SIGTERM)
    member.wait(timeout=5)

zombie = subprocess.Popen(['sh', '-c', 'exit 0'], preexec_fn=os.setpgrp)
try:
    import time
    for _ in range(100):
        stat = pathlib.Path(f'/proc/{zombie.pid}/stat').read_text().rpartition(') ')[2].split()[0]
        if stat == 'Z':
            break
        time.sleep(.01)
    else:
        raise AssertionError('zombie did not appear')
    (tmp / 'log/e2e/relay.pid').write_text(str(zombie.pid) + '\n')
    env.pop('BRAID_FAIL_PS', None)
    down = subprocess.run(['bash', str(tmp / 'deploy/e2e-down.sh')], env=env, capture_output=True, text=True)
    assert down.returncode == 0 and '已停止' in down.stderr, down.stderr
    env['BRAID_FAKE_DOCKER_EXIT'] = '42'
    up = subprocess.run(['bash', str(tmp / 'deploy/e2e-up.sh')], env=env, capture_output=True, text=True)
    assert up.returncode == 42, up.stderr  # guard passed; fake docker stopped startup
    print('PASS: zombie-only group is treated as stopped')
finally:
    zombie.wait(timeout=5)
PY
echo 'PASS: matching groups stopped; unknown identities preserved; failures reported; repeated down succeeds'
python3 deploy/tests/e2e-down-preserve-selftest.py
bash deploy/tests/e2e-relay-guard-selftest.sh
