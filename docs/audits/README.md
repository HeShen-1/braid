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
