#!/usr/bin/env bash
# braid 生产栈一键启动(issue #194,D-2026-10-08-prod-oneclick):交付栈
# prod.docker-compose.yml 的单命令包装。职责序:
#   ① deploy/prod.env 缺失则自 .env.example 播种(交付栈配置文件与 dev 的
#      deploy/.env 物理分离);密钥解析三级:prod.env 已有值(幂等重跑)>
#      deploy/.env 继承(**存量卷连续性**:卷以初始化时的 .env 值为真,
#      另起新值必 1045——首跑实证)> 全新部署 openssl rand 生成(既有卷下
#      root 无源即停;git 占位值 braid_dev_root 轮换仅限无卷;只填空缺不覆写)
#   ② LLM 三键(BASE_URL/API_KEY/PRICE_TABLE,worker 启动即败项)继承已并入
#      ①,此处仅验在场,缺则回显待补退出——业务密钥脚本替不了
#   ③ compose build(根 Dockerfile 多阶段,四二进制一镜像 braid:prod)→ up -d
#   ④ 等 reaper audit-ready 健康闸(T20.7 安装口径:janitor 首刷补当日分区
#      过闸,api/worker 才放行;上限 300s,超时报错指路 compose ps)
# 幂等:重跑安全(已生成密钥跳过 / build 缓存命中 / up 已在位 no-op)。
# 停栈:docker compose -f deploy/prod.docker-compose.yml down(加 -v 连卷清场)。
# 升级 / 改密 / dirty 恢复:deploy/UPGRADE.md;运维接入(反代):deploy/RUNBOOK.md。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="$ROOT/deploy/prod.env"
DEV_ENV="$ROOT/deploy/.env"
COMPOSE=(docker compose --env-file "$ENV_FILE" -f "$ROOT/deploy/prod.docker-compose.yml")

command -v docker >/dev/null 2>&1 || { echo "缺 docker"; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "缺 docker compose 插件"; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "缺 openssl"; exit 1; }

# ---- ① 配置播种 + 密钥解析(三级优先级,只填空缺不覆写)----------------
if [ ! -f "$ENV_FILE" ]; then
  cp "$ROOT/deploy/.env.example" "$ENV_FILE"
  echo "== 已从 .env.example 播种 deploy/prod.env"
fi
# 防末行无换行导致追加键与既有行粘连
[ -n "$(tail -c1 "$ENV_FILE")" ] && echo >> "$ENV_FILE" || true

SECRET_KEYS=(MYSQL_ROOT_PASSWORD BRAID_DB_API_PASSWORD BRAID_DB_WORKER_PASSWORD
             BRAID_DB_RELAY_PASSWORD BRAID_DB_REAPER_PASSWORD BRAID_AUTH_JWT_SECRET)

inherit_key() { # inherit_key <KEY>:prod.env 无值且 deploy/.env 有 → 整行拷贝
  local key="$1"
  grep -qE "^${key}=.+" "$ENV_FILE" && return 0
  if grep -qE "^${key}=.+" "$DEV_ENV" 2>/dev/null; then
    grep -E "^${key}=.+" "$DEV_ENV" >> "$ENV_FILE"
    echo "== 已从 deploy/.env 继承 ${key}"
  fi
}
gen_secret() { # gen_secret <KEY>:键缺失或值为空才生成,生成返 0/已有返 1;
  local key="$1" # hex 输出恒落 grants.sh 密码字符集白名单
  if grep -qE "^${key}=.+" "$ENV_FILE"; then return 1; fi
  if grep -qE "^${key}=" "$ENV_FILE"; then
    sed -i "s|^${key}=.*|${key}=$(openssl rand -hex 24)|" "$ENV_FILE"
  else
    printf '%s=%s\n' "$key" "$(openssl rand -hex 24)" >> "$ENV_FILE"
  fi
  echo "== 已生成 ${key}(写入 deploy/prod.env,git-ignored)"
}

HAS_VOL=$(docker volume ls --format '{{.Name}}' | grep -cx braid-prod_mysql-data || true)
for k in "${SECRET_KEYS[@]}" BRAID_LLM_BASE_URL BRAID_LLM_API_KEY BRAID_LLM_PRICE_TABLE; do
  inherit_key "$k"
