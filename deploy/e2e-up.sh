#!/usr/bin/env bash
# T16.6 Playwright e2e 栈一键启动(D-2026-10-04-t166-playwright-e2e):
# compose 数据面(MySQL 13307 / Redis 16380,与 dev/t13/drill 全错开)
# → 迁移(prod compose 同款 migrate 镜像一发 up)→ 种子用户(braid CLI
# 原语,hash/词域同则)→ api/relay/worker/reaper 四进程(api 18081)。
# 与 dev-up.sh 零共写(独立脚本/独立端口/独立日志目录);每进程 setsid
# 独立进程组 → e2e-down.sh 只收自己的组,不误伤并行 dev 栈(活进程联调
# 仅主仓公约下,两栈同宿主是常态)。重复运行先 e2e-down.sh(种子冲突
# exit 1;数据面 tmpfs,down 即清、每次全新)。
#
# 密钥面:JWT secret 与 LLM key 运行期 openssl rand 生成(栈生命周期内
# 稳定;零密钥形态字面量落盘);LLM 零节点执行,worker 构造期校验在位
# 即可;file_read 白名单 = 仓内 e2e fixture 目录。跑法:
#   deploy/e2e-up.sh
#   BRAID_E2E_BASE_URL=http://127.0.0.1:18081 npm --prefix web/canvas run test:e2e
set -euo pipefail
cd "$(dirname "$0")/.."

E2E_MYSQL="root:braid_dev_root@tcp(127.0.0.1:13307)/braid"
E2E_REDIS="127.0.0.1:16380"
E2E_API_ADDR="127.0.0.1:18081"
# 密钥面:JWT secret 与 LLM 三件均运行期随机生成(e2e 栈生命周期内
# 稳定即可;占位字面量会触发 gitleaks generic-api-key——对抗审计 HIGH,
# 修法 = 不落任何密钥形态字面量,而非扩 allowlist);LLM 零节点执行,
# worker 构造期校验在位即可。file_read 白名单 = 仓内 e2e fixture 目录。
E2E_JWT_SECRET="$(openssl rand -hex 32)"
E2E_LLM_KEY="$(openssl rand -hex 16)"
E2E_FILEREAD="$(pwd)/web/canvas/e2e/fixtures/files"
E2E_FILEREAD_JSON="[\"$E2E_FILEREAD\"]" # 词法:JSON 数组形(builtin.go 单源装配)
LOGDIR="log/e2e"

# 防双实例:本栈 pid 文件仍有活进程(双实例 = relay/worker 双消费)。
if [ -d "$LOGDIR" ]; then
  for f in "$LOGDIR"/*.pid; do
    [ -f "$f" ] || continue
    if kill -0 "$(cat "$f")" 2>/dev/null; then
      echo "e2e 组件进程仍在跑(pid 文件 $f),先停:deploy/e2e-down.sh" >&2
      exit 1
    fi
  done
fi

# ① 数据面(compose 项目 braid-e2e;healthcheck 内建,等 mysql 可 ping;
# 落穿 = 响亮失败,不对不可达空库跑 migrate——对抗审计 LOW)。
docker compose -f deploy/e2e.docker-compose.yml -p braid-e2e up -d
mysql_ok=0
for _ in $(seq 1 60); do
  if docker compose -f deploy/e2e.docker-compose.yml -p braid-e2e exec -T mysql \
    mysqladmin ping -h127.0.0.1 -uroot -pbraid_dev_root >/dev/null 2>&1; then
    mysql_ok=1
    break
  fi
  sleep 2
done
if [ "$mysql_ok" -ne 1 ]; then
  echo "e2e mysql 120s 未 healthy;查:docker compose -f deploy/e2e.docker-compose.yml -p braid-e2e logs mysql" >&2
  exit 1
fi

# ② 迁移:prod compose 同款钉版镜像(全仓无 migrate cmd 入口)。
docker run --rm --network host -v "$PWD/internal/db/migrations:/migrations:ro" \
  migrate/migrate:v4.18.2 -path /migrations \
  -database "mysql://root:braid_dev_root@tcp(127.0.0.1:13307)/braid?multiStatements=true" up

# ③ 种子用户:CLI 建号(DB 直连 + 雪花短租约,MySQL-only);口令 = e2e
# 占位值(8..72 bcrypt 域内),playwright spec 同字面消费。
seed() {
  BRAID_MYSQL_DSN="$E2E_MYSQL" BRAID_CLI_PASSWORD="$2" \
    go run ./cmd/braid user create -username "$1" -roles "$3"
}
seed author e2e-author-pass-1 author
seed alice e2e-alice-pass-1 approver
seed bob e2e-bob-pass-1 approver

# ④ 四进程(api/relay/worker/reaper;env 面见各 cmd 注释)。
mkdir -p "$LOGDIR"
start() { # $1=组件,其余 = 组件专属 env 赋值对(公共面统一注入)
  local comp="$1"
  shift
  setsid nohup env "$@" \
    BRAID_MYSQL_DSN="$E2E_MYSQL" \
    BRAID_AUTH_JWT_SECRET="$E2E_JWT_SECRET" \
    BRAID_FILEREAD_DIRS="$E2E_FILEREAD_JSON" \
    go run "./cmd/$comp" > "$LOGDIR/$comp.log" 2>&1 &
  echo $! > "$LOGDIR/$comp.pid"
}
# 登录限流抬闸:旅程不测登录防御(那是 loginlimit 的面),而 e2e 每
# run 4 登录、快速迭代连跑两次即 8/10——第三次快跑必撞 IP 窗 429 假红。
start api BRAID_API_ADDR="$E2E_API_ADDR" BRAID_API_LOGIN_RATE=60
start relay BRAID_REDIS_QUEUE_ADDR="$E2E_REDIS"
start worker \
  BRAID_REDIS_QUEUE_ADDR="$E2E_REDIS" \
  BRAID_LLM_BASE_URL="http://127.0.0.1:9/v1" \
  BRAID_LLM_API_KEY="$E2E_LLM_KEY" \
  BRAID_LLM_PRICE_TABLE='{"gpt-x":{"input_per_mtok":"0","output_per_mtok":"0","currency":"USD"}}'
start reaper BRAID_REDIS_QUEUE_ADDR="$E2E_REDIS"

# ⑤ api 就绪探针(编译 + 启动,给 90s)。
for _ in $(seq 1 45); do
  curl -sf "http://$E2E_API_ADDR/healthz" >/dev/null 2>&1 && break
  sleep 2
done
if ! curl -sf "http://$E2E_API_ADDR/healthz" >/dev/null 2>&1; then
  echo "e2e api 90s 未就绪;查 $LOGDIR/api.log(常因端口占用/迁移未过)" >&2
  exit 1
fi

# ⑥ 组件活性复核(对抗审计 MEDIUM:只探 api 会让静默死亡的 relay/worker
# 拖到测试断言超时才暴露——此处死了就当场带日志退出)。
for comp in api relay worker reaper; do
  if ! kill -0 "$(cat "$LOGDIR/$comp.pid")" 2>/dev/null; then
    echo "e2e 组件 $comp 启动后死亡;日志尾:" >&2
    tail -n 20 "$LOGDIR/$comp.log" >&2 || true
    exit 1
  fi
done

echo "e2e 栈已起:api http://$E2E_API_ADDR(/ui/canvas/ 画布,/ui/ 审批台)"
echo "跑测试:BRAID_E2E_BASE_URL=http://$E2E_API_ADDR npm --prefix web/canvas run test:e2e"
echo "日志:$LOGDIR    停止:deploy/e2e-down.sh"
