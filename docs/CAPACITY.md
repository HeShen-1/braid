# braid 容量模型与压测基线(T15.5)

> 真源:本文 = 引擎核心压测基线与容量模型(D-2026-10-01-t155-bench-baseline);
> benchmark 代码 = `internal/scheduler/bench_test.go`(核心吞吐 / 回滚锁窗)、
> `internal/worker/bench_e2e_test.go`(端到端吞吐 / 时延)、
> `internal/api/auditquery_bench_test.go`(审计查询延迟,D-t63#3 裁定输入)。
> 本页数字为 T16.4(README 性能章)的上游;重测按 §2 复跑口径。

## 1. 测量环境(如实入档)

- 机型:Intel i5-12400(12 逻辑核)/ 30GiB RAM(测量时可用 ≈7.8GiB)/ MySQL
  8.0.46 默认 innodb(**buffer_pool=128MB 默认**,大行集档位部分走 tmpfs 内存页)
- 栈:t13(`deploy/migrate-test.docker-compose.yml`)——MySQL 8.0(默认 innodb 配置,
  **datadir 在 tmpfs**:写路径无落盘成本,数字 = 存储乐观包络,生产盘速下写侧打折)、
  Redis 7.4(noeviction、无持久化,队列语义真源在 MySQL)。
- 系统负载:**测量窗 load average ≈ 52(1/5/15min 全程)**,运行间方差实测 ±2×
  (同形状 300 行档 fixture 1.78s vs 真路径 0.42s)——
  全部数字为**负载竞争下的保守下界**:独占窗口下 CPU 侧成本只会更好;
  DB 行锁/事务形状类指标(回滚锁窗的行数斜率)对 CPU 竞争相对不敏感,
  但绝对墙时仍偏保守。定档结论(§5/§6)均取保守侧。
- 基准负载:即回执行器(echo,零外部调用)× 线性 llm 链 / fan-out 网
  格——隔离 engine 与管道成本;真实 LLM/工具调用的执行器耗时按
  「外部调用窗内不占 engine 资源」外推(§7)。

## 2. 复跑口径

```bash
# 核心吞吐 + 回滚锁窗(scheduler 包零 Redis)
go test ./internal/scheduler/ -bench 'BenchmarkInstantiate|BenchmarkComplete|BenchmarkRollback' \
  -benchtime=5x -run '^$' -timeout 3h
# 端到端(需 Redis 槽位显式;worktree 各自查 .testenv / AGENTS.md 分配表)
BRAID_TEST_REDIS_DB=<slot> go test ./internal/worker/ -bench BenchmarkWorkerE2EChain \
  -benchtime=3x -run '^$' -timeout 2h
# 审计查询(api 包零 Redis)
go test ./internal/api/ -bench BenchmarkAuditQuery -benchtime=10x -run '^$' -timeout 3h
```

- 栈不可达自动 skip(CI `BRAID_TEST_REQUIRE_STACK=1` 下红);bench 仅显式
  `-bench` 触发,`go test ./...` 零成本。
- 回滚锁窗网格 = fixture 直插终态行(种子 O(1)),与真路径的等价性由
  `TestRollbackFixtureEquivalence`(~300 行档)钉死——该测试随 CI 常跑。

## 3. 核心吞吐(scheduler 面)

| 场景 | 档位 | 结果 |
|---|---|---|
| Instantiate 物化(触发事务) | 16 / 64 / 256 节点 | 8.7 / 51.6 / 218.7 ms/op(≈0.54→0.85 ms/行) |
| Complete 收口(生产 worker 收口事务:全行锁 + attempt 收口 + CAS + advance 派发 + touch) | 16 / 64 节点链(行集 18 / 66) | 20.3 / 16.8 nodes/s(≈49 / 60 ms/节点) |

读法:Complete 的全行锁(D-t24#6)使单节点收口成本随执行总行数(含
parallel 实例行)单调增长(18→66 行 ≈ 49→60ms/节点);Instantiate 成本
≈0.85ms/行随节点数线性。绝对值受负载窗影响,斜率形状是结构性结论。

## 4. 端到端吞吐 / 时延(worker 面,真 relay + asynq + worker)

| 并发 | relay 轮询 | nodes/s | execs/s | p50 ms/节点 | p99 ms/节点 |
|---|---|---|---|---|---|
| 1 | 1s(生产缺省) | 6.51 | 0.41 | 1155 | 1173 |
| 4 | 1s | 6.30 | 0.39 | 1252 | 1377 |
| 8 | 1s | 6.37 | 0.40 | 1162 | 1217 |
| 1 | 20ms(压缩对照) | **29.5** | 1.85 | **208** | 225 |
| 8 | 20ms(压缩对照) | 7.41 | 0.46 | 971 | 1148 |

结构性结论(管道模型,不随机器变):

1. **时延地板 = relay 轮询周期**:链式 DAG 每跳经 outbox,稀疏流量下
   relay 空转间隔直接成为每节点时延地板——生产缺省 1s 档 p50 ≈
   1155-1252ms/节点(逐跳等轮询),压缩 20ms 档 p50 = 208ms = 引擎
   管道净成本(调度事务 ×2 + 队列往返 + asynq 派发,负载 52 下)。
2. **吞吐帽 = relay 单事件单事务**:worker 并发 1→4→8 在生产轮询档下
   吞吐纹丝不动(6.51 / 6.30 / 6.37 nodes/s,差 < 3%)——帽在 relay
   主循环(单行 FOR UPDATE SKIP LOCKED + 单 Redis 入队/事务),不在
   worker。多副本 relay / 队列 HA 是 T15.1 的裁定面,本曲线即其消费数据。
3. **高并发 × 快轮询在 DB 争用下反噬**:20ms+8 并发档吞吐塌至 7.4
   nodes/s、p50 回升 971ms——8 路 Complete 收口事务(各自全行锁)+
   relay 单事件事务在共享 innodb(buffer_pool 128MB)上互相挤压,中转
   瞬态错误触发 relay 退避链(5s→20s→80s→320s)。独占窗口下该档预期
   改善,但「加并发先过 DB 写串行段」是结构性结论:并发度 = f(DB 写
   延迟),不是免费旋钮。

## 5. 大 DAG 回滚锁窗(MaxRollbackRows 定档依据)

| 行集(控制行+实例行) | RollbackTo 墙时(= 锁窗上界) |
|---|---|
| 100 | 0.66 s |
| 300 | 1.78 s |
| 1000(单 fan × 997 实例) | 6.64 s |
| 3994(4 fan) | 9.98 s |
| 10980(11 fan) | 7.34 s(方差窗内与 3994 倒挂,见 §1) |
| 296(真路径锚,3 fan × 97) | 0.42 s(同形状 fixture 档 1.78s = 负载方差实证) |
| 100(容量闸拒绝,cap=2) | 12.9 ms(COUNT 索引范围扫 + 拒绝面 = 生产配置每笔回滚的闸内开销) |

- **>4000 档 = 定档输入,非生产可达形态**:主网格以闸关闭(MaxRollbackRows=0)
  量闸外曲线(定档的依据);生产缺省配置下 >4000 行的回滚在锁行前即被
  409 拒绝(拒绝成本见末行)。
- 墙时含前置两点查与提交后 finishIfDone(独立事务),锁窗 ⊆ 墙时;
  回滚事务内含失败源闭包重置 + advance 重派(fan-out 节点的实例重
  claim 与快照重求值计入锁窗——生产形态)。
- **曲线分解**(两段线性拟合):纯锁行/扫描 ≈ **1.1 ms/行**;回滚事务内的
  fan 重派(fanout 重解析 + 逐实例 claim + 快照重求值)≈ **5.8 ms/实例**——
  ≤4k 行段的主导项是 fan 重派而非锁本身。
- **定档**:`DefaultMaxRollbackRows = 4000`——负载态最差形状(单 fan ×997
  实例)≈ 10s、典型形状 ≤10s 包络;独占窗口按比例折好(§1 方差带)。
  DSL 上限形状(512 fan × 1000 实例 ≈ 512k 行)外推锁行项 ≈ 分钟级 =
  守卫的结构性存在理由。env `BRAID_ROLLBACK_MAX_ROWS` 覆盖,`0` 关闭;
  超限 409 `rollback_too_large` + `rollback.denied` 审计(`reason=too_large`)。

## 6. 审计查询延迟与 audit_trace_map 裁定(D-t63#3)

| 体量 | trace hit / miss | actor hit / miss | 深页(50 页游标) |
|---|---|---|---|
| 200k 行 / 13 分区 | 2.4 / 0.5 ms | 6.8 / 4.5 ms | 4.9 ms/页(50 页 244ms) |
| 1M 行 / 13 分区 | 3.5 / 3.5 ms | 17.7 / 3.1 ms | 16.9 ms/页(50 页 846ms) |

**裁定**:v1 不建 audit_trace_map **维持**(D-t63#3 原判继续成立)——判据 =
交互容忍线 1s;实测最重形态(actor 深页,1M 行)16.9ms/页、整走 846ms,
距该线 ≈ **59×**;体量 5×(200k→1M)延迟增长 ≈ 2.5×(actor-hit 6.8→17.7ms),
外推破线需 ≈ 数千万行级或分区数 ×10+。
唤醒触发(任一命中即重议):审计行体量 > 2000 万行(按 2.5×/5× 体量斜率
外推破线量级)、保留期拉长使分区数 > 130(≈ 现窗 ×10)、或交互延迟实测超标。

## 7. 外推模型(容量估算)

- 每节点端到端成本 ≈ 引擎管道净成本 208ms(§4 压缩档,负载 52;调度
  双事务 + 队列往返 + 派发)+ relay 轮询等待(稀疏 ≈ 1s/跳;密集摊零)
  + 执行器外部调用窗(不占 engine 资源,占执行时延与 worker 并发槽)。
- 单 worker 进程吞吐帽:生产形态(relay 1s)≈ **6.4 nodes/s**;轮询
  压缩后净帽 ≈ **29.5 nodes/s**(单并发,负载 52 下)——两帽都在 relay,
  worker 并发不解除。日吞吐外推(生产形态):6.4 nodes/s ≈ 55 万节点/天
  ≈ 3.4 万次 16 节点执行/天(单副本、echo 负载;真实 LLM 调用窗不占
  engine 资源,吞吐不降,但受并发槽与外部配额另限)。DB 写压 = 每节点
  2 事务(收口 + 心跳摊销)+ Instantiate/执行级事件。
- 大 DAG 预算:节点收口成本随行集线性增长(§3),回滚锁窗同斜率(§5)
  ——>4000 行的执行设计上应拆分(触发侧即受 512 节点 × fanout 1000 的
  DSL 上限约束,实测档位见 §5)。

## 8. 已知边界

- tmpfs 写路径(生产盘速打折);负载竞争窗(§1);即回执行器(真实
  LLM 时延不改变 engine 侧结论,只改变执行时延与并发占用);
  audit bench 种子绕过哈希链写路径(读面延迟不依赖哈希列,量值不受
  影响;写吞吐面未测——chain_heads 锁另属 audit 写路径任务)。

## 9. node_attempt 留档体量面(T18.6,D-t186)

- **表画像**:全库最高频业务行表面——每 attempt 一行,T9.2 起
  input_snapshot/output JSON 留档列承载旧轮证据四件套;保留期默认
  **30d**(`BRAID_REAPER_NODE_ATTEMPT_RETENTION`,≥24h 启动即败),
  janitor 终态行批量 DELETE(单批 500 / 批间 100ms,**专属单轮上限
  25000** 非共用 1 万,见下),RUNNING 永不删 + 活执行守卫(非终态
  执行证据永续)。
- **清理帽与摄入帽对账(对抗审计 2×HIGH 修正)**:本面摄入 = 无界
  业务吞吐;§7 有两档 worker 帽,**对账前提 = 生产轮询形态(relay
  1s)帽 6.4 nodes/s ≈ 55.3 万行/天**。若共用
  janitorRoundCap=1 万 × 24 轮 = 24 万/天 < 摄入帽,满帽持续负载下
  保留期**无界漂移**(净积压 ≈ 31 万行/天)——故走专属
  nodeAttemptRoundCap=**25000**:60 万/天 > 该形态摄入帽(盈亏平衡
  6.94 行/s,余量 9%),轮活跃代价 ≈ 50 批 ×(DELETE ~10ms + 批隙
  100ms)≈ 5.5s。**超帽形态登记**:①多副本(摄入随副本线性放大);
  ②§7 压缩轮询形态净帽 ≈ 29.5 nodes/s ≈ 255 万行/天——**单副本即超
  本帽 4.3×**,保留期在该形态满帽下漂移(algo 补齐审查);届时以
  调大轮上限/缩 JanitorInterval(下限 5m = 288 轮 × 2.5 万 = 720
  万/天)扩容,唤醒信号见下。
- **存量首排**:升级前该表无清理路径,存量积压按清理帽排空(如
  90d × 满帽 ≈ 5000 万行 ≈ 83 天;压 tick 至 5m ≈ 7 天)——排空期
  保留期超 30d(晚删方向,无窗内证据丢失),属一次性迁移窗。
- **稳态行数外推**(§7 口径,前提 = 摄入 ≤ 清理帽):6.4 nodes/s ≈
  55 万 attempt/天(单副本 echo;重试因子 ≤10 被 max_attempts 钳制)
  → 30d 稳态 ≈ **1650 万行**;90d 档 ≈ 5000 万行——JSON 留档列
  体量 ×3,是 30d 定档的容量否决依据。
- **行体量口径**:骨架列 ~0.5KB + JSON 留档列(llm 节点非空,体量随
  prompt/输出执行画像放大);容量预算 = 稳态行数 × 画像实测均值行长,
  部署侧以本式折算存储配额(外推级,非实测——与 §7 同档)。
- **唤醒触发(重议保留期/轮上限/加索引)**:①稳态行数 > 5000 万
  (30d 档 3×:多副本摄入 > 单 reaper 清理帽 6.94 行/s、压缩轮询形态
  满帽(255 万/天),或 JanitorInterval 被调大);②待删积压持续增长/
  删除年龄持续抬升(摄入超帽的运行时信号,不等行数越限);③台账/
  追溯查询因行量现秒级延迟。与 §5 的关系:MaxRollbackRows=4000 是
  单次回滚**行数**闸,保留期是**时间**窗,二者正交(D-t186 决策 3)。
