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

**多副本显式算式(T15.6,R7 注记;D-t151 决策 7)**:全部常驻入口各 N
副本时——

```
max_connections ≥ 100 × N + 30
```

MySQL 出厂 `max_connections` = 151,**N=2 即超**(需 ≥230):扩副本前
必须同轮抬 `max_connections`,否则新副本的池在连接第 152 位起拿不到
连接。观测对拍:`braid_db_pool_*`(下表)按副本分 instance 曝光,饱和
会先出现在单个副本上。

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
| workflow_execution | SELECT,INSERT,UPDATE¹ | SELECT,UPDATE | — | SELECT,UPDATE |
| node_execution | SELECT,INSERT,UPDATE¹ | SELECT,UPDATE | — | SELECT,UPDATE |
| node_attempt | SELECT,UPDATE¹ | SELECT,INSERT,UPDATE | — | SELECT,UPDATE |
| approval_decision | SELECT,INSERT | SELECT | — | SELECT |
| outbox | INSERT | INSERT | SELECT,UPDATE | SELECT,INSERT,DELETE |
| audit_log | SELECT,INSERT | SELECT,INSERT | — | SELECT,INSERT,ALTER,DROP |
| audit_pending | INSERT² | INSERT | — | SELECT,DELETE |
| chain_heads | SELECT,INSERT,UPDATE(last_id,last_row_hash) | SELECT,INSERT,UPDATE(last_id,last_row_hash) | — | SELECT,INSERT,UPDATE,DELETE |
| login_audit | INSERT | — | — | SELECT,DELETE |
| login_throttle | SELECT,INSERT,UPDATE(window_start,count,locked_until,updated_at) | — | — | SELECT,DELETE |
| users / user_role | SELECT | SELECT | — | — |
| snowflake_node_lease | SELECT,INSERT,UPDATE,DELETE | SELECT,INSERT,UPDATE,DELETE | — | SELECT,INSERT,UPDATE,DELETE |
| schema_migrations | SELECT | SELECT | SELECT | SELECT |

¹ api 的 node_execution/node_attempt UPDATE 是**锁定读权限面**,workflow_execution UPDATE 是**回滚/续跑缝的状态翻转面**(engine RestartExecution,对抗审计 HIGH 补)(T15.6
演练实证):MySQL 的 `SELECT ... FOR UPDATE` 要求对应表写权限——审批决定
事务的 TOCTOU 守卫(锁 node_execution)与 rollback/resume 缝(engine node
CAS + node_attempt 锁读)都在 api 连接上执行,无 UPDATE =
least-privilege 下 1142(t13 测试 root 连接不可见)。api 对两表无直接
UPDATE 语句;权限跟随锁的写意图。² 回滚缝重派节点的 node.state_changed 走 Enqueue
暂存(T6.1 批量事件族),flush 消费归 reaper——演练臂 ⑦ 实证。

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
- **snowflake_node_lease 是运行态簿记面(T15.2,D-2026-10-01-t151
  决策 2)**:雪花节点号租约仲裁(acquire INSERT / 复占与过期接管
  UPDATE / auto 池扫描 SELECT / SIGTERM 优雅释放 DELETE——DELETE 是
  设计语义,行生命周期 = 进程生命周期)。三账号同面(api/worker/
  reaper);relay 无雪花生成器零面。不在 CheckAuditAppendOnly 断言表集,
  preflight「预登记未断言」台账(internal/preflight/mysql.go);过期判定
  全 DB 端 NOW(3),kill -9 残窗 ≤ TTL(缺省 30s),runbook 清行应急见
  UPGRADE.md §7。
