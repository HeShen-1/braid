#!/usr/bin/env bash
# T16.6 e2e 栈停止:只收本栈进程组(e2e-up.sh 的 setsid pgid = pid 文件,
# 组内含 go run 与其编译产物)→ compose down(数据面 tmpfs 随容器退场,
# 栈可重复)。刻意不做按名 pkill 清扫——那会误伤并行 dev 栈(同二进制
# 名;活进程联调仅主仓公约下两栈同宿主是常态)。
set -uo pipefail
cd "$(dirname "$0")/.."

LOGDIR="log/e2e"
if [ -d "$LOGDIR" ]; then
  for f in "$LOGDIR"/*.pid; do
    [ -f "$f" ] || continue
    kill -TERM -- -"$(cat "$f")" 2>/dev/null || true
  done
  for _ in $(seq 1 10); do
    alive=0
    for f in "$LOGDIR"/*.pid; do
      [ -f "$f" ] || continue
      kill -0 -- -"$(cat "$f")" 2>/dev/null && alive=1
    done
    [ "$alive" -eq 0 ] && break
    sleep 1
  done
  for f in "$LOGDIR"/*.pid; do
    [ -f "$f" ] || continue
    kill -KILL -- -"$(cat "$f")" 2>/dev/null || true
  done
fi

docker compose -f deploy/e2e.docker-compose.yml -p braid-e2e down -v
echo "e2e 栈已停(数据面 tmpfs 随 down 清空)"
