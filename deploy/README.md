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
| `BRAID_DB_MAX_OPEN` | 25 | 单进程最大打开连接数 |
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

## 5. 账号授权矩阵(生产账号分离)

审计/账号域表的表级最小授权(T7.4 交付完整方案,本表先声明契约;与
0001/0004/0005 迁移头注释同源):

| 账号 | audit_log | audit_pending | login_audit | outbox |
|------|-----------|---------------|-------------|--------|
| api | INSERT / SELECT | INSERT | INSERT / SELECT | INSERT |
| worker | INSERT / SELECT | INSERT | — | INSERT |
| relay | — | — | — | SELECT / UPDATE |
| reaper | INSERT / SELECT / ALTER | SELECT / DELETE | **DELETE**(T7.1 新写面) | INSERT / SELECT / DELETE |

- reaper 对 audit_log 的 ALTER 是分区水位作业专属(ADD/DROP PARTITION);
  其 INSERT/SELECT 来自 T6.1 搬运(暂存行批量 INSERT 进 audit_log + COUNT
  对账回读,internal/audit/flush.go)。业务账号(api/worker)对 audit_log
  仅 INSERT/SELECT 是 append-only 铁律,T7.3 preflight 启动断言(schema 级
  DELETE 会被 CheckAuditAppendOnly 拒启)。
- login_audit 的 reaper DELETE 授权是本配额表新增契约(0005 迁移注释
  登记);漏授权的症状 = janitor 每轮报错重试(不 brick,错误隔离)。
- 业务对账面(workflow_execution / node_* 等)的 reaper 授权以
  internal/reaper 语句集为准,随 T7.4 定稿。

## 6. 指标 exposition 端口

api 的 `/metrics` 恒挂 `BRAID_API_ADDR`(与 /healthz 同回环绑定);worker/
relay/reaper 经 `BRAID_METRICS_ADDR`(空 = 不启)。无认证 exposition,
只绑回环或留内网(反代只转发 /api/v1 与 /ui/)。