- **login_throttle 同为簿记面(T11.3,D-2026-09-29-t113 #2)**:登录
  防御状态(连败锁定 + IP 固定窗口,多副本共享真相源),UPDATE 是设计
  语义、api 无 DELETE(成功重置走 UPDATE,且 UPDATE 列级收窄到四数据
  列——kind 仅 INSERT 写);超龄行由 reaper janitor 回收(常量 30d
  horizon,`updated_at` 谓词 + 锁定期未满行守卫)。
- **列级最小授权**:矩阵中括号列集(如 chain_heads 的
  `(last_id,last_row_hash)`、login_throttle 的
  `(window_start,count,locked_until,updated_at)`)是 GRANT 的字面
  列集——不变量列(kind、broken_*/verified_* 等)对写路径结构性
  不可达,grants.sh 与本矩阵同源。chain_heads 的收窄由来(T9.3):
  api/worker 的 UPDATE 限 (last_id, last_row_hash)
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

### 7.1 沙箱 owner 部署契约(T15.3,D-t151 #5)

**owner 部署契约**(多 worker 同 daemon 不互杀的前提,升格为部署面
义务):worker 身份 = WorkerID(`BRAID_WORKER_ID` 显式或 hostname 兜底),
沙箱容器打 `braid.sandbox.owner=<WorkerID>` label,worker 启动清扫
**只清自家**(代码路径唯一形态,全局清扫无函数面)。因此要求:

- 「owner 在并发活体间唯一且跨重启稳定」:prod compose 已显式钉
  `hostname`(restart 与 recreate 均身份稳定,后者是 compose 缺省容器
  短 ID 必换身份的场景);多副本 compose 须**逐副本**显式
  hostname/BRAID_WORKER_ID;k8s 侧 pod 名并发活体间天然唯一、免设——
  Deployment 滚动重建换名,「跨重启稳定」半边不成立:新 pod 清扫不互杀
  旧 pod 在飞容器(owner 互异,互杀消除),旧 pod 非优雅死亡残留 = 带
  旧 pod 名 owner 的孤儿,归 runbook 按-owner 原语(身份可知)。
- `stop_grace_period ≥ 35s`(compose)/`terminationGracePeriodSeconds ≥ 35`
  (chart):停机宽限须盖过 `BRAID_WORKER_SHUTDOWN_TIMEOUT`(30s)的
  排水窗——缺省 10s/30s 会在排水中途被 SIGKILL,在飞沙箱容器变孤儿。
- 已知边界(登记不修,D-t151 #8-③):手工同 hostname ×2 等活体同 owner
  冲突会复活互杀面。本 lane 选型 = **部署契约承载**(零依赖);「sweep 前
  租约活性复核」留作 followup 位(t15.2 租约表合入后可补,读面依赖)。

kill -9 孤儿容器与泄漏 egress 网的**回收原语见 deploy/RUNBOOK.md §1**
(与 T15.4 泄漏网同节;按-owner 精确清 + 全局最后手段,含首个 T15.3
升级窗特例)。

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

| pubkey | package/signature | 内置工具键(含 BRAID_MCP_SERVERS/TOOLS;TOKEN_* 凭证键例外) | 结果 |
|--------|-------------------|-----------|------|
| 未设 | 全缺 | 任意 | 内置 env 路径现状(签名未部署) |
| 未设 | 任一在场 | — | 启动败(无钥不可验) |
| 设了 | 半配 | — | 启动败(半配置即败) |
| 设了 | 全配 + 验签过 | 空 | 清单为唯一工具来源 |
| 设了 | 验签/清单校验失败 | — | 启动败(验签失败即拒) |
| 设了 | 任意 | 非空 | 启动败(unsigned 源即拒——开签名忘摘旧键 = 响亮拒绝,配置漂移防) |

清单 v1(字段面封闭:未知工具/未知字段/Endpoints 与 Dirs 交叉携带/
尾随数据/"tools" 键缺席或 null 均拒——空发布必须是显式 {};工具词表 =
内置名 http/file_read/db_query ∪ MCP 声明族(T13.2,`mcp_servers` +
逐工具 `mcp` 引用;server 必须被引用、别名/注册名词形同内置、凭证恒
`BRAID_MCP_TOKEN_<ALIAS>` env 通道——签名工件零凭证;详见 D-t132)):

```json
{"version":1,
 "mcp_servers":{"weather":"https://mcp.example.com/mcp"},
 "tools":{
  "http":{"endpoints":{"docs_api":"https://docs.example.com"}},
  "file_read":{"dirs":["/var/braid/docs"]},
  "mcp_weather_lookup":{"mcp":{"server":"weather","tool":"lookup_weather"}}
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

- **内网(一 worker 一网,T15.4/D-t151 决策 5)**:worker 启动时幂等创建
  `--internal` docker 网络,网名 = 基段 + worker_id 实例段
  (`braid-sbx-egress-<sha256(worker_id) 前 8 hex>`;基段
  `BRAID_TOOL_SANDBOX_EGRESS_NETWORK` 可覆写,≤55 字符——实例名须落
  docker 网络名值域 ≤64;daemon ≥ 20.10,internal 隔离语义下限)。同
  worker_id 跨重启同名幂等复用;异 worker 各持独立 bridge 子网——独立
  网关 IP,同端口 9210 不撞,顺带 worker 间沙箱容器物理网络隔离。容器
  在内网上唯一可达面 = 本 worker 网关地址,直连外网/宿主其他地址(含
  他 worker 的沙箱网——同属宿主侧地址,隔离依据与下方「保证的适用域」
  同一前提与实证)在网络层结构性拒绝——默认拒绝由拓扑保证,不是
  进程内策略保证。
- **代理**:worker 进程内 CONNECT-only 代理,绑内网网关
  `BRAID_TOOL_SANDBOX_EGRESS_PORT`(缺省 9210;0 = 内核指派)。策略:
  host(:port) 白名单三源单派:`BRAID_HTTP_ENDPOINTS`(或验签工具包,
  §8)∪ MCP server 声明(T13.2,`EgressAllowFromMCPServers`——常规
  Allow 非 AllowInternal,私网禁段不豁免)∪ db_query DSN 钉死条目
  (§10);端口级管控在此收口;DNS 全在代理侧解析,任一解析 IP 落 SSRF
  禁段整体拒(fail-closed),拨已解析 IP(DNS rebinding 无二次解析窗)。
  明文 http 不转发(CONNECT-only),端到端 TLS 在容器与目标之间,代理
  零明文可见。
- **地址流**:worker 启动自算代理地址与实例网名,分别写
  `BRAID_TOOL_SANDBOX_EGRESS_ADDR` / `BRAID_TOOL_SANDBOX_EGRESS_NETWORK`
  (进程内传递,非手配键;后者覆写基段,容器 `--network` 位与建网/监听
  三面同源);代理地址经 stdin 信封进容器——容器 env 恒无 `BRAID_*` 键
  (§7 §5 白名单不变量)。
- **运维(daemon 维度)**:多 worker 副本各持实例网、同端口不撞——撞
  端口面解除 ≠ 副本 >1 全面就绪,沙箱清扫仍是 daemon 全局 label 面
  (互杀红线 R2,owner-scoped 清扫随 T15.3 落地后才开多副本)。同
  worker_id 二启 = 同网关同端口 bind 拒启——「owner 在并发活体间唯一
  且跨重启稳定」部署契约的执法形态;限**显式端口**,`EGRESS_PORT=0`
  内核指派 = dev/test 语义,不执法(同族登记边界;活体同 owner 冲突面
  见 D-t151 决策 8-③);网络属
  daemon 持久对象,worker 重启幂等复用(daemon 重启若变更网关,worker
  下次启动重读重绑);拒绝姿态拆毁形态(同名网络非 `--internal`)启动
  即拒。**地址池上界**:docker 缺省地址池 ≈31 个用户网/daemon(k8s
  host docker.sock = 节点级 daemon 共享,上界按节点计;扩容 =
  daemon.json `default-address-pools`);一 worker 一网 + 网络不自回收
  ⇒ worker 身份变更(容器 recreate/升级路径)每次泄漏一网,回收原语见
  deploy/RUNBOOK.md §1.1(孤儿容器清扫原语由 t15.3 落位,并节仲裁见
  RUNBOOK §1.2 注记)。并发容器配额
  `BRAID_TOOL_SANDBOX_MAX_CONCURRENCY` 是**进程级**语义(键注释见
  deploy/.env.example,单副本口径不变),daemon 维度总额 = Σ 各副本。
- **保证的适用域(登记,对抗审查收口)**:「结构性拒绝」限 **off-host 方向**、
  且以 **docker 托管 iptables 的宿主** 为前提——daemon 配 `--iptables=false`、
  firewalld 接管、或管理员把 FORWARD 链改回 ACCEPT 的旧 daemon(28 前无显式
  DOCKER-INTERNAL DROP)窗内,拒绝强度退化为宿主转发策略。rootless daemon 不
  支持(网关 IP 活在 rootlesskit netns,worker bind 即 EADDRNOTAVAIL 拒启,
  形态响亮);egress 网禁用 `gateway_mode=isolated`(Docker 28+ 选项,会删宿主
  侧网关地址,与本设计「宿主侧监听」前提互斥)。T15.4 复核:实例段网名不
  改变上述适用域——独立子网不豁免 rootlesskit netns 边界(rootless 拒启
  形态不变),受限/非托管 iptables 退化语义同判(D-t102 followup ⑦ 原样)。
- **t14.4(Helm)前置契约**:k8s 化时「网关 + 进程内代理」对应
  egress NetworkPolicy + 代理 sidecar/独立端点;env 键词表与
  `EgressAllowFromEndpoints` 单源在此两处复用,不再新增拓扑键。
- **t14.4 落地注记(D-2026-09-30-t144-helm)**:chart(deploy/chart)按
  docker.sock 选通面落地——env 键单源复用 + worker 可选挂载
  /var/run/docker.sock,拓扑在有 docker 的节点原样工作;上条的
  NetworkPolicy + sidecar K8s 原生映射需 Go 面改动,登记 P1 deferred。

## 10. db_query 内置工具(T10.4)

对运维钉死的**只读 MySQL 账号**执行参数化 SELECT(分析型查询;DSL 输入
`sql` + `args`,占位符 `?`)。三层只读纵深:客户端语句头词表
(SELECT|WITH)+ 会话 `transaction_read_only=1`(构造期强制)+ 账号
GRANT SELECT-only(下方 SQL)。参数化为结构保证:`interpolateParams=false`
+ `multiStatements=false` 强制覆写运营方 DSN 同义值,字符串字面量/注释/
多语句在客户端门被拒(值只能经 `args` 占位符入协议)。

### env 键(单个;未设 = 不注册 db_query)

```sh
# deploy/.env(值含 () 时整体加单引号,见 .env.example 文件头书写规则)
BRAID_DBQUERY_DSN='ro:密码@tcp(db.internal:3306)/analytics'
```

- 只读账号创建(部署纪律,与业务账号同 one-shot 面;密码字符集建议对齐
  grants.sh 限界 `[A-Za-z0-9._~@+%-]`):

```sql
CREATE USER IF NOT EXISTS 'braid_ro'@'%' IDENTIFIED BY '<随机口令>';
GRANT SELECT ON `analytics`.* TO 'braid_ro'@'%';
-- 不要 FILE/EXECUTE/写权限;INTO OUTFILE/DUMPFILE 另有客户端门先拒
```

- DSN 可携带驱动参数(如 `tls=skip-verify`、`timeout=3s`),工具只强制
  安全三参数;**凭证不进日志/审计/错误消息**(解析失败只报「invalid DSN
  (value withheld)」)。
- 沙箱模式(BRAID_TOOL_SANDBOX=docker)下:db_query 进容器执行,MySQL
  连接经 egress 代理 CONNECT 隧道——DSN 的 host:port 自动进代理
  AllowInternal 钉死条目(豁免私网/环回禁段,metadata/link-local 仍拒;
  D-2026-09-30-t104-dbquery-egress),**无需也不应在 BRAID_HTTP_ENDPOINTS
  里登记数据库**。off 模式 = 进程内直连。
- 结果集上限(固定,无 env 旋钮):行 1000、序列化输出 8MiB,**超限报错
  不截断**(错误会建议加 LIMIT);整体耗时由 attempt deadline 约束。
  单行峰值另受界:driver 整行驻留后才进预算判定,上界 = 服务端
  `max_allowed_packet`(缺省 64MiB)——建议把该账号/库的
  max_allowed_packet 压到接近 8MiB(分析查询不需要更大包),峰值内存
  即钉在 ~max(8MiB, 单行)。
- 只读词表:语句头 SELECT|WITH + 顶层 DML/管理动词黑名单(含 WITH 前缀
  CTE-DML;INSERT/REPLACE 的字符串函数形态 `NAME(` 放行);层 2 =
  会话 `transaction_read_only=1`、层 3 = GRANT SELECT-only——纵深三层,
  任一层的绕过由下一层兜底。
- 工具包签名态(T10.3):清单可声明 `{"tools":{"db_query":{}}}`
  (presence-only,不得携带 endpoints/dirs);DSN 仍经 env 注入——签名
  工件零凭证。

## 11. 多副本部署注记(T15.6;D-t151 决策 7 R10)

四组件(api/worker/relay/reaper)均可水平扩展(雪花租约 T15.2 / 沙箱
owner 化 T15.3 / egress 一 worker 一网 T15.4 已解除三挡)。多副本清单:

- **雪花节点号**:多副本一律 `*_SNOWFLAKE_NODE=auto`(compose 单副本可显式;
  k8s RollingUpdate 下 pinned-env 永久僵局,决策 2)。
- **worker 身份(owner)**:并发活体间唯一且跨重启稳定——compose 逐副本
  显式 `hostname` + `BRAID_WORKER_ID`;k8s 用 pod 名(hostname 缺省天然
  唯一)。`stop_grace_period ≥ 35s`(compose)/ `terminationGracePeriodSeconds
  ≥ 35`(k8s),排水中途被 SIGKILL 会在飞沙箱容器变孤儿。
- **连接容量**:见 §1 多副本算式(`max_connections ≥ 100×N+30`)。
- **TrustedProxies(R10,必配)**:`BRAID_API_TRUSTED_PROXIES` 在多 api
  副本 + 反代形态下**必须显式配置**——api 是 stateless 可水平扩展,但限流
  键依赖客户端真实 IP;反代未登记进信任列表会折叠全部流量到单一限流桶
  (共享 DoS 面:一个桶打满全体 403/429)。单值配置经 LB 对全部副本生效
  (配置同源,非 per-replica)。
- **观测聚合**:reaper SQL 快照 / worker-relay 的 Redis 探测类 gauge 多副本
  各曝一份相同值,告警查询一律 `max`(T15.6 R9 重标定,
  `deploy/prometheus/alerts.yml` 文件头口径)。
- **滚动升级**:升级序见 UPGRADE.md §2;mode=all 会签工作流的发布/触发
  另受启动期能力硬闸保护(UPGRADE.md §mode=all,T15.6)。
- **演练**:`BRAID_TEST_REQUIRE_DRILL=1 go test ./internal/drill/`(五臂
  故障注入,独立 compose 栈,docker 在场即真跑)。

## 12. 镜像构建与发布(T16.5;D-2026-10-03-t165-image-delivery)

发布通道裁定 = **本地导出**(零 Secrets 姿态维持;GHCR 评估与重访触发器见
D- 条目)。`deploy/release.sh` 四臂:

| 命令 | 作用 |
|------|------|
| `deploy/release.sh build [--allow-dirty]` | 构建双 tag:`braid:<short-sha>` 钉版 + `braid:prod` 浮动(compose 契约名) |
| `deploy/release.sh save [tag] [dir]` | 导出 `dist/braid-<tag>.tar` + `.sha256`(默认 tag = 当前 HEAD) |
| `deploy/release.sh load <tar>` | 验和后 `docker load`(缺 `.sha256` 或哈希不匹配即拒) |
| `deploy/release.sh k3s-import <tar> [容器名]` | 验和后导入 k3s containerd(k3s 面部署形态) |

- **钉版部署**:Helm 侧 `--set image.tag=<short-sha>`;compose 侧 prod 栈
  名即 `braid:prod`(build 段缺席时自动触发同源构建)。tar 内双 RepoTags
  同随镜像走,目标机 load / k3s-import 后双名齐落。
- **完整性校验(防误传/损坏,非防篡改)**:sha256 由 save 单点生成;
  load / k3s-import 一律先 `sha256sum -c`,缺失或不匹配即拒(RC=1)。
  tar 与 .sha256 同通道搬运,防的是离线搬运中的误传与损坏;防篡改须
  带外通道比对哈希(镜像签名属 GHCR 重访触发器域)。
- **`--provenance=false` 钉死**(build 臂内置,非可选):BuildKit attestation
  会把镜像做成 OCI index,`docker save` / `ctr import` 双双
  `content digest not found`(本机实证)。
- **GOPROXY 解析链**:shell env 优先,缺省回落 `go env GOPROXY`(受限网络
  出口常经 `go env -w` 配置,不落 shell env;不透传则容器内打
  proxy.golang.org,本机 EOF 不可达)。
- **脏树守卫**:build 默认拒脏树,判脏口径 = `git status --porcelain`
  (staged/unstaged/**untracked 三态**——未跟踪文件经 `COPY . .` 烘进
  镜像,游离迁移文件会经 migrate Job 真执行;gitignored 面不误报);
  `--allow-dirty` 豁免时 tag 追加 `-dirty` 后缀保真溯源。
- **k3s 名归一**:K8s/CRI 把 `braid:<tag>` 归一成 `docker.io/library/braid:<tag>`
  再查本地;实证(k3s v1.31.4)单段名 `ctr import` 自动归一,脚本保留
  重标名防御分支(不归一形态兜底,幂等)。
- **构建上下文**:`.dockerignore` 随本任务增补 `dist`/`log`/
  `web/canvas/node_modules`(T7.4 凭证面隔离口径之上,导出面与依赖面
  不进 context)。
