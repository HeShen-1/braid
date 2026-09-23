# 决策日志(ADR)

> v1 的 18 项决策见 docs/ARCHITECTURE.md §1(2026-09-21 经 4 轮评审 + 对抗审计定稿)。本文件只记录 **v1 之后**的新决策,不追改历史。

## 模板

```
## D-YYYY-MM-DD-<slug>
- 背景:
- 决策:
- 理由:
- 影响:(改哪些模块/任务)
```

## D-2026-09-21-snowflake-rollback-bounded
- 背景:0001_init.up.sql 头注释原定「时钟回拨 = 生成器阻塞等待 + NTP 偏移监控告警」(无限阻塞)。T2.2 实现雪花生成器时,三视角设计中的事务正确性视角指出:outbox.Append 发生在状态迁移事务内,时钟回拨期间无限阻塞 = 该进程**全部引擎写入停摆**,故障被写入路径无限吸收。
- 决策:回拨 ≤ maxBackwardWait(默认 2s,覆盖 NTP 微调常见形态)时阻塞等待时钟追平;超过则快速失败返回 ErrClockBackward(不重试,触发 NTP 偏移告警)。经用户确认(2026-09-21)。
- 理由:小幅回拨是可等待的正常抖动;大幅回拨是受监控条件(告警在),写入路径不应替它买单。有界等待保留了原决策「不产出回退 ID」的正确性内核。
- 影响:internal/snowflake(实现);0001_init.up.sql 头注释同步修订(仅注释,无 DDL 变更);T7.1 配置化 maxBackwardWait。

## D-2026-09-22-git-native-collab
- 背景:handoff 只解决跨会话状态,不支持并行分叉(SKILL.md「何时不交接」节自认缺口);单 worktree 串行吞吐受限,review 全靠会话内 6 视角审计,任务执行状态锁在 ROADMAP 勾选行。经社区模式调研(官方 worktree 并行文档 / vibe-kanban / Copilot issue→PR 流)+ 5 读者对抗式代码调查(碰撞面取证),grill 会话 13 问定稿,用户确认(2026-09-22)。
- 决策:
  1. 目标:并行吞吐 + 任务状态外化 + AI review 流程化(附带收益,不作主目标)。
  2. 远端:新建 private 仓 `braid`(gh 已认证);推送前全历史密钥/敏感内容扫描(gitleaks 级)+ docs 人工复查,清白才推。
  3. 真源分层:ROADMAP.md = 里程碑规划真源(任务定义与依赖,不再维护勾选);GitHub issues = 执行队列真源(认领/进行中/闭环)。ROADMAP 勾选提交包含在任务 PR 内,merge 即生效;PR 描述带 `Closes #N` 自动关 issue。
  4. 合并唯一通道 = PR:feat/<task-id> 分支 → push → draft PR → 零密钥 CI(vet+lint+单测,集成测试无栈自动 skip)→ 本地新会话当 AI reviewer(读 `gh pr diff`,findings 用 `gh` 贴回 PR;中转密钥不出本机,不用 GitHub Secrets 跑模型)→ CRITICAL/HIGH 清零 → 用户 GATE 2 在 GitHub 点 merge(merge commit)→ 本地拉回 master。会话内 6 视角对抗审计不变,AI PR reviewer 定位为补充第二道。纯文档仍可直接进 master。
  5. 存量迁移:只导入 M2 剩余(T2.3–T2.6)+ T5.1(调查确认其足迹 internal/auth + migrations/0002 + cmd/api 与引擎侧完全不相交,是首对并行搭档);更远任务派单时懒创建 issue。
  6. worktree 语义:`/handoff fork <task-id>` 一条命令 = 认领 issue + `git worktree add .worktrees/<id> -b feat/<id>`(基于最新 master)+ bootstrap(cp deploy/.env、写 .testenv、初始化 .handoff/);每 worktree 独立 .handoff;注册表 = `git worktree list` + `gh issue list`,不建第二真源;人工派单起步,自动认领(issue assignee + 分支存在双信号 CAS)后续再加。
  7. skill 落点:handoff v2 写入 ~/.agents/skills/handoff(三端共享单真源,不物理拆分),Claude 专属内容继续隔离在「Claude Code 专属增强(不得依赖)」节;编排一律裸 git worktree 命令,不依赖 EnterWorktree 等专属工具。
  8. 测试隔离 b 先行(调查结论:唯一运行时冲突面 = MySQL 三个同名 schema 与 Redis DB 0,均为硬编码;Go 测试代码本身 worktree 干净):
     - schema 名 = base + "_" + sha256(worktree 根绝对路径)[:8];wtKey helper 由 os.Getwd() 向上找 go.mod 自动推导(不依赖 GIT_WORK_TREE 或人工注入,kill_test 的 exec 子进程 cwd 继承天然成立);`BRAID_TEST_SCHEMA_SUFFIX` 作可选覆盖(CI/调试)。base ∈ {braid_engine_test, braid_relay_test, braid}
     - 新增 `BRAID_TEST_MYSQL_DSN`(sqltest serverDSN 与 migrate_test testBaseDSN 共用此口,默认值不变);db 包测试顺带脱离 dev 库 `braid`,改用 `braid_<wtKey>` 独立 schema
     - Redis 侧 `BRAID_TEST_REDIS_DB` 保留不换名,语义扩为 worktree 槽位:DB = wt_slot×4 + pkgBase(relay=0、worker=1、reaper=2),0..15 恰容纳 4 worktree × 4 包;槽位人工分配写进 AGENTS.md(禁哈希自动分配:16 槽下 3 worktree 碰撞率≈18%,失败模式恰是静默互踩);redistest.go 零代码改动,组合发生在消费包 TestMain
     - 单 worktree 行为零变化(所有默认值=现状);约 4 文件、净增 40-60 行、零 compose 改动
     - c(每 worktree 独立栈)= 按需升级,b 的 env 层是其地基:删 t13 compose 硬编码 container_name(纯减法,-p 派生名已唯一)+ 端口改 `${BRAID_T13_*_PORT:-默认}`(host = base + slot×10)+ `-p braid-t13-<slot>`;触发条件 = 活进程联调/整栈重置
  9. 并行公约:engine 包同一时刻单 worktree 持有;migrations 序号独占(第二个迁移任务必须基于已合并 master);go.mod 冲突合并后必须 go mod tidy + 三件套重跑;ROADMAP 勾选写各自 PR,相邻行冲突后者 rebase(纯文档);活进程联调(dev 栈)仅主仓,worktree 只跑测试;t13 栈 tmpfs 契约下 `docker compose restart` 即全清测试 schema。
  10. 已知坑:auto-memory/longhand 按主 checkout 路径键控,worktree 新路径记忆断档;Claude 侧解法 = symlink worktree 的项目 memory 目录到主仓(归专属增强节)。
