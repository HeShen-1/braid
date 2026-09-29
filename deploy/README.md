# braid 部署配额与容量口径

T7.1 配置体系交付的配额表。**数字同源声明**:本文全部默认值与代码同源
(`internal/db` DefaultPoolConfig / 各包 DefaultConfig / `deploy/prometheus/alerts.yml` /
`deploy/dev.docker-compose.yml`),代码漂移时本文必须同步修;两处冲突时以代码为准。

env 旋钮全表与逐键注释见同目录 `.env.example`;换装点纪律 = 四入口
(api/worker/relay/reaper)各自 `ConfigFromEnv` 是唯一 env 解析点,cmd 层
不直读 `BRAID_*`(T7.1 收编,`internal/db/testenv.go` 测试基建豁免)。

## 1. MySQL 连接池配额

三键四入口共用:

| 键 | 默认 | 语义 |
|----|------|------|
| `BRAID_DB_MAX_OPEN` | 25 | 单进程最大打开连接数(软上限 100:超限收敛到 100 + 启动 warn,见下方公式锚) |
| `BRAID_DB_MAX_IDLE` | 25 | 空闲保留上限(校验 0 ≤ idle ≤ open) |
| `BRAID_DB_CONN_MAX_LIFETIME` | 5m | 单连接最长复用时长(消化 MySQL wait_timeout 与 LB 轮转陈旧连接) |

对账表(默认姿态):

| 入口 | MAX_OPEN 默认 |
|------|---------------|
| api / worker / relay / reaper | 各 25 |
| **Σ 常驻入口** | **100** |

MySQL 侧:单实例默认 `max_connections` = 151(MySQL 8.0 出厂值;dev compose
未覆写)。容量公式:

```
Σ BRAID_DB_MAX_OPEN(全部常驻入口副本) ≤ max_connections − 预留(≥30)
```

预留口径 = 运维/监控连接 + golang-migrate CLI 短连接(迁移器逐库开新连接,
不走进程池)。默认 100 + 30 = 130 < 151,余 21。扩容入口副本数时同轮检
MySQL 侧,并遵守**只升不降**:`max_connections` 下调会杀既有连接(含在途
事务),必须停机窗口内操作。

单进程软上限(T12.2,`db.MaxOpenSoftCap` = 100):`BRAID_DB_MAX_OPEN`
配置值超过 100 时解析层收敛到 100 并打启动 warn(自伤型误配护栏——单
进程超过全 fleet 默认预算即越过公式默认头寸)。真要单进程 >100 = 同轮
抬 MySQL `max_connections` 并自核上方公式;改上限值属部署契约变更,走
DECISIONS D- 条目。

池饱和观测(T6.4 指标面 + T7.1 池观测,四入口 30s 常量周期采集):

| 指标 | 口径 |
|------|------|
| `braid_db_pool_max_open_connections` | 生效配置可见性(启动 slog 亦回显三值) |
| `braid_db_pool_in_use_connections` | 饱和度分子;持续 ≈ max_open = 池打满 |
| `braid_db_pool_open_connections` / `idle` | 池形态(open = in_use + idle) |
| `braid_db_pool_wait_total` | 累计等待获取数;持续增长 = 饱和,升 `BRAID_DB_MAX_OPEN`(先对上面的公式) |

## 2. 队列 Redis 配额

dev 栈契约(`dev.docker-compose.yml`;生产同契约,T7.3 preflight 硬拒
relay/worker 侧破约):`maxmemory` 256mb + **noeviction** + AOF everysec。

- noeviction 是硬要求:写满后入队**响亮失败**(OOM),绝不逐出任务
  (任务状态真相源在 MySQL,Redis 仅队列;AOF everysec 官方 ~1s 丢失窗)。
- 内存水线告警:`used/max > 0.8` 扩容/排水行动线(`alerts.yml`
  QueueRedisMemoryWatermark 同口径);观测 = `braid_redis_memory_used_bytes` /
  `max_bytes` + `braid_asynq_queue_tasks`(队列深度)。
- 热缓存实例(6380)allkeys-lru 不持久化,与队列实例**物理分离**——缓存
  压力不会逐出任务。

## 3. 保留期配额(存储增长面)

