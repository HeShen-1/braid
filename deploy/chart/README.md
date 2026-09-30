# braid Helm chart(T14.4)

braid 交付栈的 Kubernetes 形态,与 `deploy/prod.docker-compose.yml` 交付面
等价(设计决策真源:`docs/DECISIONS.md` D-2026-09-30-t144-helm)。默认
values 渲染 = compose 默认面:**逐组件 env 键集、对象清单、零宿主暴露、
Redis 契约(noeviction + AOF everysec)**;等价性由
`deploy/chart/tests/verify.sh` 的 compose 对拍断言执法(compose 改键即红,
零 fixture 漂移),CI `helm` job 同口径(D- 决策 1:静态门,弃 kind)。

## 对象映射(compose 服务 → chart 对象)

| compose 服务 | chart 对象 | 说明 |
|---|---|---|
| mysql | Deployment + Service + PVC(`*-mysql-data`) | 单副本 Recreate;MYSQL_DATABASE=braid 钉死;探针 = mysqladmin ping(就绪 3s/3s/30;liveness 放宽,见边界 3) |
| redis-queue | Deployment + Service + PVC(`*-redis-queue-data`) | args 契约:策略/AOF 键不可调,仅 maxmemory 体量可调 |
| migrate | Job(hook:post-install + pre-upgrade,weight −2) | 幂等(ErrNoChange no-op);尾参 = `migrate.args` |
| grants | Job(hook:post-install + pre-upgrade,weight −1) | grants.sh 于迁移后执行,幂等(CREATE USER IF NOT EXISTS) |
| api | Deployment + Service(ClusterIP) | 公开面经反代/Ingress 另接 |
| worker / relay / reaper | Deployment | 副本数 values 可调(见边界 1) |
| (.env 注入面) | Secret(`*-secret`) | values→Secret;existingSecret 可选路 |

两 PVC 带 `helm.sh/resource-policy: keep`(见边界 5)。

## 快速使用

```sh
# 1. 构建并推送交付镜像(一镜像四入口;含 /delivery 烘焙面)
docker build -t <registry>/braid:prod . && docker push <registry>/braid:prod

# 2. 写 values(必填清单见下;选通面默认关)
cp deploy/chart/values.yaml my-braid-values.yaml  # 编辑

# 3. 安装(慢存储 mysql 首启初始化可超缺省 5m,建议显式放宽 timeout)
helm install braid deploy/chart -n braid --create-namespace \
  -f my-braid-values.yaml --set image.repository=<registry>/braid --timeout 10m
```

**首装序**(hook 相位 = post-install,审计收口 ①,详见 D- 条目):组件
Deployment 与 migrate/grants Job 并行创建——hook 追平(迁移 + 授权完成)
前组件 crash-loop 自愈(缺表/缺授权即拒启,属一次性窗,起回即正常);
migrate/grants Job 的 wait-mysql initContainer 等 mysql 就绪。

**升级序**(`helm upgrade braid deploy/chart ...` 同 values):pre-upgrade
hook 先跑迁移与授权(幂等),**再**应用新资源——迁移执行时旧 Deployment
仍跑旧二进制,即 UPGRADE.md §2 的 N-1 混跑窗(schema > binary 仅 warn):
迁移只增量、破坏性迁移须两阶段、升级前备份(§1 第 2 步)的维护纪律在
chart 面同样适用。**dirty 恢复**(UPGRADE.md §4 的 chart 形态;Job 模板
创建后不可变,`kubectl edit` 路不通):

```sh
helm upgrade braid deploy/chart -f my-braid-values.yaml \
  --set 'migrate.args[0]=force' --set 'migrate.args[1]=<N>' --timeout 10m
# 恢复后把 values/命令行改回缺省 up(migrate.args: ["up"])
```

## 必填 values(缺即渲染失败 = compose `${VAR:?}` fail-fast 对齐)

| values 键 | compose 键 | 说明 |
|---|---|---|
| `secrets.mysqlRootPassword` | MYSQL_ROOT_PASSWORD | MySQL root(mysql 容器 + migrate/grants Job 消费) |
| `secrets.db{Api,Worker,Relay,Reaper}Password` | BRAID_DB_*_PASSWORD | 四组件账号;字符集限 [A-Za-z0-9._~@+%-](grants.sh 门,越界即拒) |
| `secrets.authJwtSecret` | BRAID_AUTH_JWT_SECRET | openssl rand -hex 32 |
| `secrets.llmApiKey` | BRAID_LLM_API_KEY | 仅经 env 注入 |
| `config.llmBaseUrl` / `config.llmPriceTable` | BRAID_LLM_BASE_URL / _PRICE_TABLE | llm 组生产装配必需 |

可选组(smtp / 工具白名单 / TRUSTED_PROXIES / toolTokenSecret)语义与
`deploy/.env.example` 同源:空 = 相应降级或不注册,半配置启动即败由进程
承载。

## 密钥面

- values 默认值恒空(密钥不进 chart 文本 / 不进 git;gitleaks 面恒绿);
  渲染期 `required` 执法。