- 理由:并行是 handoff 自认缺口,且 ROADMAP 今日即有 4 个就绪任务(T2.3/T2.5/T2.6/T5.1);issue 化把执行状态从单文件勾选行外化为 git/gh 可见状态;本地 AI reviewer 保住密钥纪律;b 是 c 的子集与地基,先 b 后 c 零返工;cwd 哈希推导 schema 比 env 注入更不易漏,Redis 槽位显式分配比哈希更安全。
- 影响:docs/AGENTS.md(真源分层/PR 单通道/并行公约/fork 语义/命令节槽位表)、docs/ROADMAP.md(顶部迁移说明 + 新增测试隔离任务)、internal/db/sqltest(DSN env + wtKey helper)、internal/db/migrate_test.go、internal/relay/run_test.go、internal/redistest(仅契约注释)、deploy/migrate-test.docker-compose.yml(c 升级时)、~/.agents/skills/handoff(fork/dispatch 模式)、.github/workflows(零密钥 CI)。前置约束:T2.5 开工前必须先钉死 T2.3 落地的确定性 TaskID 契约(ROADMAP 未标注的语义耦合,写进 T2.5 issue body)。
- 实施顺序:⓪ 全历史扫密 → ① 建私有仓推送 → ② 零密钥 CI → ③ AGENTS/ROADMAP 修订 → ④ 建 issue(M2 剩余 + T5.1)→ ⑤ handoff v2 + 三端冒烟 → ⑥ 测试隔离 env 化(TDD,排 ROADMAP 正经任务)→ ⑦ 首次并行试跑(主仓 T2.3 ∥ fork t5.1)。⓪–⑤ 可与主仓 T2.3 单线穿插;首对 fork 前第 ⑥ 步必须落地。

