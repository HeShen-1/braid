#!/usr/bin/env bash
# braid Helm chart 静态验收 harness(T14.4;D-2026-09-30-t144-helm 决策 1)。
#
# 断言面(与 prod compose 交付面等价 = 静态可断言属性):
#  1. helm lint 零告警(--strict:WARNING 亦非零退出)
#  2. 全覆盖渲染成功 + kubeconform strict(钉 K8s schema 版本)
#  3. compose 对拍:默认面渲染的逐组件 env 键集 == prod compose 逐服务键集
#     (docker compose config 渲染提取,零 fixture 漂移——compose 改键即红)
#  4. 对象清单(默认面渲染)= 交付面映射清单(8 服务 → 14 对象)+ 文档
#     总数闭环(渲染文档数 == 已枚举对象数,新 kind 不失明)
#  5. Redis 契约(noeviction + AOF everysec,T7.3 preflight 互锁的是策略
#     与 AOF;maxmemory 体量可调,仅断言旗标在场)
#  6. Service 全 ClusterIP(集合相等,非「至少一个」;零宿主暴露)
#  7. 缺密钥渲染必须失败(required 执法 = compose ${VAR:?} fail-fast 对齐)
#  8. values.yaml secrets 默认值恒空(密钥不进 chart 文本与默认值)
#
# 用法:任意目录执行 deploy/chart/tests/verify.sh;依赖 docker(helm/
# kubeconform/yq 经钉版镜像取用,promtool job 同先例;compose 用本机 CLI)。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$ROOT/deploy/chart"
TESTS="$CHART/tests"
HELM_IMAGE=alpine/helm:3.16.4
KUBECONFORM_IMAGE=ghcr.io/yannh/kubeconform:v0.6.7
YQ_IMAGE=mikefarah/yq:4.44.3
K8S_VERSION=1.31.0 # kubeconform schema 钉版
RELEASE=braid # release 名含 chart 名 ⇒ fullname = braid(对象名前缀确定)

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
note() { echo "== $*"; }

helm_() { docker run --rm -i -v "$ROOT:/work" -w /work "$HELM_IMAGE" "$@"; }
# yq 经 stdin 读宿主任意文件(渲染产物在 mktemp 目录,不依赖挂载):
yq_in() { docker run --rm -i "$YQ_IMAGE" eval-all "$1" - < "$2"; }

cd "$ROOT"

# ---- 1. helm lint ----
note "1/8 helm lint --strict(零告警,WARNING 亦红)"
helm_ lint deploy/chart -f deploy/chart/tests/test-values.yaml --strict --quiet

# ---- 2. 渲染 + kubeconform ----
note "2/8 渲染(全覆盖)+ kubeconform strict(K8s $K8S_VERSION)"
helm_ template "$RELEASE" deploy/chart -f deploy/chart/tests/test-values.yaml > "$WORK/full.yaml"
[ -s "$WORK/full.yaml" ] || fail "full render empty"
docker run --rm -i "$KUBECONFORM_IMAGE" \
  -strict -summary -kubernetes-version "$K8S_VERSION" - < "$WORK/full.yaml"

# 默认面渲染(对拍基准:密钥齐备、选通面全关)
helm_ template "$RELEASE" deploy/chart -f deploy/chart/tests/parity-values.yaml > "$WORK/parity.yaml"

# ---- 3. compose 对拍(逐组件 env 键集)----
note "3/8 compose 对拍:逐组件 env 键集"
docker compose -f deploy/prod.docker-compose.yml \
  --env-file deploy/chart/tests/dummy.env config --format json > "$WORK/compose.json"
[ -s "$WORK/compose.json" ] || fail "docker compose config rendered empty"

# compose 服务 → chart 对象(交付面映射;Secret 是 values→env 注入面,不在 compose 侧)
chart_env_keys() { # kind name
  yq_in "select(.kind == \"$1\" and .metadata.name == \"$2\") | .spec.template.spec.containers[0].env[]?.name" "$WORK/parity.yaml" | sed '/^$/d;s/^"//;s/"$//' | sort -u
}
compose_env_keys() { # service
  yq_in ".services.$1.environment // {} | keys" "$WORK/compose.json" | sed '/^\[\]$/d;/^null$/d;/^---$/d;/^$/d;s/^- "//;s/^"//;s/"$//' | sort -u
}
declare -A MAP=(
  [mysql]="Deployment ${RELEASE}-mysql"
  [redis-queue]="Deployment ${RELEASE}-redis-queue"
  [migrate]="Job ${RELEASE}-migrate"
  [grants]="Job ${RELEASE}-grants"
  [api]="Deployment ${RELEASE}-api"
  [worker]="Deployment ${RELEASE}-worker"
  [relay]="Deployment ${RELEASE}-relay"
  [reaper]="Deployment ${RELEASE}-reaper"
)
rc=0
for svc in mysql redis-queue migrate grants api worker relay reaper; do
  read -r kind name <<<"${MAP[$svc]}"
  chart_env_keys "$kind" "$name" > "$WORK/c.svc" # 工具坏 = set -e 即红,不吞
  compose_env_keys "$svc" > "$WORK/k.svc"
  if ! diff -u "$WORK/k.svc" "$WORK/c.svc" > "$WORK/d.svc"; then
    echo "env 键集不等:compose:$svc ↔ chart:$kind/$name"
    cat "$WORK/d.svc"; rc=1
  else
    echo "  $svc:$(wc -l < "$WORK/c.svc" | tr -d ' ') 键一致"
  fi
