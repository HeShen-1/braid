# T15.1 HA 目标形态:红线底稿与单点裁定论证

> 状态:**定稿**(用户确认 2026-10-01;D-2026-10-01-t151-ha-target 的论证全文)。
> 范围:只裁 ARCHITECTURE 18 项锁定决策之后的**多副本新面**;Q3(MySQL 真相源)/Q14(交付形态)/Q16(自研引擎)不在重开之列。
> 方法:全仓「副本>1 即触发」红线 survey(2026-10-01,证据锚逐条核到 file:line)→ 逐单点裁定 → DECISIONS.md D- 条目;rev2 = 11-agent 对抗审计(4 镜头 → 去重 → 逐条反驳验证:5 CONFIRMED + 1 REFUTED + 15 条配额外的自查补验)后修订。
> 下游:T15.2(雪花)/T15.3(沙箱)/T15.4(egress)/T15.6(混跑闸+演练)以本文件为前置闸;与 T15.5(压测)并行,§5 留数据接口。

## 1. 红线清单(副本>1 即触发,证据锚已核)

### R1 雪花节点号撞号 → 业务表 PK 1062(挡 api/worker/reaper 全部)

- 现状(两条撞号路径,交付面主因是前者):
  - **交付面常量节点号跨副本共享**:prod compose 显式钉 `BRAID_API_SNOWFLAKE_NODE=1` / `WORKER=2` / `REAPER=3`(deploy/prod.docker-compose.yml:124,147,199),chart 三模板同钉(worker.yaml:50、api.yaml:42、reaper.yaml:37)——Deployment 各副本继承同一常量,**副本>1 即同号**;
  - **缺省派生路径**:api/worker 无 env 时内建默认 1/2(internal/api/config.go:31);reaper 无 env → `os.Getpid() & 1023`(internal/reaper/config.go:206)——容器 PID 恒 1 ⇒ 未显式配置的自管部署 reaper 恒 1,与 api 默认撞号。
- 故障机制:same (ms, node, seq) → 相同 snowflake id → 全部雪花 PK(workflow_execution / node_execution / node_attempt / audit_log / outbox / approval_decision)1062 写入中止(snowflake.go 注释自认「跨进程重号的唯一症状」);碰撞是概率性事件,负载下必然出现。
- 结构性缺口:Deployment 无稳定 ordinal;compose 无 ordinal 概念;全平台无任何分配/仲裁机制;chart「副本>1 需外部雪花节点号分配」只是注释登记(values.yaml:42)。
- 影响:这是**唯一挡住全部三类组件水平扩展**的红线,T15.2 承载。

### R2 沙箱清扫互杀(挡 worker,沙箱启用时)

- 现状:worker 启动 `SandboxSweep` 按 `label=braid.sandbox=1` **全局**清扫(internal/tools/sandbox_carrier.go:424-440);t101 基线 #11(docs/designs/t101-sandbox-baseline.md:89)明文「多副本/k8s 滚动部署沙箱前必须先解」。
- 故障模式:同 daemon 第二个 worker 启动(或滚动重启的新实例)把共存 worker 的在飞沙箱容器 `rm -f` → attempt 死在半途 → reaper 判死重派。正确性由 reaper 兜底,但故障是**自造的**;更糟的是 kill -9 后死实例的容器身份只能靠全局 label 追溯(误伤面与回收面纠缠)。
- 影响:T15.3 承载;k8s 滚动部署(节点级 host docker.sock 挂载形态)同样触发。

### R3 egress 代理撞端口拒启(挡 worker,egress 启用时)

