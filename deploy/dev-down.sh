#!/usr/bin/env bash
# 停 dev 栈 host 四进程(deploy/dev-up.sh 起的;手动起的同组件一并停)。
# 日志不删(归档目录由 dev-up.sh 的保留策略管理)。
# 停序:先 TERM(go run 会转发信号给编译产物并等待退出)→ 宽限 → KILL
# 兜底(SIGKILL 不转发,残留的编译产物进程按名清扫)。
set -uo pipefail
cd "$(dirname "$0")/.."

# pid 文件优先(本次启动的 go run 包装进程)
if ls log/20*/[a-z]*.pid >/dev/null 2>&1; then
	for f in log/20*/[a-z]*.pid; do
		kill -TERM "$(cat "$f")" 2>/dev/null || true
	done
fi
# 兜底:按名 TERM(手动起 / pid 失效场景)
pkill -TERM -f 'go-build.*-d/(api|relay|worker|reaper)$' 2>/dev/null || true

for _ in $(seq 1 10); do
	pgrep -f 'go-build.*-d/(api|relay|worker|reaper)$' >/dev/null 2>&1 || break
	sleep 1
done

# KILL 清扫(残留编译产物 + go run 包装)
pkill -KILL -f 'go-build.*-d/(api|relay|worker|reaper)$' 2>/dev/null || true
pkill -KILL -f 'go run \./cmd/(api|relay|worker|reaper)' 2>/dev/null || true

if pgrep -f 'go-build.*-d/(api|relay|worker|reaper)$' >/dev/null 2>&1; then
	echo "仍有组件进程存活:" >&2
	pgrep -af 'go-build.*-d/(api|relay|worker|reaper)$' >&2
	exit 1
fi
echo "四进程已停(dev 栈容器不受影响;docker compose -f deploy/dev.docker-compose.yml down 另行)"