done
[ $rc -eq 0 ] || fail "compose 对拍键集不等"

# T23.3:键集相等不足以发现「api 能发布、worker 收不到」；用非空配置
# 同时核对两进程实值、非零预算和只读挂载，防止两模板一起漏接仍假绿。
note "3/8 工具配置实值、非零预算与只读文件挂载"
docker compose -f deploy/prod.docker-compose.yml \
  --env-file deploy/chart/tests/dummy.env --env-file deploy/chart/tests/tools.env \
  config --format json > "$WORK/tools.json"
chart_env_value() { # component key
  yq_in "select(.kind == \"Deployment\" and .metadata.name == \"${RELEASE}-$1\") | .spec.template.spec.containers[0].env[] | select(.name == \"$2\") | .value" "$WORK/full.yaml" | sed '/^---$/d;/^$/d'
}
for svc in api worker; do
  for key in BRAID_HTTP_ENDPOINTS BRAID_FILEREAD_DIRS; do
    actual="$(chart_env_value "$svc" "$key")"
    expected="$(yq_in ".services.$svc.environment.$key" "$WORK/tools.json")"
    [ -n "$actual" ] && [ "$actual" = "$expected" ] \
      || fail "$svc $key 实值未贯通(compose/chart)"
  done
done
budget="$(chart_env_value worker BRAID_LLM_TOKEN_BUDGET)"
[ "$budget" = "321" ] \
  && [ "$budget" = "$(yq_in '.services.worker.environment.BRAID_LLM_TOKEN_BUDGET' "$WORK/tools.json")" ] \
  || fail "worker 非零 token 预算未贯通(compose/chart)"