| 面 | 旋钮 | 默认 | 机制 |
|----|------|------|------|
| audit_log | `BRAID_REAPER_AUDIT_RETENTION` | 2160h(90d) | janitor 分区 DROP(被活执行钉住的分区顺延) |
| login_audit | `BRAID_REAPER_LOGIN_AUDIT_RETENTION` | 2160h(90d) | janitor 按 event_at 批量 DELETE(单批 500 / 单轮 10000 / 批间 100ms;≥24h 校验) |

容量估算口径:audit_log = 峰值事件率 × retention(分区物理文件粒度);
login_audit 写入被登录限流钳制(默认 10/min/IP),量级天然有界。两旋钮
均 ≥24h 启动即败(防误配一天清光审计)。

## 4. api HTTP 超时配额

| 键 | 默认 | 封的攻击面 |
|----|------|-----------|
| `BRAID_API_READ_HEADER_TIMEOUT` | 10s | slowloris 慢速头部 |
| `BRAID_API_READ_TIMEOUT` | 60s | 请求体(1MiB DSL 上限)无界读取 |
| `BRAID_API_WRITE_TIMEOUT` | 30s | 慢速读响应的客户端 |
| `BRAID_API_IDLE_TIMEOUT` | 120s | keep-alive 空闲连接堆积 |

公开面(BRAID_API_ADDR 放开 + 反代)部署不建议放宽;收窄前用最大 DSL
压测(整请求 60s 对固定 JSON API 是宽余量,不是冗余)。优雅停机窗
(10s)与 OTel 排空窗(2s)是停机常量,非旋钮。

## 5. 账号授权矩阵(生产账号分离,T7.4 定稿)

四组件账号 × 全表最小授权。**同源声明**:矩阵 = 2026-09-27 对
`internal/*` 非测试 SQL 语句集的逐包 survey(组件装配链 → 语句 → 权限,
交叉审计核验);**可执行真源 = `deploy/grants.sh`**(交付栈 grants
one-shot 于迁移后自动执行),手工路径(已有卷/扩权/改密)见
`deploy/UPGRADE.md §3`。代码新增 SQL 写面时本表与 grants.sh 必须同步修。
**口径修正(6 视角审计)**:按**调用链可达**而非 import 可达核权——
scheduler.Instantiate 只被 api 调用、outbox 状态迁移原语只在 relay、
audit 暂存发射只在 worker 路径;初版矩阵按包归属多授 7 cell
(worker/reaper 的 execution/node INSERT、reaper 的 node_attempt INSERT /
outbox UPDATE / audit_pending INSERT)已全部收回。

| 表 | api | worker | relay | reaper |
|----|-----|--------|-------|--------|
| workflow_definition | SELECT,INSERT,UPDATE | SELECT | — | SELECT |
| workflow_execution | SELECT,INSERT | SELECT,UPDATE | — | SELECT,UPDATE |
| node_execution | SELECT,INSERT | SELECT,UPDATE | — | SELECT,UPDATE |
| node_attempt | — | SELECT,INSERT,UPDATE | — | SELECT,UPDATE |
| approval_decision | SELECT,INSERT | SELECT | — | SELECT |
| outbox | INSERT | INSERT | SELECT,UPDATE | SELECT,INSERT,DELETE |
| audit_log | SELECT,INSERT | SELECT,INSERT | — | SELECT,INSERT,ALTER,DROP |
| audit_pending | — | INSERT | — | SELECT,DELETE |
| chain_heads | SELECT,INSERT,UPDATE(last_id,last_row_hash) | SELECT,INSERT,UPDATE(last_id,last_row_hash) | — | SELECT,INSERT,UPDATE,DELETE |
| login_audit | INSERT | — | — | SELECT,DELETE |
| users / user_role | SELECT | SELECT | — | — |
| schema_migrations | SELECT | SELECT | SELECT | SELECT |

- **append-only 铁律与启动断言**:api/worker 对 audit_log 仅
  INSERT/SELECT;login_audit 业务面(api)仅 INSERT。`preflight.
  CheckAuditAppendOnly`(T7.4 扩面纳 login_audit)在四入口启动断言
  UPDATE/DELETE 违约即拒启;唯一豁免 = reaper 对 login_audit 的表级
  DELETE(保留期清理)。schema 级授权(`GRANT ... ON braid.*`)覆盖
  audit_log,豁免形态照拒——组件写权限必须表级精确授予。