## D-2026-09-22-conditional-auto-merge
- 背景:D-2026-09-22-git-native-collab 第 4 条把 merge 定为用户人工点击(GATE 2);T2.7 交付后用户提出 merge 自动化诉求,三选一确认「条件自动」(2026-09-22)。
- 决策:PR 闸门(CI 三作业全绿 + AI reviewer CRITICAL/HIGH 清零)满足后,常规任务由本地会话直接 `gh pr merge --merge`(merge commit 形态不变)并事后报告;触及铁律面(migrations 序号、engine 包、go.mod、CI 配置、AGENTS.md 自身)的 PR 仍由用户在 GitHub 人工合。纯文档直进 master 例外不变。
- 理由:单人私有仓,CI + 对抗审查已构成主闸门,人工点击是纯摩擦;铁律面是并行公约的冲突面与不可逆迁移所在,保留人工抽查,自动化不越过风险边界。
- 影响:AGENTS.md「约定·commit」条;merge 收尾(本地拉回 master + 删分支)随自动化一并由会话执行。

## D-2026-09-22-t24-scheduler-semantics
- 背景:T2.4 DAG 调度器(入度推进、桩谓词路由、执行完成回写)实现前,三个调度语义与一个 CAS 形状无既有锁定决策覆盖;用户三问拍板 + 对抗校验(Plan agent)发现两个 P1 竞态后定稿(2026-09-22)。
- 决策:
  1. 失败语义 = fail-fast:任一节点终败(attempts 耗尽 / input_eval / branch_eval)→ 同事务 FinishExecution(FAILED);在飞兄弟自然跑完但终态守卫阻断推进,节点留 EXECUTING 归 T2.5 扫描。
  2. 跳过传播 = 任一前驱 SKIPPED ⇒ 该节点 SKIPPED(递归闭包);派发条件 = 全部前驱 SUCCEEDED。推论:P0 的 branch 臂不可汇合(汇合节点恒被剪枝);T1.2 validate 不禁该形状,后续可在 validator 收紧(独立任务)。
  3. branch 内联求值:路由先于 claim(错误不留 deadline=NULL 的不可扫描 EXECUTING 行),ClaimNode(operands 快照)→ SucceedNode(output=NULL)同事务、无 attempt 行;取路目标进入推进,未取直接后继剪枝+传播。路由决定落 audit branch.evaluated 延后 T2.6(期间取路仅可从后继状态推断)。
  4. 工作流级 outputs 引用被剪枝臂/未产出节点 → 物化为 null(键保留,形状跨路由稳定;validate 对 outputs 豁免祖先约束的必然推论)。
  5. RedispatchNode(status-preserving CAS):节点级重试再派发 = EXECUTING→EXECUTING、attempts+1、worker_id 重写、deadline 刷新、version+1;不写 error_code、不重写 input_snapshot;DDL「attempts claim 时 +1」延展为「平台重试的 claim 即本语句」。
  6. 并发形状:Complete 事务第一条语句序列 = 按 node_id 升序逐行 FOR UPDATE 锁全执行行集(当前读防幻读;锁集全量同序 ⇒ 无部分交叠死锁);残余交错由 engine.Transient(1213/1205)内部重试 ≤3 次(不重跑 executor、不烧 asynq MaxRetry=1 预算)。终态判定在提交后新事务(新鲜快照)——事务内判定在不相交臂并发下漏判;双收口由 FinishExecution 的 status-only CAS 仲裁。锁序实际不变量修正为 node 行(node_id 升序)→ attempt → workflow_execution(最后),与 T2.3 心跳一致;Start 例外先拿 wf(PENDING 执行无 claim/attempt,无并发 Complete)。
- 理由:fail-fast 最简可审计;「数前驱 SUCCEEDED」直接对齐 DDL 索引注释设计意图;branch 内联是 schema 强制(output 恒 NULL、无路由列、无 PENDING→SUCCEEDED 边)下的唯一一致形状;逐行全锁以 N 次索引点查换「零 DBTX 契约改动 + 死锁类消灭」,P0 节点数(数十级)下成本可忽略,批量化留待 DBTX 演进注记。
- 影响:internal/scheduler(新包,依赖 {engine,outbox,dsl,snowflake})、internal/engine/nodecas.go(RedispatchNode)、internal/worker(Outcome.Output + Sched 依赖 + execution.start mux)、internal/engine/wfcas.go(锁序备忘注释修正)、issue #4(T2.5)锁序回归验证结论回帖。