compose_mount='.services.worker.volumes[] | select(.target == "/var/braid/docs")'
[ "$(yq_in "$compose_mount | .source" "$WORK/tools.json")" = /braid-fixture/docs ] \
  && [ "$(yq_in "$compose_mount | .read_only" "$WORK/tools.json")" = true ] \
  && [ "$(yq_in "$compose_mount | .bind.create_host_path // false" "$WORK/tools.json")" = false ] \
  && [ "$(yq_in "$compose_mount | .bind.create_host_path" "$ROOT/deploy/prod.docker-compose.yml")" = false ] \
  || fail "compose file_read 未按指定来源只读挂载(禁止自动创建宿主目录)"
worker_spec="select(.kind == \"Deployment\" and .metadata.name == \"${RELEASE}-worker\") | .spec.template.spec"
[ "$(yq_in "$worker_spec | .volumes[] | select(.name == \"file-read\") | .hostPath.path" "$WORK/full.yaml" | sed '/^---$/d;/^$/d')" = /var/braid/docs ] \
  && [ "$(yq_in "$worker_spec | .volumes[] | select(.name == \"file-read\") | .hostPath.type" "$WORK/full.yaml" | sed '/^---$/d;/^$/d')" = Directory ] \
  && [ "$(yq_in "$worker_spec | .containers[0].volumeMounts[] | select(.name == \"file-read\") | .mountPath" "$WORK/full.yaml" | sed '/^---$/d;/^$/d')" = /var/braid/docs ] \
  && [ "$(yq_in "$worker_spec | .containers[0].volumeMounts[] | select(.name == \"file-read\") | .readOnly" "$WORK/full.yaml" | sed '/^---$/d;/^$/d')" = true ] \
  || fail "chart file_read 未按节点/容器同路径只读挂载"

# ---- 4. 对象清单 ----
note "4/8 对象清单(默认面)"
# 注:yq eval-all 下跨文档字符串拼接(.kind + "/" + .name)行为损坏(kind
# 粘首文档),逐 kind select + shell 拼前缀是实证可靠的提取路径。
: > "$WORK/objs"
for k in Deployment Job PersistentVolumeClaim Secret Service; do
  yq_in "select(.kind == \"$k\") | .metadata.name" "$WORK/parity.yaml" \
    | sed '/^---$/d;/^$/d;s/^"//;s/"$//' | sed "s|^|$k/|" >> "$WORK/objs"
done
sort -o "$WORK/objs" "$WORK/objs"
# 文档总数闭环(审计收口 ④):逐 kind 枚举对新增 kind 失明,以「渲染文档
# 数 == 枚举对象数」堵住(每个文档恰一行零缩进 kind:)。
total_docs=$(grep -c '^kind: ' "$WORK/parity.yaml")
enum_objs=$(wc -l < "$WORK/objs" | tr -d ' ')
[ "$total_docs" -eq "$enum_objs" ] \
  || fail "对象清单失明:渲染 $total_docs 个文档 ≠ 已枚举 $enum_objs 个对象(新 kind 未入清单?)"
cat > "$WORK/objs.exp" <<EOF
Deployment/${RELEASE}-api
Deployment/${RELEASE}-mysql
Deployment/${RELEASE}-reaper
Deployment/${RELEASE}-redis-queue
Deployment/${RELEASE}-relay
Deployment/${RELEASE}-worker
Job/${RELEASE}-grants
Job/${RELEASE}-migrate
PersistentVolumeClaim/${RELEASE}-mysql-data
PersistentVolumeClaim/${RELEASE}-redis-queue-data
Secret/${RELEASE}-secret
Service/${RELEASE}-api
Service/${RELEASE}-mysql
Service/${RELEASE}-redis-queue
EOF
diff -u "$WORK/objs.exp" "$WORK/objs" || fail "对象清单不等价(compose 8 服务 ⇒ 14 对象)"

# ---- 5. Redis 契约 ----
note "5/8 Redis 契约(noeviction / AOF everysec;体量仅要求旗标在场)"
yq_in "select(.kind == \"Deployment\" and .metadata.name == \"${RELEASE}-redis-queue\") | .spec.template.spec.containers[0].args | join(\" \")" \
  "$WORK/parity.yaml" | sed '/^---$/d;/^$/d' > "$WORK/redis.args"
grep -q -- '--maxmemory-policy noeviction' "$WORK/redis.args" \
  || { cat "$WORK/redis.args"; fail "Redis 契约破坏:noeviction 策略缺席(T7.3 preflight 互锁:逐出会静默丢任务)"; }
grep -q -- '--appendonly yes' "$WORK/redis.args" \
  || { cat "$WORK/redis.args"; fail "Redis 契约破坏:AOF 未开(everysec 丢失窗契约)"; }
grep -q -- '--appendfsync everysec' "$WORK/redis.args" \
  || { cat "$WORK/redis.args"; fail "Redis 契约破坏:appendfsync != everysec"; }
grep -q -- '--maxmemory ' "$WORK/redis.args" \
  || { cat "$WORK/redis.args"; fail "Redis 契约破坏:--maxmemory 旗标缺席"; }
# mysql 钉死面:库名 braid(非凭证,DSN 确定性)
yq_in "select(.kind == \"Deployment\" and .metadata.name == \"${RELEASE}-mysql\") | .spec.template.spec.containers[0].env[] | select(.name == \"MYSQL_DATABASE\") | .value" \
  "$WORK/parity.yaml" | sed '/^---$/d;/^$/d' | grep -qx braid || fail "MYSQL_DATABASE != braid"

# ---- 6. 零宿主暴露 ----
note "6/8 Service 全 ClusterIP(集合相等)"
svc_types="$(yq_in 'select(.kind == "Service") | .spec.type' "$WORK/parity.yaml" | sed '/^---$/d;/^$/d;s/^"//;s/"$//' | sort -u)"
[ "$svc_types" = "ClusterIP" ] \
  || fail "存在非 ClusterIP Service(类型集:$(echo "$svc_types" | tr '\n' ' '))——零宿主暴露契约破坏"

# ---- 7. required 执法 ----
note "7/8 缺密钥渲染必须失败(required = compose :? fail-fast 对齐)"
if helm_ template "$RELEASE" deploy/chart > "$WORK/nope.yaml" 2> "$WORK/nope.err"; then
  fail "无 values 渲染竟成功(required 未执法)"
fi
grep -q 'required' "$WORK/nope.err" || { cat "$WORK/nope.err"; fail "失败原因不是 required"; }

# ---- 8. values 默认值零密钥 ----
note "8/8 values.yaml secrets 默认值恒空"
awk '
  /^secrets:/ { f=1; next }
  /^[A-Za-z]/ { f=0 }
  f && /^[[:space:]]+[A-Za-z][A-Za-z0-9]*:/ && $0 !~ /: ""([[:space:]]*#.*)?$/ { print; bad=1 }
  END { exit bad ? 1 : 0 }
' deploy/chart/values.yaml || fail "values.yaml secrets 段存在非空默认值(密钥不进默认值)"

echo "PASS: braid chart 静态门 8/8(helm lint / kubeconform / compose 对拍 / 契约断言)"
