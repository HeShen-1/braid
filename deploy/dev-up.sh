#!/usr/bin/env bash
# dev 栈 host 四进程一键启动 + 一键 demo seed(T16.3):MySQL 就绪等待 →
# seed db 段(迁移 + 建号,未迁移库上组件会启动失败,故先于四进程)→
# 四进程 → healthz → seed rest 段(示例定义发布 + 触发,幂等可重跑)。
# 交互式调试仍可手动起四进程。日志管理:每次启动归档到 log/<时间戳>/
# <组件>.log,log/latest 软链指向本次;历史全部保留(文本日志占用可
# 忽略,手动清理)。
# 前置:deploy/.env 已配(缺失时先 cp deploy/.env.example deploy/.env);
# dev 栈容器(deploy/dev.docker-compose.yml)会在下方等 healthy,未起栈
# 会响亮报错。跳过 demo seed:BRAID_DEV_SEED=0 deploy/dev-up.sh。
set -euo pipefail
cd "$(dirname "$0")/.."

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

# MySQL 就绪等待(容器 healthcheck 口径,免取口令;首次初始化要十几秒)。
for _ in $(seq 1 60); do
	[ "$(docker inspect -f '{{.State.Health.Status}}' braid-dev-mysql 2>/dev/null)" = "healthy" ] && break
	sleep 1
done
if [ "$(docker inspect -f '{{.State.Health.Status}}' braid-dev-mysql 2>/dev/null)" != "healthy" ]; then
	echo "braid-dev-mysql 60s 未 healthy:先起栈 docker compose -f deploy/dev.docker-compose.yml up -d(或查其日志)" >&2
	exit 1
fi

# seed 二进制一次编译,两段共用;随日志归档(无清理面)。
if [ "${BRAID_DEV_SEED:-1}" != "0" ]; then
	go build -o "$LOGDIR/braid-seed" ./cmd/braid
	echo "== demo seed db 段(迁移 + 建号;幂等,重跑安全)=="
	if ! bash -c "set -a; source deploy/.env; set +a; exec '$LOGDIR/braid-seed' seed -stage db"; then
		echo "seed db 段失败(常见:DSN 配错/迁移 dirty);组件进程未起,修好后重跑" >&2
		exit 1
	fi
fi

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

if [ "${BRAID_DEV_SEED:-1}" != "0" ]; then
	echo "== demo seed rest 段(定义发布 + 触发 + 落点等待;幂等,重跑安全)=="
	if ! bash -c "set -a; source deploy/.env; set +a; exec '$LOGDIR/braid-seed' seed -stage rest"; then
		echo "seed rest 段失败;四进程仍在跑(查上方输出,常见:LLM/登录面),停止用 deploy/dev-down.sh" >&2
		exit 1
	fi
	echo "demo 就绪:审批页见上方 seed ⑤ 行;重跑本脚本 = 幂等 no-op(既有执行幂等键命中)"
fi

echo "四进程已起:api http://127.0.0.1:8081(/ui/ 审批页) Asynqmon http://127.0.0.1:8080"
echo "日志:$LOGDIR(log/latest 软链;历史全部保留,手动清理)"
echo "跟踪:tail -f log/latest/worker.log    停止:deploy/dev-down.sh"