## D-2026-09-22-t25-reaper-semantics
- 背景:T2.5 reconciler(超时扫描、幂等重入队或标 FAILED、Redis 锁选主)实现前,判死阈值、收口事务形状、W1 哨兵等价、扫描靶收口目标态等 12 个决策点无既有锁定决策覆盖;3 视角独立设计 → judge 合成 → 17 条对抗攻击(1C/4H/8M/4L,全部吸收)后定稿(2026-09-22),用户确认(2026-09-23:DECISIONS 10 条 + 验收口径结构等价 + SCAN_INTERVAL 30s)。设计全文 = docs/designs/t2.5-reaper.md(实现蓝本)。
- 决策:
  1. 形态 = MySQL 真相源上的纯周期对账器:候选扫描全部无锁普通 SELECT,处置在短事务内「锁后复核 + CAS 收置」;Redis 锁仅 leader 去重且 fail-open,正确性结构性不依赖 Redis/时钟(ARCHITECTURE「Redis 锁仅留给 reconciler 选主」落地;断 Redis 验收的结构前提)。
  2. 判死 grace = 45s = 3×(RetryDelay+HeartbeatInterval),谓词全 DB 端 NOW(3);env 可读时 grace ≤ RetryDelay+HeartbeatInterval 启动即败(公式有执行点强制)。验收相位钉死:「断 Redis 30s」停机窗 = [t0, t0+30](t0=kill),重派写(≥t0+grace−HB)结构性错开,relay 退避链不触发,3×scan=90s 成立;验收可测化口径 = 结构等价(reaper 锁客户端断不可达端口自动测 + relay 侧人工停容器演练记录)。
  3. 收口事务 reapAttempt 四入口统一分派:attempt 锁后复核五项(收口态/心跳新鲜〔无条件、入口无关〕/deadline 复活/wf 当前读/attempt 存在性三分);DEAD 边 = CloseAttemptDead(RUNNING→DEAD,finished_at 恒 NULL);deadline 路径:started_at ≥ deadline_at(迟到启动)→ ExtendNodeDeadline 一次性重挂(新 CAS,不烧 attempt 预算),否则 DEAD('deadline_exceeded')。
  4. 「EXECUTING + 无 RUNNING attempt」分支:outbox 活事件闸(PENDING 恒活,DISPATCHED 界于 DISPATCH_STALE=30m)→ 有则 skip;无则 attempts<max → RedispatchNode+outbox.Append(新 attempt_no ⇒ 新 TaskID,零冲突),耗尽 → FailNode+FinishExecution(FAILED,'node_failed')(fail-fast 同事务镜像)。
  5. Redispatch-over-Requeue:重派一律 RedispatchNode;RequeueNode 不启用(PENDING 节点无扫描者,会成永久孤儿),保留为 T7.4 runbook 手工原语。预算源 = node_execution.max_attempts,业务失败与 infra 判死同池。
  6. 扫描靶 a(fail-fast 遗留 EXECUTING)收口目标态 = CANCELLED(平台裁决,不伪造业务失败);终态 wf 下 PENDING 后继/WAITING_APPROVAL 遗留 P0 不扫(从窄)。
  7. W1 哨兵:接受等价——attempt 已非 RUNNING ⇒ 收口权旁落,迟到 Complete 走 errAttemptAlreadyClosed 幂等成功;可观测 = ConflictError.Observed==DEAD 的结构化 warn(discarded_by_reaper)+ error_code 数据层区分。前置:DEAD ⇒ 节点必已被重派或取消(同事务约束),破坏需重审。
  8. worker_id 跨接管不刷新(首任归因);接管原子性由 worker 接管分支锁内复核保证(非 RUNNING 即放弃 ack),消灭「接管与判死同拍」重叠执行。
  9. PENDING 执行判死:wf 锁先行(Start 同例外)+ PENDING_TTL=30m + exec: 事件活性闸;RUNNING 终态补判委托 scheduler.FinishIfDone。
  10. janitor:独立 goroutine + 专用 Conn + lock_wait_timeout=5;ADD 升序,{1517,1493} 容忍+复读确认;DROP 前活执行守卫,保留期语义 ≥ retention;p_min 永不 DROP;outbox DISPATCHED 清理双列谓词批删、无 ORDER BY。asynq 积压 > DISPATCH_STALE 的双执行残余窗 P0 接受有界(P1 以 asynq Inspector 硬化)。
