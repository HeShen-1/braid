# 2026-10-15 交付复核与承接

基线:`7eefd84ce45a1bb938dce4511edd3657968e8d34`。本轮复核覆盖路线图/队列、scheduler/worker 竞态、组件权限、部署配置、画布往返与发布/触发链。该提交 CI 七项成功不等于这些未覆盖情形通过。当前结果包含代码确认与隔离 SQL driver 反例，尚无真 MySQL 并发、真实组件账号完整旅程或真实模型验收证明。

## 1. 新增/复核问题

| 问题 | 机制与影响 | 证据级别 | 承接 |
|---|---|---|---|
| R1 | 旧轮终态快照可误收口已回滚的新轮 | 原函数 + SQL driver interleaving counterexample；非真 MySQL | T23.1 |
| R2 | claimAttempt 快照读取与 attempt 插入不原子 | 取消和回滚两分支 SQL driver counterexamples；非真 MySQL | T23.1 |
| R3 | 生产组件授权缺少运行时必需 INSERT | 静态读写权限对照，未用真实组件账号执行 | T23.2 |
| R4 | 生产 worker 缺少工具与预算环境变量透传 | compose/chart worker 环境声明与 cmd/worker 读取路径对照 | T23.3 |
| R5 | 画布保存静默丢失 parallel.min_success | 直接运行当前 toCanvas/fromCanvas/serializeDoc | T23.4 |
| R6 | 发布没有保存当前编辑，随后关闭编辑器 | 当前调用链静态确认；未运行浏览器写入 | T23.5 |
| R7 | 界面显示版本但触发请求未携带 version | 拦截 fetch 验证请求体，零网络 | T23.6 |
| R8 | 审计 UI 丢弃 next_cursor，最多显示首 100 条 | 当前源码静态确认，属于旧审计 F13 的现存问题 | T23.9 |

隔离反例原始观察:

```text
workflow=SUCCEEDED node=EXECUTING output={"result":"old-round"}
current_node=CANCELLED attempts_base=0 inserted_attempt=1
current_node=EXECUTING attempts_base=1 inserted_attempt=1
parallel.min_success: 1 -> 字段缺失
trigger request: {definition_name,input}，不含 version
```

SQL driver 只控制交错点，不替代 InnoDB 行锁/隔离级别证明。T23.1 必须以真 MySQL 屏障测试将反例转成回归，且保留正常路径与相反锁获取次序的对照。

## 2. 事实纠正

- #229 的“没有 aria-label/title”不成立：`web/canvas/src/NodePanel.tsx:91-92` 自 c6bcab63 就有两属性；用户找不到入口的体感保留，任务改为视觉发现性与可完成删除旅程。
- #227 参照系已拍板；正文由等待选型改为已有四子项及前置依赖。sparkline 只有外观方向，须先定指标/窗口/数据来源。
- 原审计 56 条原始计数保留；F30 已由 #224 处理，F42 等待时长已由 #223 处理，剩余上下文仍有承接。
- 升级手册当前迁移由 0005 纠正为 0015；旧交接不是当前执行队列，阶段收口时同步。

## 3. 原审计 56 条逐项承接

此表只记录来源与承接，不创建第二套执行状态；实时认领/关闭只查 GitHub。

| 承接任务 | 原审计发现 |
|---|---|
| T22.1 | F05 |
| T22.2 | F03 |
| T22.3 | F04 |
| T22.4 | F10 |
| T22.5 | F02 |
| #224/T22残件 | F30 |
| T23.5 | F26 |
| T23.7 | F34 / F38 / F43 / F32 / F37 / F54 / F63 / F65 / F69 |
| T23.8 | F21 / F22 / F23 / F24 / F28 / F39 / F41 / F44 / F49 / F50 / F53 |
| T23.9 | F13 / F16 / F17 / F18 / F19 / F45 / F68 / F71 |
| T23.10 | F14 / F20 / F25 / F29 / F40 / F42 / F55 / F56 / F57 |
| T23.11 | F01 / F12 / F47 / F48 / F60 / F61 |
| T23.12 | F15 / F66 |
| UX-F1-a | F08 / F27 |
| #228 | F36 / F64 |

## 4. 已批准的目标与验收口径

完整可演示旅程优先；保存当前画布后发布，保存失败停止；运行界面选中版本；沿用文档审查并覆盖批准、合规不过、人工驳回、失败恢复、回滚重跑；使用正式配置与组件账号、独立测试环境。保留既定视觉方向与 API/安全不变量。工程桩与真实模型分别举证，真实调用预算执行前确定。

## 5. 后续修复证据

T23.1 的真实 MySQL 交错、修复边界与门禁独立记录于 [T23.1-VALIDATION.md](T23.1-VALIDATION.md)。原复核时“尚未做真 MySQL”的状态是历史基线，不能抹去，也不代表修复后未补验证。

2026-10-15 追加：T23.1–T23.5 已分别合并，以下入口记录对应修复证据；前文反例、原始审计计数与证据级别保持原样。此表为交付快照，实时状态仍以 GitHub issues / PR 为准。

| 原问题 / 承接 | 修复与验证范围 | 合并入口 |
| --- | --- | --- |
| R1/R2 → T23.1 / #230 | [真 MySQL 确定性交错](T23.1-VALIDATION.md)：旧轮终态收口、认领与 attempt 插入原子性，保留正常路径与失败历史。 | [PR #247](https://github.com/HeShen-1/braid/pull/247) |
| R3 → T23.2 / #231 | [真实组件账号验证](T23.2-VALIDATION.md)：worker 动态扇出及 reaper 耗尽收口，保持 audit_log append-only。 | [PR #249](https://github.com/HeShen-1/braid/pull/249) |
| R4 → T23.3 / #232 | [正式 Compose 专项旅程](T23.3-VALIDATION.md)：file_read/HTTPS 成功、路径/SSRF 拒绝、预算调用前拒绝、只读挂载及 Helm 静态交付门。 | [PR #251](https://github.com/HeShen-1/braid/pull/251) |
| R5 → T23.4 / #233 | [画布往返与发布契约](T23.4-VALIDATION.md)：min_success 启用/缺席保真、前端测试及 dist；门禁顺序失误与合并后补验原样保留。 | [PR #248](https://github.com/HeShen-1/braid/pull/248) |
| R6/F26 → T23.5 / #234 | [保存后发布与脏态保护](T23.5-VALIDATION.md)：220 例 vitest、5/5 本机浏览器旅程、真栈三件套、迟到发布错误不污染后续编辑。 | [PR #250](https://github.com/HeShen-1/braid/pull/250) |

T23.3/T23.5 的最终文档 head、七项 CI、独立审查和 merge commit 已追加到各自验证报告。T23.4 的补验完成不改写提前合并的事实，也不能统称为“五项任务全部在门禁通过后合并”。

R7 的界面钉版触发仍由 T23.6 / #235 验收，R8 的完整审计/轮次消费仍由 T23.9 / #238 验收；正式配置完整业务旅程仍归 T23.13 / #242。现有组件/工程桩与局部浏览器证据，不代表真实模型或整条业务旅程已通过。[后续开发计划](../plans/M23-NEXT-WAVES-2026-10-15.md)只安排波次，不替代 ROADMAP 与 issue 状态。
