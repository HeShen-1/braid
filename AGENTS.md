# braid — 企业级 Agent 工作流编排平台(Go)

**必读顺序**:docs/ARCHITECTURE.md(18 项决策已锁定)→ docs/ROADMAP.md(任务真源)。

## 铁律

- 不得重新争论 ARCHITECTURE.md 已锁定的决策;有新想法 → 在 docs/DECISIONS.md 追加 `D-<date>-<slug>` 条目,经用户确认后才改变实现。
- 任务只从 ROADMAP.md 取;完成即勾选并 commit。一次会话聚焦一个任务。
- 密钥/凭证不进代码、不进日志、不进审计;.env 已忽略。
- `audit_log` 只允许 INSERT;节点状态迁移只经 CAS helper(`WHERE status=? AND version=?`),禁止裸 UPDATE。
- 所有外部调用(LLM/工具/DB)带 attempt 幂等键。

## 约定

- 目录规划:`cmd/`(api | worker | relay | reaper,同一二进制多入口)、`internal/`(engine | nodes | tools | audit | auth | api | config)、`migrations/`、`deploy/`、`docs/`。可开源核心(engine)与企业增值(enterprise)物理分层,不得交叉 import。
- commit:conventional commits,英文主题(feat/fix/docs/test/chore/refactor);代码任务走 `feat/<task-id>` 分支,纯文档可直接进 master。
- 测试:表驱动,`go test ./...` 全绿 + `golangci-lint run` 零告警才算任务完成。
- 文档中文;代码与 commit 英文。

## 命令(有代码后更新此节)

- 测试:`go test ./...`
- Lint:`golangci-lint run`
- 本地栈:首次 `cp deploy/.env.example deploy/.env`;之后 `docker compose -f deploy/dev.docker-compose.yml up -d`(MySQL 3306 / Redis 队列 6379、缓存 6380 / Asynqmon 8080,均仅绑回环;重置数据 `down -v`)

## 会话交接

- 新会话开始时读 `.handoff/HANDOFF.md`:若 `status: active` 且 mtime 在 72h 内——用户选择继续才一行确认并把 status 改为 `consumed`;用户首条消息与交接无关则不提交接、保持 active、直接做事;未表明做什么则先一行询问「检测到未接手的交接(<updated>),继续它还是本次做别的?」
- 接手后禁止重做「已完成+验证状态」与「已试过且失败的路径」清单中的项。
- `/handoff` 时机:ctx > 50% 且处于任务断点(验证通过/提交完成)时主动建议;任务进行中无断点不主动建议,先推进到断点。