- 现状:worker 在 egress 网关 IP 上 `net.Listen(gwIP, 9210)`(internal/tools/egress.go:688,`sandboxEgressPortDefault`);网名缺省单一 `braid-sbx-egress`,幂等建网 = 第二个 worker **复用同一网络**(D-t102 #1)→ 同网关 IP 同端口 → EADDRINUSE → 拒启(D-t102 登记④,chart README:98 沿用)。
- 故障模式:响亮拒启(非静默),第二副本永远起不来。
- 影响:T15.4 承载;与 R2 同根(沙箱拓扑按「单 worker」假设),一并裁定(§4.4)。

### R4 mode=all N-1 混跑窗(挡全组件滚动升级)

- 现状:D-t142①已登记——旧 api 对 all 首票定局(**否决权静默剥夺,不可逆**)、旧 worker 无定局复核、旧 reaper 空转重投;发布序纪律(全量升级后才发布/触发 mode=all)与回滚前置 SELECT **全靠 UPGRADE.md 人工纪律,零机制闸**。
- 故障模式:多副本滚动升级窗内发布/触发 mode=all 工作流 → 数据级不可逆破坏。单副本停机升级无此窗(Q14);**多副本常态化滚动 = 此窗常态化**。
- 影响:T15.6 承载;硬闸的能力边界(首个落地窗无保护)见 §4.5 如实登记。

### R5 队列 Redis 单点(全栈 SLO 上限)

- 现状:asynq 队列(pending/active/retry/scheduled/archived 全在 Redis,无外部持久化)+ reaper 选主锁(fail-open,正确性零依赖);api 零 Redis;热缓存实例已物理分离(ARCHITECTURE §2)。
- 已有的结构韧性(这是裁定的地基):
  - **正确性结构性不依赖 Redis**:outbox 意图先落 MySQL(ARCHITECTURE §3.1「outbox + relay + reconciler(P0 硬性验收)」锁定设计;internal/relay/run.go:11-23 注释),asynq 只是投递通道;reaper 是「MySQL 真相源上的纯周期对账器」(D-t25 #1),任务丢失 → 心跳判死/无活 attempt 分支/PENDING_TTL 三路兜底重派。
  - **断 Redis 30s 停机窗是 P0 已验收面**(D-t25 #2 验收相位)。
  - 丢已确认写入的恢复路径:outbox 行 DISPATCHED 但 asynq 任务消失 → 节点滞留 → reaper 扫描收敛;attempt 幂等键(att: TaskID)吸收重复执行。
- 故障模式:Redis 宕 → 新事件滞留 outbox(relay 退避重试)、判死窗推后——可用性受损,正确性保持。
- 影响:本文件 §4.2 裁定。

### R6 MySQL 单点(全栈 SLO 上限)

- 现状:Q3 真相源;全部 CAS/outbox/审计哈希链/雪花租约(§4.1 落地后)在其上;交付面 = 单实例手写模板(D-t144 #4 否决 Bitnami 子 chart 的理由 = 契约保真);braid 代码对 MySQL 拓扑**零感知**(单写入口,无读写分离,连接池默认 25/进程)。
- 故障模式:MySQL 宕 → 全组件 fail-closed 停摆(设计内);failover 丢已提交事务的影响面见 §4.3 论证。
- 影响:本文件 §4.3 裁定。

### R7 DB 连接容量公式(多副本部署 checklist 项,登记不修)

- 现状:D-t122 #2 软上限 100/进程锚 README §1 公式「Σ 常驻四入口默认 = 100 ≤ max_connections − 30」。
- 故障模式:副本数 N → fleet Σ = 25×4×N;MySQL 默认 max_connections=151 下 **N=2 即超** → 连接耗尽随机拒连。
- 处置:公式按 Σ 表述已天然含副本维度,补多副本注记(副本 N → max_connections ≥ 100N+30)随 T15.6 演练栈实测对拍;不改实现。

### R8 审计哈希链混合版本窗变长(已有设计覆盖,随演练复核)

- 现状:D-t93③——迁移后旧代码短窗写未启链行(NULL row_hash)按跳过处理;原按单实例(Q14)估窗分钟级。
- 多副本影响:滚动升级窗 = 逐副本换二进制时长,窗内 NULL 行数量变多,**跳过语义不变**(逐行判定,无跨行假设)。
- 处置:零改动;T15.6 演练含滚动窗哈希链校验用例,复核跳过口径在长窗下仍然成立(ROADMAP T15.6 验收已点名)。

### R9 多副本观测聚合语义(对抗审计新增,登记)

- 三类共享态探测按「每副本各自探测」:worker 副本各自 `braid_asynq_archived_tasks` 探测同一共享队列 → Prometheus `sum()` = N×实值(阈值 >0 语义保真,告警文案 `{{ $value }}` 虚胖 N 倍);QueueRedisMemoryWatermark 的 sum/sum 比值恰好抵消 N(巧合非设计);reaper backlog/age gauges 多副本各报一份,聚合语义未标定。
- 处置:告警口径随 T15.6 演练栈按 max/去重语义重标定(或 T17.4 告警补面);不改架构。

### R10 TrustedProxies 多副本必配(部署 checklist 项,登记)

- api ×N 实际负载均衡 = 反代/LB 在场;登录防御 IP 窗、ua_ip24 token 绑定、login_audit 归因全吃 ClientIP。TrustedProxies 空 = fail-closed(所有请求共享代理 IP)→ 单 IP 桶共享 → 阈值内次坏尝试即锁死全平台登录(共享桶 DoS);配宽 = XFF 伪造绕过 IP 窗(username 锁仍在)。交付面注释已有(D-t64),多副本部署 checklist 显式登记。

## 2. 正面清单(已 multi-replica-safe 的面,论证与叙事用)

| 面 | 机制 | 证据锚 |
|---|------|--------|
| relay 投递 | `ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`,选择→标记同事务 | internal/relay/run.go:9-16「多 relay 天然分片,稳态零重复」 |
| relay 重投 | ErrTaskIDConflict 幂等收敛(D-t42 #1 四段式 TaskID) | ARCHITECTURE §3.1 outbox 范式;internal/outbox |
| reaper 扫描作业 | leaderless fail-open;处置事务「锁后复核 + CAS」 | D-t25 #1 |
| reaper flush/janitor 收敛性 | 预过滤 + 裸 INSERT 使 leaderless 重叠搬运收敛(1062 回滚 + 下轮核销);janitor ADD 幂等(1517/1493 容忍) | D-t61 #3、D-t25 #10(注:flush/janitor 在 leader 闸外,N×空转面见 §4.5 登记项) |
| login_throttle | DB 锚定行锁单事务,跨副本阈值精确 | 0010 迁移、D-t113 #2,TestLoginThrottleCrossReplicaExact |
| api/MCP stateless | 零 session、零粘性路由,api 零 Redis | D-t131 #2(SDK Stateless)+ 理由段 |
| api 触发幂等 | uk_exec_idem 1062 + 读回重放,跨副本精确收敛 | D-t55 #5、D-t42 #6 |
| api 审批决定事务 | 锁 node 行 + CountRoundDecisions + uk(node_execution_id, round, decided_by) 三层 DB 收敛 | D-t42 #2、D-t142 #1/#3 |
| OIDC state / token 绑定 | 签名短时 cookie(HKDF 派生密钥)+ bnd claim 校验,无服务端会话态 | D-t111 #5、D-t113 #1 |
| worker 节点执行 | claim CAS + worker_id 接管复核(「接管与判死同拍」重叠执行已消灭);attempt 幂等键防双烧 | D-t25 #8 |
| 审计哈希链 | chain_heads per-execution 行锁,多写者串行安全 | D-t93 #2 |

结论:**四组件(api/worker/relay/reaper)在 R1 解除后即全部可水平扩展**;R2/R3 是 worker 在沙箱启用时的附加条件;R4 是升级过程属性;R5/R6 是基础设施层性状(由架构性韧性承载,见 §4.2/§4.3);R9/R10 是观测与部署 checklist 面。

## 3. 目标拓扑

```
            ┌─ api ×N ────────── stateless,DB 锚定登录防御,MCP stateless
            ├─ worker ×N ─────── claim CAS 分片;per-worker 沙箱网络域(§4.4)
MySQL 单写 ─┤  真相源:CAS/outbox/哈希链/雪花租约+能力宣告(§4.1)
            ├─ relay ×N ──────── SKIP LOCKED 天然分片
            └─ reaper ×N ─────── leaderless fail-open 全幂等
Redis 单实例(队列+选主)── 可丢失传输层:outbox+对账承载正确性,AOF+容器自愈承载可用性
```

HA 达成方式 = **每单点都有不丢正确性的恢复路径 + 明确 RTO/RPO 口径**,而非「每个组件都有热备」。

## 4. 单点裁定

### 4.1 R1 雪花节点号 → MySQL 租约表(T15.2 承载)

选项对比:

| 方案 | 判据 | 结论 |
|---|---|---|
| (a) MySQL 租约表(心跳 TTL) | compose/k8s/裸机同构;真相源 = MySQL(哲学一致);故障域单一;TTL 自然回收死副本 | **采纳** |
| (b) StatefulSet ordinal | k8s-only,compose 无 ordinal;跨平台两套机制 | 否决 |
| (c) 名字哈希 + 冲突检测 | 冲突检测本身需要共享仲裁点;若落 MySQL 已是 (a) 的退化版(无租约,副本死亡后 ID 悬死) | 否决(可作 (a) 的选位策略) |
| (d) 人工 env 区段 | 即现状(prod compose/chart 钉常量 1/2/3 = 隐式手工区段,R1 主因);运维负担 + 副本>1 即撞 | 保留为单副本 override 通道(见下) |

裁定细节(D- 钉死,T15.2 细化实现):

1. **新表**(迁移 0012 起):`node_id SMALLINT UNSIGNED PK`(0..1023 全局单池,三类组件共用不分池)、`holder_kind ENUM('api','worker','reaper')`、`holder_id VARCHAR(64)`(实例身份)、`expires_at DATETIME(3)`、`capabilities JSON NULL`(NULL = 未宣告,fail-closed;**建表即含,一次建齐**——T15.2 建表、T15.6 消费,省一次 ALTER 与一个迁移序号);relay 不参与(无生成器)。
2. **节点号解析三层优先级**:`显式 env 数字 > 内建默认(1/2,视同显式 claim,「单副本现状零破坏」在此语义下成立)> 字面 "auto"(哨兵值,池分配)`;缺省 = 内建默认(现状逐字节不变)。多副本部署 = 各副本写 auto 或分配不同号。
3. **显式 claim 冲突语义**:同 `holder_id`(实例身份)复占自己的租约 = 即时成功(正常重启复航);被他持占 = 拒启(**kill -9/OOM 残窗 ≤ TTL,有界**,runbook 应急 = 清租约行);**优雅停机(SIGTERM)必须 DELETE 释放租约**——覆盖正常重启路径,把拒启窗压到非优雅路径。**k8s 限制登记**:Deployment 默认 RollingUpdate(maxSurge=1/maxUnavailable=0)下新副本先起、旧副本持号续期 → pinned-env 永久僵局,**显式 env 通道限单副本/compose 使用,k8s 多副本一律 auto 池**;chart 静态钉 1/2/3 随 T15.2 摘钉转 auto(验收「副本>1 可起」的前提)。
4. **auto 池**:线性取首个空闲号(过期判定见下);全满(1024 活实例)→ 拒启。**acquire 原子性与竞态**:INSERT 抢注,PK(node_id) 使双副本同取一号时败者 1062——败者**有限次重扫**(退避,≤3 次)后拒启,不做拒启风暴。
5. **过期判定全 DB 端 NOW(3)**:结构性排除的是**时钟漂移**路径(不信任进程时钟);不声称结构性排除一切双持——**已知边界登记**:MySQL 异步 failover 尾部回退可吞掉已提交的租约授予/续期(§4.3 自己钦定的 RPO 无上界场景),holder 自以为续期而行内已过期,他副本懒回收同号 → 双签发窗,症状 = 1062 响亮写入中止(不损已提交数据);心跳 goroutine 停顿(进程冻结/长 GC)同理留 zombie 窗。收紧面 = 半同步复制(§4.3);彻底解 = 租约 fencing token(签发单调递增 epoch,消费端拒旧 token),复杂度不对等,**登记后续不随 T15.2 实施**。
6. **续期失败 = 停止签发 + 停止消费**:续期失败 → 停止签发新 id **并优雅下线 asynq 消费**(停止接新任务;只停签发会把该副本变成烧 MaxRetry=1 预算的任务黑洞——每个投递到的任务确定性失败进 archived 再由 reaper 重派)+ 响亮;复航 = 重新 acquire 成功后重启消费。
7. **回收 = 懒回收**(acquire 覆盖过期行即回收);显式 janitor DELETE 可选(镜像 login_throttle 形态,T15.2 定)。
8. **reaper PID 派生退役**:未显式配置时容器 PID 恒 1 = 必撞 api 默认 1;reaper 缺省改走 auto 池。
9. **授权面影响**(铁律面):新表须为 api/worker/reaper 三账号补 INSERT/UPDATE/SELECT(采纳优雅释放再加 DELETE);配套 deploy/grants.sh、UPGRADE.md §3 存量数据卷手工授权路径、README §5 授权矩阵、T17.3 组件级授权断言台账。

### 4.2 R5 队列 Redis → 单实例 + 结构韧性(Sentinel 负样本否决)

**Sentinel 丢写负样本论证**(issue 钦定必含):

- Redis 复制是异步的;Sentinel failover 提升滞后副本,**已确认写入丢失窗口无上界**(官方文档自认 "there is always a window for losing acknowledged writes";Jepsen 实测单次 partition 丢 1126 条已确认写入——ARCHITECTURE 审计事实链已录)。
- `WAIT` 不构成 CP:它只确认「N 个副本已收到」,不是共识提交——极端分区下 failover 仍可丢(WAIT 与 Sentinel 组合的窗口收缩也不改变性质)。
- 对 braid 的具体伤害:丢的不是正确性(outbox+reaper 对账兜底)而是 **DISPATCHED-but-丢失的恢复延迟**(等 reaper 判死窗,分钟级)+ 全量在飞任务重派一轮(幂等键吸收重复副作用,但 LLM 计费类外部调用重复执行面扩大)。
- 代价面:3×Sentinel + quorum 配置 + client 重连语义适配 + preflight 契约(SENTINEL 拓扑下的 CONFIG GET 探测路径)全部新增,换来的只有 **RTO 自动化**(10-30s),而 RPO 反而劣于单实例 AOF(有界)。

**替代选型全谱**(逐路否决理由):

| 候选 | 结论 | 理由 |
|---|---|---|
| 单实例 + AOF everysec + 容器自愈 | **采纳(目标态)** | RPO 有界(见下表);prod compose 已带 healthcheck + `restart: unless-stopped`(prod.docker-compose.yml:41,71)→ 宿主内故障 RTO 秒级自动;正确性零依赖(§2 正面清单);零新增运维面 |
| Sentinel | 否决,登记未来 D- 方向 | 丢写负样本如上;asynq 侧支持面属实(UniversalClient + MasterName),**但 braid 全链硬编码具体类型 `asynq.RedisClientOpt`**(cmd/relay/main.go:83,105、cmd/worker/main.go:151,308、worker.Run 签名 worker.go:155)——自管接入 Sentinel 需改 Go 装配面与签名 + 新 env 键,**不是纯运维动作**;当前不提供该形态,诉求成立时按重开条款走独立 D-(装配面改动) |
| Redis Cluster | 否决 | 丢写语义同 Sentinel(异步分片复制);分片解决写吞吐——braid 不需要(P2 规模化靠 worker 分片,队列单键空间吞吐富余);asynq 有 cluster 测试面(inspector ClusterKeySlot)但多键 Lua/事务跨槽语义需逐个验证;运维复杂度最高 |
| WAIT 增强(投递确认绑副本落盘) | 否决 | WAIT 须插进 asynq 内部 MULTI/Lua 才有意义,relay 层包不住(fork asynq 或侵入其内部 = 不可实施面);且不改变非共识本质 |
| 热备(副本 + 手动 failover) | 否决为默认形态,登记运营扩展位 | RPO = 复制滞后(仅人工决定 failover 时丢失);compose 单机形态下两 Redis 同故障域,收益 ≈ 0;跨机热备 = 已超出 compose 交付面,归运营域与 Sentinel 同等登记 |
| 换队列技术(Kafka/RocketMQ) | 否决 | 重开 Q3/技术栈铁律面;outbox+对账架构下队列持久性要求已被架构性降低——这正是自研引擎的设计红利,反向追随强持久队列是需求倒置 |

**RTO/RPO 口径表**(写入 D- 与 README;口径修正:进程崩溃与宿主灭失分立):

| 场景 | RPO(数据) | RTO(可用) | 恢复机制 |
|---|---|---|---|
| 进程崩溃/容器重建(数据卷存续) | ≈ 0(AOF 已 fsync 部分完整回放) | 秒级(自动) | restart 策略 + AOF 回放 |
| 宿主重启(卷存续) | ≈ 0 | 分钟级(自动,栈随宿主起) | restart 策略 |
| 宿主灭失/数据卷损坏(appendfsync=everysec) | ≤1s(fsync 间隔;fsync 拖延 >2s 时 Redis 阻塞写入,最坏 ~2s) | 运维响应 | reaper 对账全量重派(正确性零损,在飞任务重执行一轮) |
| Sentinel failover(若未来采纳) | **无上界**(复制滞后) | 10-30s(自动) | 同上,重复执行面扩大 |

**重开条款**(与 T15.5 的数据接口):压测定容量模型时给出队列吞吐阈值;若实测证明单实例 Redis 成为吞吐瓶颈(而非网络/CPU),Redis 拓扑重议——届时 Sentinel/Cluster 与丢写语义一并上桌,以压测数据为据。**当前无证据不预支。**

### 4.3 R6 MySQL → 运营域 + 文档化(braid 零代码零迁移)

**分层判据**:雪花租约是 braid 数据模型的一部分(节点身份是引擎概念,进产品域);MySQL 拓扑是基础设施配置(半同步/组复制/orchestrator/RDS 是部署架构师的选择,不进产品域)。产品承诺 = 「单 MySQL 真相源上的正确性」;交付面维持单实例模板(契约保真,D-t144 #4 先例)。

**failover 语义论证**(文档化义务的核心,也是「不破哈希链」的论证;前提限定 rev2):

- 复制流前缀性质依赖三个运维前提,**须随文档一并声明**:①failover 提升**最先进的副本**(提升落后副本 = 其余存活副本相对新主出现洞;orchestrator/管理面纪律);②无复制过滤器、无人工 skip 事务(两者在副本侧制造永久洞);③MTS 并行回放期 executed GTID 集存在瞬时 gap,追平后收敛为前缀( steady 态性质)。前提满足时:**failover 丢失 = 尾部前缀回退,不产生中间洞**。
- braid 每个状态迁移事务只触碰**单个执行**的链(node 行 + audit 行 + chain_heads 推进同事务原子,D-t93 #5 写入即算;链推进与暂存 INSERT 同事务,flush 纯搬运)→ 尾部丢失 = 整事务丢失 → 每条 execution 的哈希链要么停在丢失前、要么整体缺失,**中间破洞结构性不可达**;chain_heads 与链尾一致回退。
- 雪花 id 由时间+node 派生:failover 后时钟回拨场景已由 D-2026-09-21 bounded-wait 决策覆盖(≤2s 阻塞,>2s 响亮)。**租约行的跨行一致性不在该论证覆盖内**(租约授予与业务 id 签发是不同事务)——双持已知边界见 §4.1-5。
- 半同步(after_sync)推荐为运营方收紧选项:尾部窗收窄到「主库与至少一个半同步副本同时灭失」;**降级语义如实登记**:`rpl_semi_sync_master_timeout` 超时后降级异步(RPO 回到无上界),部署注记须钉该参数语义。

**文档义务清单**(随本 D- 落 README/UPGRADE,零代码):①HA MySQL 部署注记(半同步推荐 + GTID + 上述三前提);②failover 后 preflight 通过性说明(组件重连池自愈,`ConnMaxLifetime` 已有);③RPO 口径(异步 = 无上界 / 半同步 = 有界,降级语义)按 §4.2 表格同形登记。

### 4.4 R2/R3 沙箱与 egress → owner 化 + 一 worker 一网(T15.3/T15.4 承载)

**前提修正(rev2,对抗审计 HIGH)**:WorkerID = `os.Hostname()`(cmd/worker/main.go:109-115,BRAID_WORKER_ID 可覆写),而 prod/dev compose **均未设 hostname 键**——docker 缺省 hostname = 容器短 ID,`docker compose restart` 身份稳定,但 `up -d` 换镜像 **recreate(即 compose 面的升级路径)必换身份**。原稿「compose 同 hostname 重启」两句论证仅在 restart 场景成立,据此修正为:

1. **owner = 既有 WorkerID(hostname 级,internal/worker/config.go:24,已落 node_attempt.worker_id 有对账面)**,不新造实例 id 概念;沙箱容器 label 扩维度:`braid.sandbox=1`(注册面保留,孤儿可发现)+ `braid.sandbox.owner=<worker_id>`,`SandboxSweep` 只清 owner 匹配的自家容器。
2. **restart 与 recreate 分立语义**(如实登记):
   - `docker compose restart`(同容器,身份稳定):owner 同名 → 清扫旧容器 = 原自机清扫语义,成立;
   - `docker compose up -d` recreate(升级路径,身份必换):新 owner 清不到前身容器 → 前身靠**优雅停机自清**(SIGTERM cancel 窗;注意:compose 缺省 stop_grace_period=10s < worker SHUTDOWN_TIMEOUT=30s,超时 SIGKILL 落孤儿桶——**compose 钉 stop_grace_period ≥ 35s 列为部署契约项**);kill -9/超时孤儿 = **全局清扫降级为 runbook 显式原语**(UPGRADE.md;按 `braid.sandbox=1` 全清,只给运维不给自动路径)。基线 #11「身份可知 + 回收不依赖全局 label 误伤」由此满足。
   - **k8s 滚动(pod 名唯一)**:新实例清不到旧实例容器 → 互杀消灭;旧实例容器靠其优雅停机自清,同上归孤儿桶/runbook。
3. **「owner 在并发活体间唯一且跨重启稳定」升格为部署契约**(对抗审计 HIGH 修):多副本 compose 模板逐副本显式 `hostname:`(或 `BRAID_WORKER_ID`),chart 用 pod 名天然满足;**活体同 owner 冲突边界登记**(手工 `docker run --hostname` ×2、StatefulSet 同名快速重建落同节点):第二活体清扫杀第一活体在飞容器,R2 原样复活——缓解选项(T15.3 选型):sweep 前借租约表 holder 活性复核(有活租约的同 owner = 拒启自检),本 D- 不锁死实现。
4. **egress 一 worker 一网**:网名带实例段 `<braid-sbx-egress>-<worker_id 短哈希>` → 每 worker 独立 bridge 子网 → **独立网关 IP,同端口 9210 不撞**;顺带把不同 worker 的沙箱容器**物理网络隔离**(R2 误伤面再收窄一层)。
5. **地址池上界与网泄漏(rev2 登记)**:docker daemon 缺省地址池(172.17.0.0/12 按 /16 切 + 192.168.0.0/16 按 /20 切)≈ **31 个用户 bridge 网/daemon**——「一 worker 一网」= 同 daemon worker 副本硬上界 ~31(扣既有栈);k8s host docker.sock 形态 = **节点级 daemon 共享**(修正原稿「每 pod 独立 daemon」措辞),上界按节点计;扩容 = daemon.json `default-address-pools`。docker 网络是持久对象不自回收 → **每次 recreate 泄漏一网**(各占一子网,耗尽后 egress 模式建网失败 = 响亮拒启)——网回收并入 runbook 孤儿清扫同节(`docker network rm`)。
6. **rootless/受限 daemon 适用域**(D-t102 followup ⑦:rootless = bind 网关 EADDRNOTAVAIL 拒启)不受影响——一 worker 一网不改变该边界。
7. **并发信号量语义重述**:`BRAID_SANDBOX_MAX_CONCURRENCY` 是进程级;多副本同 daemon 下 daemon 负载 = Σ 各副本。README 注记按 daemon 维度重述,不改实现(调度压力天然被 fan-out 上界与 attempt deadline 钳制)。

### 4.5 R4/R7/R8 演练口径 → compose 多副本注入集 + DB 能力注册表硬闸(T15.6 承载)

**演练形态**:独立 multi-replica compose(prod compose 四服务钉 container_name,compose 对 container_name 服务拒绝 `--scale`,参数化不可行 → 新建独立文件或模板化重构)+ t8.1 故障注入套件形态(BRAID_TEST_REQUIRE_STACK 门控先例)。**实施前置登记**:演练栈的测试面若新增 Redis 消费包,必须先扩 Redis DB 槽位分配表(0..15 已满,AGENTS 铁律)或复租既有零键空间基号——T15.6 开工前裁定。

**注入集(验收面)**:

1. 双 worker 抢 claim → CAS 分片收敛、零 1062(雪花租约断言:双 worker 租约号互异);
2. kill -9 单 worker(持租约)→ 优雅路径外的租约残窗按 §4.1-3 语义收敛(TTL 过期回收 / 复占)+ reaper 判死接管在飞 attempt;
3. worker 滚动重启 + 沙箱在飞 → owner-scoped 清扫不互杀断言(旧实例容器存活至自然收口;recreate 路径另验孤儿/runbook 面);
4. Redis 重启(AOF)→ outbox 恢复投递 + 判死重派收敛(正确性断言复用 t8.1 断 Redis 面);
5. mode=all 混跑窗硬闸:**断言钉在「新 api 收到的请求」**(演练栈 api 路由钉死或单 api 副本——多 api 副本下请求落旧 api = 零闸放行,按现文会偶发假绿/假红);全量升级后放行;哈希链校验在滚动窗后全链绿(R8 复核)。

**mode=all 硬闸机制 = 能力宣告挂实例注册面**(§4.1 租约表 capabilities 列,一次建表):

- **宣告时点 = acquire(启动期)**,与 T15.6 验收措辞「启动期能力探测」对齐;api 在**发布/触发 mode=all 工作流时**查「在场实例(holder 未过期)全部宣告 mode=all」,在场未宣告即拒(fail-closed)。
- **闸扫描 kind 集合 = api/worker/reaper 三类全扫**(mode=all 语义由三方共同消费,D-t142 决策 1/3/6/7;漏扫任一 kind = 对应腿裸奔)。
- **误放 < 误拒**取舍:死实例租约未过期(<TTL)→ 它未宣告 → 拒(误拒方向,安全:升级中短暂误拒发布,优于静默破坏);过期后不再计入。
- **硬闸铺满窗如实登记(已知边界,rev2)**:硬闸自身只对「租约表全舰队上线后的**后续**升级」生效——①首个落地窗内,pre-T15.2 旧二进制从不写租约表,闸看到的「在场」= 仅新副本子集,旧 api 可继续放行;②混跑窗内多 api 副本经 LB 把请求路由到旧 api 时,**旧 api 进程内无闸代码,零闸放行**(正是 R4 要挡的破坏路径)。故首个落地窗与请求路由面仍由「升级完成前不发布/触发 mode=all」人工纪律承载(UPGRADE.md),**硬闸是后续升级的机制闸,不是首个落地窗的替身**——R4 闭合度按此如实陈述,不高估。
- 判定通过后、节点执行前旧副本新上线的残余窗(原登记项)同样保留;彻底闭合需要旧二进制能自检,而旧二进制对 all 是「认识但语义错」,无法自检——结构性极限,如实登记。
- 回滚前置 SELECT(D-t142②)保留,硬闸是前置防线不是替代。

**R7 处置**:README §1 补多副本注记(max_connections ≥ 100×N + 30),随演练栈实测连接数对拍;不改实现。**R9 处置**:告警口径(archived sum N×虚胖等)随演练栈按 max/去重语义重标定,归 T15.6(或 T17.4 告警补面)。**R10 处置**:TrustedProxies 配置进多副本部署 checklist(README §部署注记)。

**flush/janitor leader 闸缺口(rev2 登记,对抗审计 CONFIRMED)**:reaper 的 `janitorLoop`/`auditFlushLoop` 在 leaderGate 之外独立运行(reaper.go:76-81 vs :98)——N 副本 reaper 每秒全员从队头并发搬运同一批暂存行(读批无锁),每批稳定产生 N-1 次空转(读批 + 预过滤 + 被 PK 锁阻塞的裸 INSERT + 1062 整批回滚),空转率 (N-1)/N 随副本数与审计流量线性放大,且每碰撞轮 ERROR 日志 N×放大。**收敛性不受影响**(D-t61 设计内),是效率与日志噪声面。修复方向(纳入 leaderGate 或 leaderless 退避)归 T15.6 演练观察后定档,不随本 D- 实施。

## 5. 与 T15.5(压测)的接口

- **Redis 重开条款**(§4.2):压测容量模型给出队列吞吐阈值 → 单实例瓶颈实证时拓扑重议(届时 Sentinel/Cluster 与丢写语义、装配面改动一并上桌)。
- **锁窗与连接压力的副本维度**:MaxRollbackRows 实测定档与多 worker 并发下 DB 连接/锁竞争数据 → 容量模型按副本数参数化。
- 两 lane 互不阻塞:D- 定稿不依赖压测数据(当前裁定全部以既有架构性质为据);压测产出只触发 §4.2 重开条款。

## 6. 已知边界与登记(不随本任务实现)

1. Sentinel/热备自管接入 = 需改 Go 装配面(`asynq.RedisClientOpt` 钉进 worker.Run 签名与 cmd 装配点)的非纯运维动作,当前不提供;诉求成立按重开条款走独立 D-(§4.2);
2. MySQL HA 全部归运营域,braid 侧仅文档义务(§4.3);前缀论证依赖三运维前提(提升最先进副本/无过滤器/无人工 skip);
3. 租约双持窗:全 DB 端时钟只排除时钟漂移路径;MySQL failover 尾吞授予/续期 + 心跳停顿留 zombie 窗,症状 = 1062 响亮;半同步收紧,fencing token 彻底解登记后续(§4.1-5);
4. mode=all 硬闸的铺满窗(首个落地窗零执法 + LB 路由旧 api 零闸)与「判定后旧副本上线」残余窗均靠滚动纪律,硬闸只保后续升级(§4.5);
5. 孤儿沙箱容器/泄漏网络的回收是 runbook 显式原语,不进自动路径;recreate 每次泄漏一网,受 ~31 网/daemon 地址池上界约束(§4.4);
6. 活体同 owner 冲突(手工同 hostname ×2/StatefulSet 同名重建)R2 复活面,缓解 = 部署契约 + 可选租约活性复核(§4.4);
7. R7 连接容量 = 文档注记 + 演练对拍;R9 观测聚合 = 演练后重标定;R10 TrustedProxies = 部署 checklist(§4.5);
8. 队列 Redis 热备(副本 + 手动 failover)与 Sentinel 并列登记为运营扩展位,不进默认形态(§4.2 表格);
9. flush/janitor 无 leader 闸的 N×空转与日志放大,修复方向归 T15.6 定档(§4.5);
10. 多租户重契约族与 K8s 原生 egress 映射(NetworkPolicy + sidecar)维持 P1 停车场登记,不在本 D- 范围(survey parked_note 口径)。
