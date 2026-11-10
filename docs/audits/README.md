# 对抗审计归档

本目录存放历次对抗审计的发现清单、验证与后续承接记录(JSON / Markdown,按各记录标明证据级别),供后续任务取材。
仓库内代码/文档只固化**采纳后**的结论;原始审计记录保留未采纳发现及历史验证，后续承接文件不覆盖历史计数。

## t1.3-mysql-ddl-audit.json

- **对象**:T1.3 MySQL 迁移 v1 六表 DDL(审计时工作副本 `/tmp/braid-t13/final_ddl.sql`;采纳修复后的定稿见 `internal/db/migrations/0001_init.up.sql`,决策注释在文件头)。
- **链路**:3 视角独立设计 → 评审合成 → 6 视角对抗审计(blocking/high 发现经双反驳者独立验证)。
- **处置**(2026-09-21):
  - `confirmed` 8 条(4 blocking + 4 high)——全部修复,落在 `0001_init.up.sql`;
  - `disputed` 4 条(high)——按裁决修复(如 audit 批量写改 `ON DUPLICATE KEY UPDATE id=id` + 行数比对、`heartbeat_at`/`asynq_task_id` NOT NULL);
  - `refuted` 0 条;
  - `medium_low` 39 条(19 medium + 20 low)——**未采纳**,是 T6.1(批量写契约)、T7.3(水位/权限自检)、T7.4(分区运维)等后续任务的素材池;采纳前须逐条复核当时的反驳记录(`facts_verdict`/`stakes_verdict` 字段)。
- 注意:JSON 内 `/tmp/braid-t13/*` 路径为审计当场的临时工作区引用,现已失效;文件仅作证据溯源,不代表当前磁盘状态。

## 2026-10 交付复核

- [陌生人旅程原始审计](STRANGER-AUDIT-2026-10-14.md)：56 条历史发现，保留原计数。
- [2026-10-15 交付复核与逐条承接](DELIVERY-RECHECK-2026-10-15.md)：新反例、事实纠正和 ROADMAP M23 映射。
- [T23.1 验证记录](T23.1-VALIDATION.md)：跨轮终态、旧任务认领、真 MySQL 交错及失败历史。
- [T23.2 验证记录](T23.2-VALIDATION.md)：真实 worker/reaper 组件账号授权及动态扇出、耗尽收口。
- [T23.3 验证记录](T23.3-VALIDATION.md)：正式 worker 工具、只读挂载、预算拒绝与最终 PR 准出回执。
- [T23.4 验证记录](T23.4-VALIDATION.md)：min_success 无损往返、Go 发布契约及门禁顺序失误/补验记录。
- [T23.5 验证记录](T23.5-VALIDATION.md)：保存后发布、脏态与迟到响应保护、浏览器旅程及最终 PR 准出回执。
- [T23.6 验证记录](T23.6-VALIDATION.md)：界面选中版本与实际执行版本一致。
- [T23.7 验证记录](T23.7-VALIDATION.md)：编排、分支连线与触发输入交互。
- [T23.8 验证记录](T23.8-VALIDATION.md)：错误、加载与空态区分。
- [T23.9 验证记录](T23.9-VALIDATION.md)：审计游标、节点轮次与执行证据。
- [T23.10 验证记录](T23.10-VALIDATION.md)：版本、角色与审批闭环。
- [T23.11 验证记录](T23.11-VALIDATION.md)：部署和能力文档事实同步。
- [T23.12 验证记录](T23.12-VALIDATION.md)：SSO Web 入口与执行终态事件延期裁决登记。
- [T23.13 验证记录](T23.13-VALIDATION.md)：正式配置独立环境验收。

上述记录的完成证据汇总见[交付复核 §5](DELIVERY-RECHECK-2026-10-15.md#5-后续修复证据)；波次历史见 [M23 开发计划](../plans/M23-NEXT-WAVES-2026-10-15.md)。任务定义/依赖仍以 ROADMAP 为准，实时执行状态只查 GitHub issues。

## 交互与布局验证

- UX-F1：[父票汇总](UX-F1-VALIDATION.md)、[壳层与审批](UX-F1-a-VALIDATION.md)、[画布](UX-F1-b-VALIDATION.md)、[执行总览](UX-F1-c-VALIDATION.md)、[执行列表](UX-F1-d-VALIDATION.md)。
- [UX-F2 画布插入与端点保存](UX-F2-VALIDATION.md)；[UX-F3 节点删除入口](UX-F3-VALIDATION.md)。
- [T24.1 布局与前后端联调](T24.1-VALIDATION.md)，及其[独立收尾评审](T24.1-REVIEW.md)。评审保留当时的发现和证据边界，不代替后续修复与最终验证。
- [T24.2 审批提示与互斥分支汇合](T24.2-VALIDATION.md)，含 [`t24.2/`](t24.2/) 中的浏览器截图。
- [T24.3 审批记录窄屏与画布落位](T24.3-VALIDATION.md)，含 [`t24.3/`](t24.3/) 中的浏览器截图。

## 通知与测试栈验证

- [审批邮件链接与时长](approval-mail/VALIDATION.md)：隔离环境邮件、链接跳转和页面验证，相关截图保存在 [`approval-mail/`](approval-mail/)。
- [测试栈进程清理与前端测试稳定性](test-stack-cleanup/VALIDATION.md)：初次清理修复和相关真实浏览器验证。
- [清理失败与孤儿 relay 防护](test-stack-cleanup/FAILURE-GUARDS-VALIDATION.md)：清理失败时保留数据面、启动前检查及各项验证边界。
