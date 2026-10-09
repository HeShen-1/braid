# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

三个角色共用同一个 Web 面，都是**桌面浏览器 + 24–27 寸显示器**，办公/机房混合光，单次驻留 20 分钟到数小时，夜间值班是常态。移动端不做（2026-10-09 用户明确）。

- **author（编排者）**：平台/业务工程师。用画布编写 JSON DSL 工作流——节点、连线、字段、分支与并行——保存草稿、发布版本、触发执行。
- **approver（审批人）**：业务/合规角色。处理待办审批队列：看清"这是什么执行、卡在哪个节点、已经等了多久"，然后批准或驳回并留备注。决定经 outbox 续跑被挂起的执行。
- **operator（值班/运维）**：盯执行图与节点状态，定位卡点，执行重试、回滚、取消、续跑。

## Product Purpose

braid 是私有化交付的企业级 Agent 工作流编排平台：JSON DSL 定义 DAG，人在回路审批，全链路审计，工具权限隔离。状态真相源只有 MySQL（每次节点迁移 = CAS `WHERE status=? AND version=?`），Redis 只做队列；审批挂起不占 worker；outbox + relay + reaper 保证 worker 随时被 `kill -9` 也不丢、不重、不悬死。

Web 面的职责：让上面这套机制**在一个产品里被看见和操作**——编排 → 发布 → 触发 → 审批 → 观察 → 回滚/续跑。成功的样子：一个卡住的执行，能在秒级读出"卡在哪一步、卡了多久、下一步该谁动"。

## Positioning

单二进制四入口（api/relay/worker/reaper）+ docker-compose 的私有化交付形态。三个护城河（README §1.2）：工具调用的 SSRF/沙箱隔离、append-only 审计链、合规交付形态。

界面不重复这些能力，而是让它们**可见**：挂在审批上的执行不占资源这件事要有物理长度，重试与回滚要有世系，审计要能顺线索读下来。这是同类"有审批、有审计"的产品抄不走的界面主张。

## Operating Context

- **部署**：私有化、内网、单实例；docker-compose 或 host 四进程。
- **认证**：本地账号 + Bearer token（`sessionStorage`，非 cookie），另有 OIDC/LDAP 企业通道。
- **CSP 硬约束**：`default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`。外部 CDN、内联脚本、内联样式属性一律不可用；唯一允许的放宽是新增 `font-src 'self'`（自托管字体）。
- **前端产物**：Vite 构建直出 `internal/api/web/`，dist 入库、`go:embed` 进单二进制，CI 有 dist 漂移门。
- **界面语言**：全中文。API 资源名保留英文（definition / draft / publish / execution / node / attempt / approval / decision / rollback / trace_id / outbox）。

## Capabilities and Constraints

**已确认能力**（全部经既有 API 面，**零后端新端点**）：登录/登出、草稿列表与新建/保存/发布、画布编辑（xyflow）、DSL 预览、执行触发、执行列表与详情、节点重试/回滚/续跑/取消、审批待办队列与详情、批准/驳回。

**约束**：
1. 不改后端契约——端点、状态机、审计词表一律不动。
2. CSP 不放松，只新增 `font-src 'self'`。
3. 自托管字体必须入库并随 dist 同 commit 提交。
4. 桌面优先，不做移动端布局。
5. 可对外开源展示：README 截图要经得起看，界面不得泄漏内部信息。

## Brand Commitments

产品名 **braid**。三个护城河词是品牌语言：**隔离**、**审计链**、**合规交付**。界面文案用产品自己的语言，不用营销腔。

## Evidence on Hand

- `docs/ARCHITECTURE.md`（18 项已锁决策）、`docs/ROADMAP.md`、`README.md`（架构图与取向表）
- `internal/api/*.go`（真实端点与错误码）、`deploy/` 脚本与 demo 种子数据
- `internal/api/web_test.go`（CSP 词面契约与端点契约测试）

**不存在**：真实客户案例、性能 benchmark 数字、定价信息、第三方 logo。未来任何界面不得编造这些。

## Product Principles

1. **状态先于装饰**：每个像素先回答"现在是什么状态、卡了多久、下一步谁动"。
2. **挂起可见**：braid 的机制特征是"等待不占资源"，界面必须把等待画成有长度的东西，而不是一个转圈。
3. **世系可追**：重试、回滚、续跑、审计要能顺着线索读下来，不能散成孤立字段。
4. **零后端新端点**：视觉与交互重做不得扩张 API 面。
5. **诚实**：没有的能力不画，不确定的状态不猜，演示数据不冒充真实数据。

## Accessibility & Inclusion

- 对比度：正文与占位文本 ≥ 4.5:1，大字与图形 ≥ 3:1。
- **状态色 + 形状双编码**：任何状态不得只靠色相承载（色盲可达）。
- 键盘可达：全部控件可 Tab 到、Enter/Space 触发、Esc 关闭浮层；焦点环始终可见。
- 不做移动端；窄窗口只保证不横向溢出。
