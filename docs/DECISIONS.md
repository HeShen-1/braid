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

## D-2026-09-23-t26-audit-writer-lockfree-read
- 背景:PR #13(T2.6)6 视角 review 挂账 #9:RR 隔离下 `INSERT INTO audit_log .. SELECT .. FROM workflow_execution` 的源表读取取共享 next-key 锁并持有至 COMMIT(t13 双会话复现);reaper leaderless fail-open 双扫描同执行两节点时,双方各持 S 后在 [4] `SELECT .. FOR UPDATE` 升级 X → 1213。Transient 重试收敛、无数据风险,但事务 churn 恰发生在 reaper 存在的 Redis 故障场景。用户拍板 P1 修(2026-09-23,issue #14)。
- 决策:修订 D-2026-09-23-t26-audit-events 第 2 条的「单语句 INSERT..SELECT 零额外往返」为两语句:普通一致读 `DATE_FORMAT(created_at,'%Y-%m-%d')` 直出字符串(review 修:不经 driver 的 time.Location 解析——go-sql-driver 把 DATE 解析为 DSN loc 午夜,loc≠UTC 时 .UTC() 换算静默移日;字符串往返 loc 无关)→ `INSERT VALUES`。零锁,created_at 行 INSERT 后不可变,一致读快照与当前读恒同值。NotFound 判定移至 SELECT ErrNoRows。否决修法 (a)(reapOnce 提前 wf FOR UPDATE):只护单调用点、skip 路径徒增锁足迹、且触 D-t25 #3 分派树形状;两语句使审计写入器对 workflow_execution 的锁面归零,死锁类在所有现在与未来调用点结构性消灭。代价 = 每审计行一次 PK 点查(P0 量级噪声;批量去重归 T6.1)。
- 理由:锁的消灭优于锁的搬移;audit 是热路径旁路写入器,不应进入任何锁序推理;S→X 升级死锁是 RR + INSERT..SELECT 的固有语义,调用点各自规避不可扩展(T4.1/T5 新调用点还会再犯)。
- 影响:internal/audit/writer.go(Append 两语句化 + 头注释)、writer_test.go(+TestAppendHoldsNoLockOnExecution 锁回归锚,innodb_lock_wait_timeout=1——行锁等待归 innodb 变量,lock_wait_timeout 只管 MDL)、docs/designs/t2.6-audit-writer.md §1。零行为变化:exec_date/event_at/NotFound 语义逐字保持。

