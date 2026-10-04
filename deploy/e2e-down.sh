#!/usr/bin/env bash
# T16.6 e2e 栈停止:只收本栈进程组(e2e-up.sh 的 setsid pgid = pid 文件,
# 组内含 go run 与其编译产物)→ compose down(数据面 tmpfs 随容器退场,
# 栈可重复)。刻意不做按名 pkill 清扫——那会误伤并行 dev 栈(同二进制
# 名;活进程联调仅主仓公约下两栈同宿主是常态)。组杀前做归属校验
# (组长 cmdline 须含 cmd/<组件>):pid 回绕复用时防误杀无关进程组
# (AI review LOW)。
set -uo pipefail
cd "$(dirname "$0")/.."

LOGDIR="log/e2e"

# 归属校验:pid 文件按组件命名(api/relay/worker/reaper.pid),组长
# 进程 cmdline 须匹配对应组件;不符(复用 pid / 残留陈旧文件)跳过并
# 提示,不杀。
own_pid() { # $1=组件 → echo 校验通过的 pid,失败输出空串
  local f="$LOGDIR/$1.pid"
  [ -f "$f" ] || return 1
  local pid
  pid=$(cat "$f")
  if [ -r "/proc/$pid/cmdline" ] && tr '\0' ' ' < "/proc/$pid/cmdline" | grep -q "cmd/$1"; then
    echo "$pid"
    return 0
  fi
  echo "跳过 $1(pid $pid 归属不符——残留 pid 文件或 pid 复用,防误杀)" >&2
  return 1
}

if [ -d "$LOGDIR" ]; then
  pids=""
  for comp in api relay worker reaper; do
    p=$(own_pid "$comp") && pids="$pids $p"
  done
  [ -z "$pids" ] || kill -TERM -- $pids 2>/dev/null || true
  for _ in $(seq 1 10); do
    alive=0
    for pid in $pids; do
      kill -0 -- -"$pid" 2>/dev/null && alive=1
    done
    [ "$alive" -eq 0 ] && break
    sleep 1
  done
  [ -z "$pids" ] || kill -KILL -- $pids 2>/dev/null || true
fi

docker compose -f deploy/e2e.docker-compose.yml -p braid-e2e down -v
echo "e2e 栈已停(数据面 tmpfs 随 down 清空)"