- **chain_heads 是簿记面不是审计面(T9.3,D-2026-09-29-t93)**:审计
  哈希链的 per-execution 锚(head 行锁 = 串行化点,verified 轮换戳 =
  全链复走触发锚,broken 闩 = fail-closed 执法位),UPDATE 是设计语义
  ——不在 CheckAuditAppendOnly 断言表集 {audit_log, login_audit} 内。
  **列级最小授权**:api/worker 的 UPDATE 限 (last_id, last_row_hash)
  两列(写路径唯一更新面),broken_*/verified_* 对它们不可写 =
  「闩复位 = 人工」的权限面承载;reaper 全行 UPDATE(校验轮换/落闩/
  推链尾)+ DELETE(janitor 分区回收级联:证据窗口消亡 ⇒ 链义务解除,
  防旧头误闩 + 头表无限增长)+ INSERT(自身是 attempt.reaped 等同步
  Append 发射面,head 懒建可达);relay 零权限(零审计写)。
- **audit_log 的 ALTER+DROP(reaper 专属)**:分区水位作业(ADD/DROP
  PARTITION)——MySQL 对 `DROP PARTITION` 要求 ALTER+DROP 双权限,缺
  DROP 则 janitor 回收路径 1142(T7.4 交叉审计发现,原表漏 DROP)。DROP
  权限不触发 preflight 断言(断言面是 UPDATE/DELETE,DROP 是独立权限)。
- **UPDATE/DELETE 的谓词列需要 SELECT**(MySQL 权限规则,T7.4 升级演练
  实证):一切带 WHERE 的写语句,其条件列对账号须有 SELECT——矩阵中
  reaper 对 login_audit 的 SELECT 即服务保留期 DELETE 的 `event_at` 谓词
  (无 SELECT 则 purge 报 1143)。
- **空格 = 无权限是设计而非遗漏**:relay 对 audit_log 零权限(零审计写);
  api 不触 audit_pending(api 审计全走同步 `audit.Append` 直写 audit_log,
  暂存面只 worker/reaper——T7.4 修正原表误列的 api INSERT);api 对
  login_audit 仅 INSERT(查询面 P1)。
- **无需显式授权的面**:preflight 的 INFORMATION_SCHEMA 探测(I_S 按
  账号既有权限过滤可见行)、`SELECT CURRENT_USER()/DATABASE()`、
  `SELECT NOW(3)` 等会话级语句。
- 漏授权的症状:组件启动 preflight 红(版本/权限断言)或运行期 1142
  响亮报错(如 janitor 缺 login_audit DELETE → 每轮重试,错误隔离不
  brick)。

## 6. 指标 exposition 端口

api 的 `/metrics` 恒挂 `BRAID_API_ADDR`(与 /healthz 同回环绑定);worker/
relay/reaper 经 `BRAID_METRICS_ADDR`(空 = 不启)。无认证 exposition,
只绑回环或留内网(反代只转发 /api/v1 与 /ui/)。

## 7. 工具沙箱镜像(T10.1)

`braid-tool-sandbox:1`(缺省引用,`BRAID_TOOL_SANDBOX_IMAGE` 可覆写)是
file_read/http 的容器载体镜像;worker `BRAID_TOOL_SANDBOX=docker` 启动时
SandboxProbe 要求镜像在场——**daemon 永不 pull**,缺失即拒启(错误消息
携带构建命令)。

**构建**(context = repo 根;宿主构建 + COPY-only,测试路径免拉 golang
镜像;**需 BuildKit**——builder stage 非 prod 祖先,BuildKit 按依赖跳过
之;legacy builder 线性执行全部 stage,缺省构建会在 sandbox-e2e 的
CA COPY 处中止(repo 根 context 无 ephemeral CA 成员)——即缺 buildx 时
缺省构建必败。行为依引擎版本而异(docker-ce 23+ 缺插件可回退 daemon 侧
BuildKit),统一口径 = 装 buildx:docker-ce 自带;Ubuntu docker.io 源缺装
`sudo apt install docker-buildx`,或无 sudo 时用户级安装
`~/.docker/cli-plugins/docker-buildx`)。两步以 `&&` 链接执行——repo 根
的 `braid-sandbox-exec` 产物跨分支留存(.gitignore 已忽略),漏跑 go
build 会把陈旧二进制静默打进镜像:

