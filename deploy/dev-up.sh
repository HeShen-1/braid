#!/usr/bin/env bash
# dev 栈 host 四进程一键启动(README ⑤ 的四终端手动形态的封装;交互式
# 调试仍可手动起)。日志管理:每次启动归档到 log/<时间戳>/<组件>.log,
# log/latest 软链指向本次,自动保留最近 KEEP 次启动(默认 5)。
# 前置:deploy/.env 已配(缺失时先 cp deploy/.env.example deploy/.env);
# dev 栈容器(deploy/dev.docker-compose.yml)已在跑。
set -euo pipefail
cd "$(dirname "$0")/.."

KEEP=5

if [ ! -f deploy/.env ]; then
	echo "deploy/.env 不存在:先 cp deploy/.env.example deploy/.env 并补齐(env 书写规则见其文件头)" >&2
	exit 1
fi

# 防双实例:残留组件进程会导致 relay/worker 双消费(实测双实例互踩)。
if pgrep -f 'go-build.*-d/(api|relay|worker|reaper)$' >/dev/null 2>&1; then
	echo "已有组件进程在跑,先停:deploy/dev-down.sh" >&2
	pgrep -af 'go-build.*-d/(api|relay|worker|reaper)$' >&2
	exit 1
fi

TS=$(date +%Y%m%d-%H%M%S)
LOGDIR="log/$TS"
mkdir -p "$LOGDIR"
ln -sfn "$(basename "$LOGDIR")" log/latest # 软链目标用 log/ 内相对名
# 保留最近 KEEP 次启动,更旧的整目录删除
ls -1d log/20* 2>/dev/null | head -n -"$KEEP" | xargs -r rm -rf

for c in api relay worker reaper; do
	nohup bash -c "set -a; source deploy/.env; set +a; exec go run ./cmd/$c" \
		> "$LOGDIR/$c.log" 2>&1 &
	echo $! > "$LOGDIR/$c.pid"
done

# api 就绪探针(编译 + 启动,给 90s)
for _ in $(seq 1 45); do
	curl -sf http://127.0.0.1:8081/healthz >/dev/null 2>&1 && break
	sleep 2
done
if ! curl -sf http://127.0.0.1:8081/healthz >/dev/null 2>&1; then
	echo "api 90s 未就绪;查 $LOGDIR/api.log(常因 .env 键缺失,错误信息会指名)" >&2
	exit 1
fi

echo "四进程已起:api http://127.0.0.1:8081(/ui/ 审批页) Asynqmon http://127.0.0.1:8080"
echo "日志:$LOGDIR(log/latest 软链;保留最近 $KEEP 次启动)"
echo "跟踪:tail -f log/latest/worker.log    停止:deploy/dev-down.sh"
