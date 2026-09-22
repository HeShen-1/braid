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