- 组件 DSN 整串进 Secret(内嵌密码,host = release 限定 Service 名)。
- `secrets.existingSecret` 选路:设则不渲染 `*-secret`,全部对象引用该
  既有 Secret。**键名契约**:`mysql-root-password`、
  `db-{api,worker,relay,reaper}-password`、`auth-jwt-secret`、
  `llm-api-key`、`{api,worker,relay,reaper}-dsn`、`tool-token-secret`、
  `smtp-password`、`dbquery-dsn`(T10.4,空 = 不注册)、`oidc-client-secret`。DSN 的 host 段 = `<fullname>-mysql:3306`,
  其中 fullname 见 `_helpers.tpl`(release 名含 `braid` 子串时 = release 名
  本身——如 `helm install braid` 的 host 是 `braid-mysql:3306`;否则 =
  `<release>-braid`,如 release `prod` → `prod-braid-mysql:3306`)。

## 选通面(默认关;开启才渲染对应键,默认面保持逐键等价)

### 沙箱与 egress(D-t102 拓扑,D- 决策 2)

`tools.sandbox.enabled: true` → worker 注入 `BRAID_TOOL_SANDBOX` 及配额/
egress 键(env 词表单源复用,零新拓扑键);`tools.dockerSocketMount: true`
→ worker 挂载 host `/var/run/docker.sock`。启用后,在**有 docker daemon
的节点、且 worker 具备 socket 读写权限**时,worker 自管 `--internal` 网络
与进程内 CONNECT-only 代理(deploy/README.md §9)原样工作。权限通路:
worker 以 uid 65532 跑,标准节点的 socket 属 root:docker——必须把
`tools.dockerSocketGid` 配成节点 docker 组 GID(查法:`stat -c %g
/var/run/docker.sock`),否则挂载形同虚设。边界:

- docker.sock = 节点 root 等价面;仅在高信任节点开启,默认关。
- worker 副本 >1 且 egress 启用 = 同网撞端口拒启(D-t102 登记④,单
  worker 现状)。
- §9 前置契约的 K8s 原生形态(NetworkPolicy + 代理 sidecar/独立端点)
  需 Go 面改动,与「零 Go 代码」冲突 → **P1 deferred**(D- 决策 2 登记)。

### OIDC(T11.1 键组,compose 未接线)

`oidc.enabled: true` → api 注入五键组;issuer/clientId/redirectUrl 与
`secrets.oidcClientSecret` 为渲染期 required(缺即 helm 失败),claim 有
缺省;discovery 启动期拉取(issuer 不可达 = 进程启动失败)。

## 已知边界(登记,非缺口)

1. **副本 × 雪花**:api/worker/reaper 副本 >1 需外部雪花节点号分配
   (Deployment 无稳定 ordinal,缺省派生必撞号 = 业务表 PK 1062);默认 1
   = compose 等价。relay 无生成器,扩副本语义最简。
2. **首装混跑窗**:hook 相位 post-install 下,组件先于 migrate/grants
   创建并 crash-loop 自愈(缺表/缺授权即拒启;一次性,非每升级);升级序
   不受影响(迁移先行)。这是 Helm 生命周期下的结构性代价(D- 审计收口①,
   pre-install 位会因依赖未建而首装死锁)。
3. **liveness 语义超出 compose 面**:compose healthcheck 失败不杀容器,
   K8s liveness 杀——mysql liveness 已加 initialDelay + 放宽预算消化首启
   初始化与 InnoDB crash recovery(readiness 保持 3s/3s/30 同参)。
4. **重试语义偏差**:migrate/grants Job backoffLimit 3 = 失败至多 4 次
   (compose 单次);幂等面下安全。
5. **uninstall / 数据卷**:两 PVC 带 `helm.sh/resource-policy: keep`——
   `helm uninstall` **不**回收数据卷(= `docker compose down` 不带 `-v`
   同口径),重装复用既有数据;hook Job(migrate/grants)残骸同理不被
   release 回收,下次 hook 的 before-hook-creation 会先清再建。
6. **rollback 不跑 hook**:`helm rollback` 不执行 pre-upgrade/pre-install
   hook,迁移不随回滚倒退(schema 恒前向,回滚后 = §2 warn 窗;下次
   upgrade 幂等补跑)。
7. **组件无容器级探针** = compose 同口径(存活面 = /healthz + metrics 由
   外部监控探,deploy/README.md §6);preflight 失败 → 容器退出 →
   CrashLoopBackOff 响亮可见。
8. **metrics 不建 Service**:监控栈由运维侧另接(§6 同口径;经
   `kubectl port-forward` 或自建 Service/ServiceMonitor)。
9. **kind 实例化非验收面**(D- 决策 1):验收 = 静态门;组件运行时正确性
   由既有 go test 承载(同二进制同 env)。
10. MySQL/Redis 为 chart 内单实例(与 compose 等价);外置 DB/Redis 的
    接线面(values 化目标地址)按需另立决策。

## 验证(本地与 CI 同口径)

```sh
deploy/chart/tests/verify.sh   # 8 断言:lint --strict / kubeconform /
                               # compose 对拍 / 对象清单+总数闭环 /
                               # Redis 契约 / ClusterIP 集合相等 /
                               # required 执法 / values 零密钥默认
```

工具经钉版 docker 镜像(alpine/helm:3.16.4、ghcr.io/yannh/kubeconform:
v0.6.7、mikefarah/yq:4.44.3;kubeconform schema 钉 1.31.0)——依赖本机
docker 与出网拉 schema,CI `helm` job 同脚本零 Secrets。