- 理由:正确性与 Redis/时钟解耦是对账器的结构要求;分支 B 补齐「EXECUTING+无活 attempt+事件死」收敛路径(对抗攻击 CRITICAL);ExtendNodeDeadline 把「队列吃掉窗口」与「执行超过窗口」分开,deadline SLA 对排队公平且保留挂死检测。
- 影响:internal/reaper(新包)、internal/engine(attempt.go +CloseAttemptDead、nodecas.go +ExtendNodeDeadline)、internal/scheduler(+NodeTimeout/+FinishIfDone 两导出、F 日志)、internal/worker(claimAttempt 接管复核 ~12 行)、cmd/reaper(新入口)、deploy(compose reaper 服务 + ALTER 授权注记)、docs/designs/t2.5-reaper.md、docs/ROADMAP.md T2.5 勾选随 PR。零新迁移。

## D-2026-09-23-t26-audit-events
- 背景:T2.6 最小同步审计写入器实现前,事件词表(D-t25 未决 3 的 reaper 裁决事件收口 + D-t24 #3 延后的 branch.evaluated)、写入器形态、埋点位置三个决策点无既有锁定决策覆盖;S 规模轻量设计,用户确认(2026-09-23)。
- 决策:
  1. 写入器 = 新包 internal/audit,逐字镜像 outbox 范式:`Append(ctx, q engine.DBTX, Event) error`,校验前置不触 DB、永不自管事务;调用方在状态迁移同一事务内调用。同步直写 ⇒ event_at = DB 端 NOW(3)(与 status_changed_at 同钟系;「事件时刻≠落库时刻」的分离归 T6.1 异步化)。
  2. exec_date 应用侧物化 = 单语句 `INSERT..SELECT DATE(created_at) FROM workflow_execution WHERE id=?` + RowsAffected==1 断言(0 行 = execution 缺行,响亮 ErrNotFound,不静默无写);audit_log.id 复用调用方已有 snowflake 生成器。
  3. P0 事件词表五类:`node.state_changed`(全部节点状态迁移,单一类型 + payload from/to,非每边一类型)、`node.redispatched`(status-preserving 重派,scheduler 重试 trigger=attempt_failed 与 reaper 重派 trigger=reaped 同型)、`attempt.reaped`(reaper 三种 DEAD 裁决,attempt_id 列锚定)、`node.deadline_extended`(reaper 迟到启动一次性重挂)、`branch.evaluated`(路由决定,payload 携 next)。
  4. actor 模型:worker 路径 actor_type=worker、actor_id=workerID(调度逻辑运行在 worker 进程内,归因到进程);reaper actor_type=system、actor_id=`reaper:<id>`;user/approval/tool/llm 词表预留(T5/T3.x 接入时落)。
  5. 埋点在调用方(9 处现有调用面:scheduler 4 + reaper 5),engine CAS helper 保持纯粹不内嵌审计——非迁移事件(Redispatch/Extend)与迁移事件走同一调用方路径,埋点在调用方天然同构;新调用点(T4.1 Suspend/Resume、T5 API Cancel)接入时必须同事务落行,nodecas.go 头注释钉契约。
  6. P0 范围外(显式钉死):wf 级状态迁移事件(execution.*)、execution.triggered、attempt 创建/观察收口(node_attempt 状态表已自证)、payload 哈希链(prev_hash 恒 '',T6.1)、trace_id 恒 NULL 占位(T6.2)、pii_masked 恒 0(T6.1);payload 只含 id/码/枚举,密钥永不入内(铁律)。
- 理由:同事务直写使「状态迁移与审计行同存亡」由事务原子性承载(P0 最简可审计);INSERT..SELECT 让分区键物化零额外往返且天然钉住「审计行必有执行锚」;单一 node.state_changed + payload 对齐 DDL 词表示例,查询侧按 payload.from/to 过滤即得每边分布;reaper 裁决单列 attempt.reaped/node.deadline_extended 收口 D-t25 未决 3,使 DEAD 裁决在审计链有 actor 归因(node_attempt 行无 actor 列)。
- 影响:internal/audit(新包)、internal/scheduler(4 埋点)、internal/reaper(5 埋点)、internal/engine/nodecas.go(头注释一行)、docs/designs/t2.6-audit-writer.md、docs/ROADMAP.md T2.6 勾选随 PR。零新迁移、零 DDL 变更。
