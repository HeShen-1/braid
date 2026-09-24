# braid — 企业级 Agent 工作流编排平台(Go)

**必读顺序**:docs/ARCHITECTURE.md(18 项决策已锁定)→ docs/ROADMAP.md(里程碑规划真源)→ GitHub issues(执行队列真源,`gh issue list`)。

## 铁律

- 不得重新争论 ARCHITECTURE.md 已锁定的决策;有新想法 → 在 docs/DECISIONS.md 追加 `D-<date>-<slug>` 条目,经用户确认后才改变实现。
- 任务定义与依赖只从 ROADMAP.md 取;执行状态只在 GitHub issues(认领/进行中/闭环),ROADMAP 勾选提交包含在任务 PR 内、merge 即生效。一次会话聚焦一个任务。
- 密钥/凭证不进代码、不进日志、不进审计;.env 已忽略。
- `audit_log` 只允许 INSERT;节点状态迁移只经 CAS helper(`WHERE status=? AND version=?`),禁止裸 UPDATE。
- 所有外部调用(LLM/工具/DB)带 attempt 幂等键。

## 约定

- 目录规划:`cmd/`(api | worker | relay | reaper,同一二进制多入口)、`internal/`(engine | outbox | relay | snowflake | nodes | tools | audit | auth | api | config | db | redistest)、`migrations/`、`deploy/`、`docs/`。可开源核心(engine)与企业增值(enterprise)物理分层,不得交叉 import。
- commit:conventional commits,英文主题(feat/fix/docs/test/chore/refactor);合并唯一通道 = PR:`feat/<task-id>` 分支 → push → draft PR → CI → AI reviewer(本地会话读 `gh pr diff`,findings 用 `gh` 贴回 PR;模型密钥不出本机)→ CRITICAL/HIGH 清零 → merge(merge commit;D-2026-09-22-conditional-auto-merge):常规任务由会话执行 `gh pr merge --merge` 自动合并事后报告;触及铁律面(migrations、engine 包、go.mod、CI 配置、本文件)时留用户在 GitHub 人工合 → 本地拉回 master。纯文档可直接进 master(唯一例外)。
- 测试:表驱动,`go test ./...` 全绿 + `golangci-lint run` 零告警才算任务完成。
- 文档中文;代码与 commit 英文。

## 远端与并行协作(D-2026-09-22-git-native-collab)

- 远端:private `HeShen-1/braid`;CI = `.github/workflows/ci.yml`(lint / test / gitleaks 三作业,零 Secrets;集成测试无栈自动 skip)。
- fork:`/handoff fork <task-id>` = 认领 issue + `git worktree add .worktrees/<id> -b feat/<id>`(基于最新 master)+ bootstrap(cp deploy/.env、写 .testenv、初始化 .handoff/);每 worktree 独立 .handoff;注册表 = `git worktree list` + `gh issue list`,不建第二真源;人工派单起步,自动认领(issue assignee + 分支存在双信号 CAS)后续再加。
- 并行公约:engine 包同一时刻单 worktree 持有;migrations 序号独占(第二个迁移任务必须基于已合并 master);go.mod 冲突合并后必须 `go mod tidy` + 三件套重跑;ROADMAP 勾选写各自 PR,相邻行冲突后者 rebase;活进程联调(dev 栈)仅主仓,worktree 只跑测试;t13 栈 tmpfs 契约下 `docker compose restart` 即全清测试 schema。
- Redis 测试 DB 槽位(人工分配,禁哈希自动分配——哈希下 3 worktree 碰撞率≈18%,失败模式是静默互踩):DB = 槽位×4 + 包基号(relay=0、worker=1、reaper=2、nodes=3——T3.6 登记,已满;preflight=0 与 relay 共租——零键空间、仅 CONFIG GET 实例级探测,T7.3 登记),0..15 恰容纳 4 个并行槽位(主仓 = 槽位 0,worktree 用 1..3)× 4 包;第 5 个 Redis 消费包出现时必须先扩分配表再动工(0..15 放不下第 5 独立基号;零键空间的实例级探测包可共租既有基号,如 preflight)。测试隔离已 env 化(T2.7 落地):schema 名自动拼 worktree 后缀,`BRAID_TEST_REDIS_DB` 语义即槽位号(组合发生在消费包 TestMain,统一入口 `internal/db/testenv.go`)。

## ECC 编排(可选加速层)

用 `/ecc:orch-*` 家族(ECC 插件,Claude Code 侧)驱动任务实现;与本文件冲突时本文件优先。

- 入口选择:新能力 `orch-add-feature` / 行为变更 `orch-change-feature` / 修 bug `orch-fix-defect` / 纯重构 `orch-refine-code` / spec 起 MVP `orch-build-mvp`;只要计划用 `ecc:plan`。
- 流程:Intake → Research(**默认跳过**:作品深度优先,核心代码自写)→ Plan → TDD 红绿重构 → Review → 提交;体积分级(trivial/small/standard/large)一句话报出,可推翻。
- 人工闸门仅两道:GATE 1 = 批准 task_list 后才写代码;GATE 2 = 确认 diff 与 commit message 后才提交;两闸之间自动推进。
- 硬约束:任务及 ID 仍以 ROADMAP.md 为定义真源;认领/进行中/闭环走 GitHub issues(一任务一 issue 一条流水线,与铁律一致);Review 只是补充视角,不能替代 6 视角对抗审计,CRITICAL/HIGH 清零才进 GATE 2;diff 触及认证/用户输入/SQL/文件路径/外部调用/加密/密钥时自动加 security-reviewer;提交走本文约定的 `feat/<task-id>` 分支。
- 验收不变:三件套(`go test ./...` / `go vet` / `golangci-lint run`)全绿 + 零告警才算完成。

## 命令(有代码后更新此节)

- 测试:`go test ./...`(集成测试需 t13 栈:`docker compose -f deploy/migrate-test.docker-compose.yml -p braid-t13 up -d`,MySQL 13306 / Redis 16379;不在时自动 skip)
- Lint:`golangci-lint run`
- 本地栈:首次 `cp deploy/.env.example deploy/.env`;之后 `docker compose -f deploy/dev.docker-compose.yml up -d`(MySQL 3306 / Redis 队列 6379、缓存 6380 / Asynqmon 8080,均仅绑回环;重置数据 `down -v`)
- 远端/issue:`git push` → draft PR(`gh pr create --draft`,描述带 `Closes #N`);执行队列 `gh issue list`

## 会话交接

- 新会话开始时读 `.handoff/HANDOFF.md`:若 `status: active` 且 mtime 在 72h 内——用户选择继续才一行确认并把 status 改为 `consumed`;用户首条消息与交接无关则不提交接、保持 active、直接做事;未表明做什么则先一行询问「检测到未接手的交接(<updated>),继续它还是本次做别的?」
- 接手后禁止重做「已完成+验证状态」与「已试过且失败的路径」清单中的项。
- `/handoff` 时机:ctx > 50% 且处于任务断点(验证通过/提交完成)时主动建议;任务进行中无断点不主动建议,先推进到断点。
