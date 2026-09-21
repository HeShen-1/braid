# 对抗审计归档

本目录存放历次对抗审计的完整发现清单(原始 JSON,含逐条反驳验证记录),供后续任务取材。
仓库内代码/文档只固化**采纳后**的结论;此处 JSON 是唯一保留未采纳发现全文的地方。

## t1.3-mysql-ddl-audit.json

- **对象**:T1.3 MySQL 迁移 v1 六表 DDL(审计时工作副本 `/tmp/braid-t13/final_ddl.sql`;采纳修复后的定稿见 `internal/db/migrations/0001_init.up.sql`,决策注释在文件头)。
- **链路**:3 视角独立设计 → 评审合成 → 6 视角对抗审计(blocking/high 发现经双反驳者独立验证)。
- **处置**(2026-09-21):
  - `confirmed` 8 条(4 blocking + 4 high)——全部修复,落在 `0001_init.up.sql`;
  - `disputed` 4 条(high)——按裁决修复(如 audit 批量写改 `ON DUPLICATE KEY UPDATE id=id` + 行数比对、`heartbeat_at`/`asynq_task_id` NOT NULL);
  - `refuted` 0 条;
  - `medium_low` 39 条(19 medium + 20 low)——**未采纳**,是 T6.1(批量写契约)、T7.3(水位/权限自检)、T7.4(分区运维)等后续任务的素材池;采纳前须逐条复核当时的反驳记录(`facts_verdict`/`stakes_verdict` 字段)。
- 注意:JSON 内 `/tmp/braid-t13/*` 路径为审计当场的临时工作区引用,现已失效;文件仅作证据溯源,不代表当前磁盘状态。