## D-2026-09-23-t31-llm-client
- 背景:T3.1(issue #16)三个计划阶段拍板项(Executor 接线范围 / tiktoken 依赖策略 / 单价表配置形态)不预锁待 GATE 1;另有幂等键线上形态、estimated 列语义、审计 payload 内容三个实现面需随拍板钉死。
- 决策(用户 2026-09-23 GATE 1 拍板 + 实现定稿):
  1. Executor 接线后置:本任务交付 Client/适配器/计费/台账缝(生产就绪组件),worker node-type mux + prompt 标签块渲染 + DSL outputs 映射抽取 + llm 生产装配归新任务 T3.6(ROADMAP M3 末尾追加);台账验收以 fakeExec 走真 asynq 链达成。
  2. tiktoken 不引入,等 T7.2 离线 loader:零 go.mod 变更(PR 非铁律面);estimated=true 语义 = 「cost 非精确」——未知模型走单价表保留键 "fallback" 估算(未配置 fallback 则 cost 两列 NULL=不计费),两态恒 estimated=1;DDL 注释「未知模型 tiktoken 估算 fallback」的 tiktoken 字样是 T7.2 落地后的完整语义,T3.1 先落承载列;pre-call token 预算估算整体归 T7.2。
  3. 单价表 = BRAID_LLM_PRICE_TABLE JSON-in-env(config 四件套范式内,T7.1 配置体系迁居):{"<model>":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"},"fallback":{…}};金额整数定点(big.Int 承载 token×单价乘积防溢出),唯一舍入点 = 最终除 1e6 四舍五入 half-up,DECIMAL(14,6) 域溢出响亮拒绝;免费模型 0.000000 与 NULL(不计费)严格区分。
  4. 幂等键线上形态 = Idempotency-Key 头(att:<node_exec_id>:<attempt_no> 直传,Stripe 惯例,OpenAI 兼容端点普遍容忍未知头);DB 侧 uk_attempt_idem 仍是永久锚,供应商侧同键去重是尽力增强。
  5. llm.call.completed payload 不含 prompt/模型返回自由文本(D-t26 §4 纪律维持):prompt 求值真相源 = node_execution.input_snapshot,响应摘要 = node_execution.output,审计副本只携 usage/cost/model/枚举;Q10 全文范围的其余部分(T6.1 脱敏钩子后评估放开)。
  6. Usage 仅随 SUCCEEDED 收口(失败 LLM 调用 P0 不计费,scheduler.Complete 入口拒绝 FAILED+Usage);适配器 2xx 缺 usage 对象 = 响亮拒绝(ErrMissingUsage),不静默零计费;双录两半边经 engine.AttemptUsage 单源(列域校验单点 Validate,audit 复用)。
- 理由:零依赖策略同时满足验收(mock usage × 单价表)与架构防线(tiktoken 在线下载词表违背内网私有化首跑目标,且 go.mod 是铁律面);接线后置依据 = mux/Request 形态/outputs 映射取数链是独立设计面(快照已含求值后 prompt 但 Outputs 只在 DSL),并入会把 M 撑成 L 且混淆两份验收;JSON-in-env 在「零文件解析先例」与「价格非密钥可 env 承载」之间取最小新范式,单价表大了的形态升级归 T7.1。
- 影响:internal/llm(新包:client/cost/config/errors/openai)、internal/engine(attempt.go:AttemptUsage + CloseAttempt 扩展)、internal/audit(Event.Usage + llm.call.completed 词表 + LLMCallCompletedPayload)、internal/worker(Outcome.Usage 透传)、internal/scheduler(CompletionRequest.Usage + auditLLMCall 同事务发射);零新依赖、零迁移、零 DDL 变更;T7.2 依赖面不变;T3.6 为新增任务。

## D-2026-09-23-t36-llm-executor
- 背景:T3.6(issue #21)LLM 节点执行器接线。并行合并审计(T3.6 开工须知,ROADMAP T3.6 行)移交 2 个 HIGH(llm 预算×30min deadline、response_format 词表漂移)+ 3 个 MEDIUM,须开工前拍板;另有 outputs 抽取语义、无执行器类型语义、e2e 测试包位三个实现面随 GATE 1 钉死。
- 决策(用户 2026-09-23 GATE 1 拍板 + 实现定稿):
  1. HIGH① 预算缝 → Validate 组合上界:llm.Config.attemptBudget = MaxAttempts×Timeout + 精确退避序列(适配器 backoff 同式 + ±20% 抖动上界、逐项 ceil 保守化)≤ scheduler.NodeTimeout("llm")(30min 单源锚定,新增 llm→scheduler import,scheduler 不反向依赖,无环);启动即败,默认配置(120s×3+3.6s=6m3.7s)余量充足;不抬高/配置化 deadline(挂死 SLA 保 30min,配置化本就是 T7.1 换装点)。
  2. HIGH② 词表漂移 → 适配器层翻译:Request.ResponseFormat 定为平台词表 ""|"text"|"json"(与 DSL schema enum 同源);OpenAICompat.buildBody 映射 json→json_object、text→text,未知值(含直写方言值 "json_object")响亮拒绝;ExtraBody 的 response_format 保留字封锁不变,翻译唯一通道。
  3. outputs 抽取语义(GATE 1 语义 a):content 按 JSON 对象解析(UseNumber,大整数原文往返)后逐声明字段抽取并校验 DSL 类型词表;任一缺字段/类型不符/非对象 JSON → FAILED(code=bad_llm_output,节点级重试可重派);outputs 声明为空 → Output={"content":全文}(仅供审计观察,T1.2 保证未声明字段不可被引用)。
  4. no_executor 语义(GATE 1 语义 b):Mux 对无执行器类型(tool/approval)产出 FAILED(code=no_executor)终态收口——部署配置错要可见终态,不留 RUNNING 行给 reaper 判死循环重派;定义读取 ErrNotFound → FAILED(node_def_missing);DB 读失败 → error 返回(asynq 重试接管)。
  5. MED① dsl_hash 重锚 compact 形态:hashDSL = SHA-256(compact(读回存储字节))。原口径锚定 MySQL JSON 读回原文(带空格),与一切 JSON 传输形态(compact)必然不同,API 消费方复算恒假阳性(闭环测试实证);重锚后执行侧漂移检测与 API 消费方复算同一字节;dev 期零迁移成本,上线后再改需批量重算。MED②:api 5 env + worker/llm 组补齐 deploy/.env.example。
  7. 审计修复(44-agent 6 维度对抗审计,1 HIGH + 8 独立 MEDIUM/LOW,4 refuted;双镜头验证存活项全修):①HIGH 预算上界追加 dispatchMargin(5min)扣减——asynq 每投递 ctx 默认 30min 自 dequeue 起算恒先于 reaper(自 claim)收紧,吃满 NodeTimeout 的合法配置必被 mid-call 掐断;②ctx 取消(DeadlineExceeded/Canceled)从业务失败改归基础设施 error(attempt 不收口,asynq 接管/T2.5 判死——取消不是节点业务失败);③429 细分 llm_rate_limited(StatusError 消费契约,其余仍 llm_call_failed);④2xx 缺 model 拒(llm_bad_response,台账归因列不容空);⑤error_message rune 截断 + 换行单行化(VARCHAR 字符语义;字节切割产非法 UTF-8 触发 1366 → 收口死循环 → reaper 循环重派重复计费);⑥MED① 完整契约 = hash 锚 compact 服务端自洽 + API 消费方对含 `<>&` 文档走语义等价(HTTP JSON 编码器 HTML 转义使字节复算不可移植,两个测试分别钉可复算子集与语义保真);⑦snowflake 槽位表注释改真话(显式配置面仅 api=1/worker=2,relay 无生成器,reaper PID 派生待 T7.1)。移交:reaper 判死后迟到 SUCCEEDED 携 usage 被 Complete 让位丢弃——已计费调用零台账痕迹(P3;需 llm.call.discarded 类审计词表设计,独立任务承载,T6.4 前)。
  6. 包结构:internal/nodes 新包(Mux + LLMExecutor,实现 worker.Executor;nodes→{engine,dsl,llm,scheduler,worker} 依赖,cmd/worker 顶端装配);e2e 归 nodes 包(同包测试 import worker 合法,nodes 产线已依赖;worker 包测试反向 import nodes 成环被 Go 禁止);nodes = 第 4 个 Redis 消费包,基号 3(relay=0/worker=1/reaper=2/nodes=3,0..15 = 4 槽位×4 包,AGENTS.md 并行公约登记)。
- 理由:预算缝收口在 Validate = 防线最早(启动即败)且不动 scheduler/reaper 语义;词表翻译在适配器 = DSL→Request→适配器全链一个平台词表,漂移面最小;outputs 声明即契约,响亮失败优于静默错值;no_executor 终态化避免 reaper 死循环;hash 重锚是唯一能让消费方复算成立的口径,且当前是零迁移窗口。
- 影响:internal/llm(config 组合上界 + openai 词表翻译 + client 注释)、internal/scheduler(NodeDefinition 只读缝导出 + LLMInput 形状契约导出 + RenderText 导出)、internal/nodes(新包:mux/llm + e2e)、internal/worker(config.SnowflakeNode)、cmd/worker(生产装配去占位)、internal/api(hashDSL 重锚 compact + MED① 闭环测试)、deploy/.env.example(api/worker/llm 组)、AGENTS.md(Redis 基号表登记 nodes=3);零新依赖、零迁移、零 DDL 变更。

## D-2026-09-23-t32-tool-runtime
- 背景:T3.2(issue #24)工具 registry + ToolRunner 接口 + 本地进程实现 + per-call scoped token(签发/过期/传递)。GATE 1 拍板 4 点(混合载体、token 密钥策略、BRAID_TOOL_ALLOWLIST 退役、失败码词表)+ 实现面(tool 执行器接线进 Mux 系 mux.go/main.go 既有注释锚定、审计事件命名对齐 DDL :238 预留词)。
- 决策(用户 2026-09-23 GATE 1 拍板 + 实现定稿):
  1. 混合载体:registry 条目统一 `Tool` 接口(Name + Run);进程内工具(T3.5 内置 http/file_read 形态)直接实现;`CommandTool` 提供子进程载体(stdin JSON 入 / stdout JSON 出 / env 白名单从零构造:PATH+LANG+BRAID_TOOL_TOKEN+BRAID_ATTEMPT_KEY 四类可见,平台主凭证结构性不可达)。「本地进程实现」= LocalRunner 编排层(token 门→registry 分派→产出契约校验)+ 两类载体;P1 换 Docker 只替换 CommandTool 同位载体,registry/Runner/执行器不动(§3.4「不动核心」)。
  2. scoped token:无状态 HMAC-SHA256(payload:ne/an/tl/exp/nc,Verify 三域 scope 比对 + 过期);密钥默认进程内 32B 随机(重启轮换,P0 校验点 = LocalRunner 调用门,同进程自洽),BRAID_TOOL_TOKEN_SECRET(hex)显式供给为 P1 跨进程校验预留;DefaultTokenTTL=60s;时钟注入可测。校验门在分派之前:过期/无效 token 下工具零触达(验收语义)。
  3. BRAID_TOOL_ALLOWLIST 退役:发布白名单真源 = tools.Registry 注册名(api.NewServer 注入 registry,publish 点 Names() 换源);空 registry = 禁用一切工具(语义同原空名单);api 与 worker 各自装配同一注册集(T3.5 起内置)。Config.ToolAllowlist 字段、env 解析、.env.example 行全部移除。
  4. 失败码词表:tool_not_found / tool_token_expired / tool_token_invalid / tool_failed / bad_tool_output(+ bad_input_snapshot 沿用 nodes 惯例);分类单点在 tools.CallError(业务失败)vs 裸 error 链 ctx(基础设施失败,attempt 留 RUNNING),nodes.ToolExecutor 只透传。
  5. 审计:tool.call.completed(DDL :238 词表注释预留同名)对称 llm.call.completed——worker.Outcome.ToolCall(仅 SUCCEEDED)→ scheduler.Complete 同事务发射,actor=tool/工具名,payload{node_id,tool,duration_ms} 无 token/凭证(短时凭证本体同样不留痕,e2e 断言整行零 token 泄漏);非 tool 节点携 ToolCall 响亮拒绝;失败路径零发射(观测由 node.state_changed 承载)。
  6. 幂等键传递面:Call.IdempotencyKey = TaskID(att:node_exec:attempt)进 Call,子进程经 BRAID_ATTEMPT_KEY env(消费归工具实现,T3.5 http 透传为请求头)。
  7. 审计修复(27-agent 6 维度对抗审计 + 3 怀疑者投票,1 HIGH + 1 MED + 4 LOW 确认,1 条 3/3 refuted;HIGH 由审计者独立程序实证后修):①HIGH 孙进程管道吊死——SIGKILL 只杀直接子进程,shell 后台孙进程继承 stdout/stderr 管道写端致 copy goroutine 永无 EOF、cmd.Run 无限阻塞(10min deadline 失效、goroutine/asynq 槽泄漏);修 = cmd.WaitDelay(默认 10s,CommandSpec 可配)kill 后强制断管,ctx 归因优先于 ErrWaitDelay/ExitError,补 `sh -c "echo hi; sleep 30 &"` 回归;②MED 无上限 Collect OOM——stdout/stderr 换 limitBuffer(8MiB/64KiB 封顶,超限丢弃记 overflow = 工具业务失败),写端不背压靠 ctx kill 收口;③LOW 相对路径拒(filepath.IsAbs:exec 初始解析走父进程 PATH,沙箱 PATH 只约束孙进程,同名顶替二进制会完整收到沙箱 env);④LOW t54 契约文档白名单行同步退役说明;⑤LOW 同义反复测试锚改名 TestToolExecToolCallBridge;⑥LOW e2e payload 负向断言(键集恰三键,防自由文本字段混入审计行)。
- 理由:无状态 token 免存储且 P0 校验面自洽,机制完备为 P1 回调预留而不提前建 API;白名单换源让「注册」成为唯一事实(定义引用的工具必然可执行,消灭 env 与 registry 双真相源漂移);env 白名单构造式(非黑名单过滤)使主凭证泄漏成为结构性不可能而非配置责任。
- 影响:internal/tools 新包(token/registry/command/runner,零 Redis 消费=不扩 DB 基号分配表)、internal/nodes(tool.go 执行器 + Mux 三参 + 双 e2e)、internal/scheduler(ToolInput 导出 + CompletionRequest.ToolCall + auditToolCall)、internal/worker(Outcome.ToolCall)、internal/audit(词表 + ToolCallCompletedPayload)、internal/api(白名单换源 + Config 删字段)、cmd/{api,worker}(registry/LocalRunner/TokenIssuer 装配)、deploy/.env.example;顺手修 internal/auth TestParseTamperedSignature flaky(篡改位取签名段首字符——base64url 末字符低位是无解码语义的 padding 位,≈4/64 概率翻转无效位致校验照过)。零新依赖、零迁移、零 DDL 变更。

## D-2026-09-23-ssrf-extra-ranges(待议,未拍板)
- 背景:T3.3(issue #26)SSRF 运行时防护落地后的 6 维度对抗审计提出:ipForbidden 锁定口径(ARCHITECTURE §3.4 L76:私网/环回/链路本地/CGNAT/metadata)之外仍存在环境依赖的可拨保留段。**本条仅登记,不改实现**——扩禁段属锁定口径变更,按铁律须用户确认。
- 候选段(按审计优先级):
  1. NAT64 转换前缀 64:ff9b::/96(内嵌 IPv4,To4()==nil 逃过 v4 分支;有 NAT64 出口的环境可借它触达内网 IPv4)——优先级最高;
  2. 198.18.0.0/15(RFC 2544 基准段,部分云 LB 内部使用);
  3. 192.0.0.0/24、0.0.0.0/8 非全零地址、240/4 剩余部分(仅广播已拒)。
- 现状口径:上述段当前放行,TestIPForbidden 有 want=false 锚点钉住(0.1.2.3 / 240.0.0.1 / 198.18.0.1)——若拍板扩禁,同步改锚点为 want=true 即可,测试已就位。
- 关联:T3.3 实现 internal/tools/ssrf.go;无 HTTP 拨号合法目标落在这些段的常规环境,属低概率环境依赖面。

## D-2026-09-23-t41-approval-suspend
- 背景:T4.1(issue #27)审批节点执行路径:worker 遇 approval 节点挂起 WAITING_APPROVAL、输入快照落库、审计记挂起事件、worker 不阻塞(Q4/L67);续跑(outbox+resume TaskID)属 T4.2、超时策略属 T4.3。勘察确认 WAITING_APPROVAL 全链已备(engine 常量/边矩阵/SuspendNode/SuspendExecution CAS、DDL ENUM、快照不变量、reaper 对 wf WAITING_APPROVAL 的合法静默排除),缺的只是 nodes 执行器 + worker/scheduler 挂起通道 + 审计词表登记——零迁移、engine 零改动。
- 决策(用户 2026-09-23 GATE 1 拍板 + 实现定稿):
  1. 执行级镜像的静默规则(quiesce rule):节点挂起事务内(全行锁在手)数在飞兄弟——无其他 EXECUTING ⇒ 同事务 SuspendExecution(RUNNING→WAITING_APPROVAL,wfcas「全部节点挂起后的执行级镜像」注释的本意);有(合法 fan-out DSL,P0 无并行原语但多后继可并发派发)⇒ wf 留 RUNNING(TouchExecution 照刷)。不变量「wf WAITING_APPROVAL ⇒ 无 EXECUTING 节点」可证保持(reaper scan_attempt 的 T4.1 契约面);fan-out 降级表示为良性(reaper 终态补判对它恒 no-op,代价仅索引扫描噪声),不在 Complete 热路径补对称检查(P0 常态线性/分支单活动路径恒走转态分支,推测性泛化不值)。
  2. 审计双行:node.state_changed(EXECUTING→WAITING_APPROVAL,与全部既有迁移一致,nodecas 调用方契约)+ 新登记 approval.requested(DDL 0001:238 预留名;payload=approvers/mode/timeout_hours/on_timeout/deadline_at,T4.4 待办与 T6.4 积压指标免 join 字源;快照全文真相源在 input_snapshot,payload 不复制自由文本——D-t26 §4 纪律;actor=worker,scheduler auditRow 同源)。
  3. T4.1 零 outbox 事件:approval.notify(DDL 预留)消费方在 T4.2/T4.5,现在发=死事件;T4.2 接手时再登记。
  4. schema default 挂起面补齐:DSL 解析不物化 schema.json default(approvalNode 缺省 mode/timeout_hours/on_timeout = Go 零值),scheduler.effectiveApprovalSpec 单点补齐(any/escalate/72h,Q9 口径)供 deadline 与审计 payload;72h 用字面常量与 nodeTimeouts["approval"](claim 执行超时词表,语义不同源)解耦。对抗审计 MEDIUM 修:补齐面扩至快照物化点(eval.go buildSnapshot 同源调用),快照(审批人可读面)/deadline/审计 payload 三处真一致——原实现快照存 DSL 零值与另两处分叉,且缺省快照形状零断言,均已收口。
  5. 通道语义:Outcome.Suspend bool(仅配 SUCCEEDED、禁携 Output/Usage/ToolCall,worker.suspendOK 校验=零 DB 单测面)→ handler 挂起分支 → Sched.Suspend(与 Complete 平行的方法,sched.go 零改动=零并行冲突面);ApprovalExecutor 零依赖纯决策(ctx 不观测);终态守卫镜像 Complete 失败分支(迟到挂起只收口 attempt,节点留 EXECUTING 归 T2.5);快照守卫在挂起事务内(engine 不变量 WAITING_APPROVAL ⇒ NOT NULL 的第二执行点);no_executor 钉死测试改用无判别指针合成类型(approval 已接线)。
- 理由:无脑转 wf 态会在 fan-out 下制造 reaper 违约态(EXECUTING 节点居 WAITING_APPROVAL 执行),静默检查以全行锁为界零额外往返;双审计行延续「迁移行 + 域事件行」既有先例(branch 双行、llm 双录);挂起信号走 Outcome 显式字段而非复用 Status,保持 Complete/Suspend 两收口路径语义分立不互串。
- 影响:internal/scheduler(suspend.go 新文件:Suspend/suspendOnce/effectiveApprovalSpec + suspend_test.go)、internal/worker(Outcome.Suspend + suspendOK + suspendNode + approval 种子/三条测试)、internal/nodes(approval.go 新文件 + Mux 四参 approval 分支 + approval_test + e2e_approval_test + mux_test 改型 + 两 e2e 装配点)、internal/audit(EventApprovalRequested + ApprovalRequestedPayload + 词表测试)、cmd/worker(装配四参)、docs/ROADMAP.md(T4.1 勾选);零新依赖、零迁移、零 DDL 变更、engine/dsl 包零改动(t3.3 零冲突面)。

## D-2026-09-23-t55-execution-api
- 背景:T5.5(issue #28)execution 触发/查询/列表 API。engine/scheduler 只读复用(单持有归并行 t4.1),触发复用 T2.4 Instantiate 缝不造第二入口;GATE 1 拍板 4 点(audit 词表归属、触发角色闸、idempotency 处理、task_list)+ 实现面(接口缝形状、响应契约、审计事务边界)。
- 决策(用户 2026-09-23 GATE 1 拍板 + 实现定稿):
  1. api↔scheduler 缝:api 包定义单方法接口 ExecutionTrigger{Instantiate},*scheduler.Scheduler 结构性满足,NewServer 第 5 参数注入(生产装配 cmd/api 构造 scheduler.New(Deps{DB,IDs}))。Instantiate 仅触 MySQL(scheduler.Deps 零 Redis)⇒ api 保持零 Redis 消费包(AGENTS 分配表不动);测试双轨 = fake 替身钉 HTTP 契约面(无栈可跑)+ t13 真链集成断言三表行集。
  2. 路由与契约:POST /api/v1/executions{definition_name, version?, input?}——version 缺省 = LatestPublished 钉版(store.go:249 注释钦定),显式 version 须 PUBLISHED(ARCHIVED→409;解析→物化之间被归档的 TOCTOU 竞态窗由 Instantiate 内部 PUBLISHED 复核兜底→404);input 缺省/null 同义(零输入→{}),非对象前置 400(镜像 sched.go 拒绝:放行会在 Start 处确定性失败、执行永卡 PENDING)。GET /executions/:id = 行视图 + nodes[](uk_node_exec 字典序,DDL 钦定确定性排序)+ audit 指针 {exec_date(DB 端 DATE_FORMAT 单源,与 writer 物化同式), trace_id(T6.2 前恒 null 不伪造)}。GET /executions?name=&status= = 跨版本 name 过滤(idx_exec_name_created 钦定)+ status 白名单 + 分页 envelope 与 definitions 同构(parsePageParams 单源抽取)。触发响应是 Instantiate 契约回显(id 字符串化 2^53)非 DB 读回——fake 缝无行可读,完整视图走 GET。
  3. 角色闸:触发 RequireAnyRole(author|admin)(触发消耗 LLM 预算 = 写操作,与 definition 写面一致);查询/列表任意有效 token(读面)。
  4. audit 词表:execution.triggered 本任务登记(DDL :238 开放词表注释预留同名,D-t26 §6 显式留给 T5 接入;与并行 t4.1(持 audit 词表)的协调 = 纯加法 4 行,先合者定形后者 rebase,GATE 1 拍板)。审计行 = Instantiate 提交后第二事务 audit.Append(actor=user/JWT sub,payload 仅 {definition_id, trigger_type},无 input 正文影子存储);二段事务非原子是已知缺口:失败 slog.Error + 仍返 201(500 诱导客户端重试 = 重复触发),身份事实源由执行行 triggered_by 列(DDL 钦定)保底。顺序无保证(对抗审计补充):该行与 scheduler 侧经 relay→worker 落的 node.state_changed 完全无序,不保证为该执行审计链首行——T6.1 哈希链 / T6.3 查询不得假设「触发行 = 首行」,归因以行级 actor/triggered_by 为准(雪花 id 是公平调度序非因果序)。
  5. idempotency_key:本任务不实现(写入须改 scheduler INSERT = 违 engine/scheduler 只读红线);定名 HTTP header `Idempotency-Key`,作用域 = 主体+定义二维(防全局键跨主体抢注,呼应 DDL :75 注释);实现归可改 scheduler 的后续任务(T8.1 双投递收敛前必须落地)。
- 理由:单方法缝把 api 对 scheduler 的依赖面收窄到 InstantiateParams 一个结构(其字段变更才产生 rebase 面);审计二段事务是只读红线下唯一路径,缺口由 triggered_by 列 + slog 观测兜底、根治(进物化事务)留 t4.1 后;幂等键「钉决策不越线」——DDL 已允许 NULL=不去重,P0 靠客户端防重。
- 影响:internal/api(execution.go 触发+查询 handlers、execstore.go 读原语、NewServer 5 参 + ids 字段、parsePageParams 抽取、六表清理链 cleanExecutionChain)、internal/audit(execution.triggered 词表 + ExecutionTriggeredPayload)、cmd/api(scheduler 装配);零新依赖、零迁移、零 DDL 变更;api 仍非 Redis 消费包。评审移交:T5.3 IdP 换源时消费面需镜像 sub ≤128 字节(triggered_by 列宽,防 1406→500 与审计静默丢行);触发无限流 = idempotency 缺位已接受面的自然延伸,限流面归 T6.4/T8 评估。下游:T8.2 经 API 触发的端到端示例。

## D-2026-09-24-t42-approval-resume
- 背景:T4.2(issue #33)审批 API:或签决定、经 outbox 续跑、resume TaskID 豁口(HIGH,issue 硬约束)。GATE 1 拍板 5 项(ID 形态/或签语义/表设计/续跑路径/顺路两件)+ 实现面(词表登记、输出契约、守卫顺序)。
- 决策(用户 2026-09-24 GATE 1 拍板 + 实现定稿):
  1. HIGH resume TaskID 豁口 → 新形态 `resume:<exec>:<node>:<attempt>`(attempt = 续跑将创建的 attempt 号,node 行锁内读出,确定性):恢复 outbox 词表不变量「同一 task_id 永不表达需要再投一次的新意图」——同一续跑意图的对账重投同 ID(ErrTaskIDConflict 幂等收敛),新续跑轮次(P1 回滚重挂)新 attempt ⇒ 新 ID,archived 滞留副本不再挡死。词表登记 append.go 正则 + 测试(旧三段式拒收锚);契约记录 = 0003 头注释(0001 §5 已应用不可改,由 0003 扩展)。否决「成文一次性论证」:论证在 P1 回滚落地日即作废。
  2. 或签语义 = 第一票定局(APPROVED/REJECTED 任一方向);节点已被决定 → 409(他人票 already_decided / 同人改判 decision_conflict);同人同决定重复 POST → 200 重放零新写。并发双票由「锁 node 行 + CountDecisions + uk_decision_approver」三层收敛(uk 是同审批人并发双投的 DB 兜底,败方事务外读回裁决)。
  3. 迁移 0003 = 新表 approval_decision(snowflake PK、uk(node_execution_id, decided_by) 每审批人一票、decision ENUM('APPROVED','REJECTED')、comment VARCHAR(2000) NULL、decided_at;FK×2 RESTRICT;append-only 无 UPDATE 面)。决定全文真相源在本表,audit payload 只携 id/码/枚举(D-t26 §4)。
  4. 续跑路径(镜像 execution.start):api 决定事务单事务 = 锁 node 行 → 守卫链(类型/成员(越权 403 + approval.decision_denied 审计)/重放/首票/状态/终态)→ decision 行 + approval.decided 审计行 + outbox execution.resume 事件(DDL §5 预留词)三写同存亡;节点状态零变化(续跑权威在 worker)。worker mux 新 handler → scheduler.Resume(resume.go):锁全行集 → 终态守卫 → 类型守卫 → 幂等收敛守卫 → 决策行复核(防伪事件)→ 快照守卫 → ResumeNode(复入新 attempt,快照原值重写)→ **执行级镜像先于终局化**(wfEdges 无 WAITING_APPROVAL→FAILED/SUCCEEDED 直达边,必须先 ResumeExecution 回 RUNNING)→ 分岔:批准 = outputs 声明校验 → SucceedNode + 下游推进;驳回 = FailNode(approval_rejected)+ fail-fast。复入后不回 executor(纯挂起决策再执行只会再挂起),attempt+1 无 attempt 行(branch 内联同先例)。
  5. 审批节点 output = {decision, decided_by, comment}(全 string;decision 词面 approve/reject 请求面小写)。声明即契约镜像 llm extractOutputs:声明字段不在产出集或类型非 string → FAILED(bad_approval_output)。
  6. 顺路① Idempotency-Key(D-t55 §5 兑现):header 可选,作用域 = 主体+定义二维,落库 = SHA-256 长度前缀帧(64 hex 适配 VARCHAR(128),域分隔版本前缀 braid.exec.idem.v1);Instantiate 参数化 + uk_exec_idem 1062 → ErrConflict(键名判别,PK 撞不映射——append.go 同哲学);重放 200 + 原执行回显(非 201)。顺路② approval.notify 词面常量登记(D-t41 #3 移交;P0 零发射点——发 = 死事件,T4.5 SMTP 落消费方)。
  7. 审批面 API 形状:GET /executions/:id/approvals/:node_id(任意有效 token——读面惯例,节点状态本就开放;**decisions[] 时间线含 comment 自由文本亦在开放读面**,对抗审计 LOW 确认:这是 comment 全文唯一暴露面,T4.4 审批页需要它,T5.2 RBAC 换装时可收窄到名单内审批人)+ POST .../decision(门 = DSL approvers[] 快照真相源,角色闸不叠加——T5.2 RBAC 换装点;错误码全分面 not_approver/already_decided/decision_conflict/not_waiting/execution_terminal/not_approval)。api 审批事件队列 = Config.ResumeQueue(缺省 default,env BRAID_API_RESUME_QUEUE;DDL §5「审批/续跑与重 LLM 任务隔离」的换装点,worker ServeQueues 必须包含)。
  8. 对抗审计(18-agent 6 维度 + 3 镜头多数票;0 CRITICAL/HIGH,2 MEDIUM 修 + 6 LOW 修/登记)落地项:①MEDIUM 测试锚强化(resume 收口审计行 EXECUTING→SUCCEEDED/FAILED 精确边锚 + node_id 维度——原计数差值断言在 dispatch 前置行面前恒真);②MEDIUM「resume 死信零对账网」登记为**已知边界**:resume 任务两次失败 archived 后执行永挂 WAITING_APPROVAL(outbox 已 DISPATCHED、reaper 扫描④显式排除 WAITING_APPROVAL),对账网归 T4.3(超时扫描最小版:已决票 + 超阈的 WAITING_APPROVAL 幂等重投/终败)与 T6.4(archived 告警),P0 恢复手段 = asynq Inspector RunTask;③LOW InsertDecision 1062 键名判别(uk_decision_approver 才映射 ErrConflict,PRIMARY 撞 = 生成器缺陷走 500,镜像 sched.go 纪律);④LOW 幂等撞键回显校验改按 DefinitionName(与哈希作用域同维;按 def.ID 校验会把跨版本重放误报哈希碰撞 500);⑤LOW resume 轮次守卫(payload.AttemptNo ≠ 锁行 attempts+1 响亮拒绝,P1 回滚重挂后旧轮事件不得套用新轮挂起);⑥LOW 决策锚定复核/轮次守卫补变异锚测试。
  9. 已知边界(登记不实现):①幂等键不绑定请求体(作用域 = D-t55 §5 拍板的主体+定义二维;input/version 分叉的同键重放静默复用首执行,body 指纹校验属增强,后续任务承载);②DSL approvers 词域(^\S+$)宽于 username 词域(^[a-z][a-z0-9_-]{0,127}$),域外名单发布合法但审批永 403——发布期词域校验归 T4.4 前(改动面 dsl/validate.go);③uk_decision_approver 无轮次维度,P1 回滚重挂/会签需扩列(round 维度)——与 ⑤轮次守卫共同构成 P1 前置。
- 理由:四段式 TaskID 是唯一同时保住「同意图重投幂等收敛」与「新意图新 ID」两半正确性的形态;首票定局免「异步窗内多票竞速的会签语义误读」(会签 P1 落地时改 CountDecisions 判据即可);决定事务零节点迁移 = 状态机权威单点在 worker 续跑事务,fail-fast/取消竞速全部收敛在终态守卫。
- 影响:internal/db/migrations(0003 up/down:approval_decision 表 + node_execution 复合 FK 载体键 uk_node_id_exec;补审 LOW 修:双单列 FK 不证同链配对,复合 FK 把「execution_id 必与 node_execution_id 同链」钉死为 DB 不变量 + migrate_test 防回退锚)、internal/outbox(词表正则 + 测试)、internal/audit(approval.decided/decision_denied 词表 + payload)、internal/scheduler(sched.go 词表 + ResumePayload + Instantiate 幂等键 + 1062 映射;eval.go ApprovalInput 导出;resume.go 新文件 + resume_test)、internal/worker(mux 注册 + handleNodeResume + 测试)、internal/api(config ResumeQueue + approvalstore + approval + execution.go 幂等键 + 路由 + 测试)、internal/nodes(e2e_resume 三连:批准/驳回/enqueue 失败注入)、docs/ROADMAP.md(T4.2 勾选)、deploy/.env.example。零 go.mod 变更、engine 包零改动。顺路承载闭环:t55 §5(T8.1 前置解除)+ D-t41 #3 approval.notify 登记。