```
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
  -o braid-sandbox-exec ./cmd/braid-sandbox-exec \
&& docker build -f deploy/sandbox.Dockerfile -t braid-tool-sandbox:1 .
```

`deploy/sandbox.Dockerfile` 三 stage:build(源构建纪律真源,非 prod
祖先,缺省构建不执行)、sandbox-e2e(hermetic e2e 专用 `--target
sandbox-e2e`,独立 tag 不覆写缺省引用,测试收尾 `docker rmi`)、prod
(缺省 target = 最后 stage,零注入)。基座 alpine:3.22 与 builder
golang:1.27-alpine 均按 digest 钉死(钉法先例 = dev.docker-compose.yml
asynqmon 条目);运行层二进制恒从 build context COPY 宿主产物(与根
Dockerfile 的 in-image 源构建区分)。拓扑不变量由
`internal/tools/sandbox_image_test.go` 机械锚钉死(锚红 = 拓扑被意外
改动;显式变更随变更同步更新锚)。

**企业私 CA 通道(同通道重建镜像)**:信任集扩张只经镜像构建,运行期
零通道——容器 env 白名单恰 PATH/LANG(`SSL_CERT_FILE`/`SSL_CERT_DIR`
被白名单结构性挡死)、stdin 信封、挂载均不承载 CA。企业私 CA 接入 =
在运行层 stage COPY 企业 CA 证书 PEM 并 `cat >>
/etc/ssl/certs/ca-certificates.crt` **append(只增不减,基座束保留——
clobber 式整体替换会静默断全部公网 https 出站)**,操作先于 USER 65532;
注入形态参照 sandbox-e2e stage,随后重建 `braid-tool-sandbox:1` 重新
部署。禁止经任何运行期通道(env/挂载/信封)注入 CA;该变更会触发上述
机械锚(prod 段唯一 COPY = helper 二进制、零束操作、零 ENV 由锚钉死),
属预期,锚随显式变更同步更新。

## 8. 工具包签名(T10.3)

工具定义的发布物 = **签名的工具定义清单**(tool package):工具名与安全
相关配置(http 端点白名单、file_read 目录白名单)整体进签名面,Ed25519
对清单**原始字节**签名(签什么验什么,无 canonical 重排)。发布侧持
私钥,加载侧(api 与 worker 经 BuiltinToolsFromEnv 单源装配)持公钥
验签;三键均 base64(StdEncoding),值不含换行:

```
BRAID_TOOL_SIGNING_PUBLIC_KEY=  # 32B Ed25519 公钥
BRAID_TOOL_PACKAGE=             # 清单 JSON 原始字节
BRAID_TOOL_PACKAGE_SIGNATURE=   # 64B Ed25519 签名
```

**fail closed 纪律(无降级开关;降级须 D- 拍板)**:

| pubkey | package/signature | 内置工具键 | 结果 |
|--------|-------------------|-----------|------|
| 未设 | 全缺 | 任意 | 内置 env 路径现状(签名未部署) |
| 未设 | 任一在场 | — | 启动败(无钥不可验) |
| 设了 | 半配 | — | 启动败(半配置即败) |
| 设了 | 全配 + 验签过 | 空 | 清单为唯一工具来源 |
| 设了 | 验签/清单校验失败 | — | 启动败(验签失败即拒) |
| 设了 | 任意 | 非空 | 启动败(unsigned 源即拒——开签名忘摘旧键 = 响亮拒绝,配置漂移防) |

清单 v1(字段面封闭:未知工具/未知字段/Endpoints 与 Dirs 交叉携带/
尾随数据/"tools" 键缺席或 null 均拒——空发布必须是显式 {};工具词表 =
http/file_read,t13.2 MCP 接入时扩展):

```json
{"version":1,"tools":{
  "http":{"endpoints":{"docs_api":"https://docs.example.com"}},
  "file_read":{"dirs":["/var/braid/docs"]}
}}
```

JSON 语义(Go encoding/json,加载侧不重排不归一):重复键 last-wins、
字段名匹配大小写不敏感——清单确定性由签名人单源产出保证,签名绑原始
字节。

**发布命令**(openssl;私钥永不进仓/日志/部署面,公钥非秘密但仅经 env
注入;`-w 0` 出单行 base64——env 值不含换行):