done
# 既有卷守卫:root 密码与卷初始化值必须一致(1045 实证),两处 env 皆无源即停
if [ "$HAS_VOL" -eq 1 ] && ! grep -qE '^MYSQL_ROOT_PASSWORD=.+' "$ENV_FILE"; then
  echo "!! 既有 mysql 卷但 deploy/.env 与 deploy/prod.env 均无 MYSQL_ROOT_PASSWORD:卷以初始化时口令为真,脚本无法代拟。" >&2
  echo "   将原值补进 deploy/prod.env 后重跑;或 docker compose -f deploy/prod.docker-compose.yml down -v 清卷重来(数据删除)。" >&2
  exit 1
fi
for k in "${SECRET_KEYS[@]}"; do
  if gen_secret "$k"; then
    if [ "$HAS_VOL" -eq 1 ] && [ "$k" != "MYSQL_ROOT_PASSWORD" ] && [ "$k" != "BRAID_AUTH_JWT_SECRET" ]; then
      echo "   (注意:既有卷下新生成的 ${k} 若与存量账号口令不一致,组件启动 1045;改密走 deploy/UPGRADE.md §3)" >&2
    fi
  fi
done
# root 占位值处置分卷态:无卷 = 轮换安全(全新初始化);有卷 = 卷内实况为真,
# 脚本不代轮换(强度收敛走 UPGRADE.md §3 改密路径)
if [ "$HAS_VOL" -eq 0 ] && grep -qE '^MYSQL_ROOT_PASSWORD=braid_dev_root$' "$ENV_FILE"; then
  sed -i "s|^MYSQL_ROOT_PASSWORD=.*|MYSQL_ROOT_PASSWORD=$(openssl rand -hex 24)|" "$ENV_FILE"
  echo "== 已轮换 MYSQL_ROOT_PASSWORD(git 占位值不得用于生产)"
elif [ "$HAS_VOL" -eq 1 ] && grep -qE '^MYSQL_ROOT_PASSWORD=braid_dev_root$' "$ENV_FILE"; then
  echo "!! 既有卷 + root 密码为 git 占位值:强度收敛走 deploy/UPGRADE.md §3,本脚本不代轮换" >&2
fi

# ---- ② LLM 三键校验(继承已并入 ①,此处仅验在场)------------------------
missing=0
for k in BRAID_LLM_BASE_URL BRAID_LLM_API_KEY BRAID_LLM_PRICE_TABLE; do
  grep -qE "^${k}=.+" "$ENV_FILE" || {
    echo "缺 ${k}:编辑 deploy/prod.env 补齐(OpenAI 兼容端点 / API key / 单价表 JSON)"
    missing=1
  }
done
[ "$missing" -eq 0 ] || exit 1

# ---- ③ 构建 + 起栈 ------------------------------------------------------
# GOPROXY 解析链(与 release.sh §12 同口径):shell env 优先,缺省回落 go env
# ——受限网络出口常经国内镜像,官方 proxy.golang.org 直连不稳定(首跑实测 EOF)
proxy="${GOPROXY:-}"
if [ -z "$proxy" ] && command -v go >/dev/null 2>&1; then
  proxy="$(go env GOPROXY 2>/dev/null || true)"
fi
build_args=()
if [ -n "$proxy" ]; then
  build_args+=(--build-arg "GOPROXY=$proxy")
  echo "== GOPROXY=$proxy(经 build-arg 透传)"
fi
echo "== compose build"
"${COMPOSE[@]}" build "${build_args[@]}"
echo "== compose up -d"
"${COMPOSE[@]}" up -d

# ---- ④ 健康闸:reaper audit-ready(迁移/授权 one-shot 由 depends_on 先行
# 收敛;api/worker 以 reaper service_healthy 为放行条件,故此闸即全栈就绪位)
echo -n "== 等 reaper audit-ready 健康闸"
deadline=$((SECONDS + 300))
until [ "$(docker inspect -f '{{.State.Health.Status}}' braid-prod-reaper 2>/dev/null || echo starting)" = healthy ]; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo
    echo "超时(300s)。现场排查:docker compose -f deploy/prod.docker-compose.yml ps"
    exit 1
  fi
  printf '.'
  sleep 3
done
echo " 过闸"

cat <<'EOF'
== 就绪。braid-prod = {api,worker,relay,reaper} + mysql + redis-queue 六常驻
   (+ migrate/grants one-shot 已退场)。零宿主端口:API 与 Web 面(/workflows/
   控制台、/executions/ 执行面、/approvals/ 审批台;旧审批台/画布前缀硬切 404)
   经反代指向 compose 内网 api:8081;运维面 deploy/RUNBOOK.md,升级/改密
   deploy/UPGRADE.md,验收:docker compose -f deploy/prod.docker-compose.yml ps
== 首个用户建号(栈零种子账号,不建号无人能登录):deploy/README.md §5.1
EOF