```
# 0. 清单写入 package.json(签名绑文件字节,提交前后别让编辑器改行尾)
# 1. 密钥对(一次性;SPKI DER 44B 取尾 32B = 原始公钥)
openssl genpkey -algorithm ed25519 -out toolpkg_key.pem
openssl pkey -in toolpkg_key.pem -pubout -outform DER | tail -c 32 | base64 -w 0
# 2. 签名(须 OpenSSL ≥ 3.0:pkeyutl -rawin,1.1.1 无此选项)
openssl pkeyutl -sign -inkey toolpkg_key.pem -rawin -in package.json | base64 -w 0 > sig.b64
# 3. BRAID_TOOL_PACKAGE 的值 = 清单字节 base64
base64 -w 0 package.json
# 4. 发布侧自检(签坏了在这里红,别等 api/worker 启动拒)
openssl pkeyutl -verify -pubin -inkey <(openssl pkey -in toolpkg_key.pem -pubout) \
  -rawin -in package.json -sigfile <(base64 -d sig.b64)
```

加载侧验证与命令行自检同门:Go `ed25519.Verify` 对 openssl
`pkeyutl -rawin` 原始签名逐字节兼容(双向实测,且 openssl 产出的真实
golden 向量已入 `internal/tools/toolpackage_test.go` 持续验证)。部署后
api/worker 启动即验,失败 = 拒启(错误带分量定位,不含密钥材料)。换钥 =
新钥重签 + 全部署点同步换三键(无过渡双钥;轮换策略需要时 D- 拍板)。

## 9. 沙箱 egress 默认拒绝拓扑(T10.2)

`BRAID_TOOL_SANDBOX=docker` 下 http 工具容器的出网通道(与 §7 镜像同一
沙箱面;file_read 恒 `--network none` 不变):

- **内网**:worker 启动时幂等创建 `--internal` docker 网络(缺省名
  `braid-sbx-egress`,`BRAID_TOOL_SANDBOX_EGRESS_NETWORK` 可覆写;daemon
  ≥ 20.10,internal 隔离语义下限)。容器在内网上唯一可达面 = 网关地址,
  直连外网/宿主其他地址在网络层结构性拒绝——默认拒绝由拓扑保证,不是
  进程内策略保证。
- **代理**:worker 进程内 CONNECT-only 代理,绑内网网关
  `BRAID_TOOL_SANDBOX_EGRESS_PORT`(缺省 9210;0 = 内核指派)。策略:
  host(:port) 白名单单源派生自 `BRAID_HTTP_ENDPOINTS`(或验签工具包,
  §8),端口级管控在此收口;DNS 全在代理侧解析,任一解析 IP 落 SSRF
  禁段整体拒(fail-closed),拨已解析 IP(DNS rebinding 无二次解析窗)。
  明文 http 不转发(CONNECT-only),端到端 TLS 在容器与目标之间,代理
  零明文可见。
- **地址流**:worker 启动自算代理地址并写
  `BRAID_TOOL_SANDBOX_EGRESS_ADDR`(进程内传递,非手配键),经 stdin
  信封进容器——容器 env 恒无 `BRAID_*` 键(§7 §5 白名单不变量)。
- **运维**:多 worker 副本同网同名会撞端口拒启(单 worker 现状);网络
  属 daemon 持久对象,worker 重启幂等复用(daemon 重启若变更网关,worker
  下次启动重读重绑)。拒绝姿态拆毁形态(同名网络非 `--internal`)启动
  即拒。
- **保证的适用域(登记,对抗审查收口)**:「结构性拒绝」限 **off-host 方向**、
  且以 **docker 托管 iptables 的宿主** 为前提——daemon 配 `--iptables=false`、
  firewalld 接管、或管理员把 FORWARD 链改回 ACCEPT 的旧 daemon(28 前无显式
  DOCKER-INTERNAL DROP)窗内,拒绝强度退化为宿主转发策略。rootless daemon 不
  支持(网关 IP 活在 rootlesskit netns,worker bind 即 EADDRNOTAVAIL 拒启,
  形态响亮);egress 网禁用 `gateway_mode=isolated`(Docker 28+ 选项,会删宿主
  侧网关地址,与本设计「宿主侧监听」前提互斥)。
- **t14.4(Helm)前置契约**:k8s 化时「网关 + 进程内代理」对应
  egress NetworkPolicy + 代理 sidecar/独立端点;env 键词表与
  `EgressAllowFromEndpoints` 单源在此两处复用,不再新增拓扑键。
