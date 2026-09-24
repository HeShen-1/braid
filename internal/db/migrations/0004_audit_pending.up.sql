-- ============================================================================
-- braid 0004_audit_pending.up.sql — M6 T6.1 审计写入器升级(异步批量)的
-- WAL 暂存表:发射方在状态迁移同一事务内 INSERT 暂存行(「staged ⟺ 状态已
-- 迁移」原子性),reaper flush 循环批量搬运至 audit_log 后 DELETE。
--
-- 语义契约(D-2026-09-24-t61-audit-async-batch;GATE 2 修订 = 预过滤 + 裸
-- INSERT,否决 ON DUPLICATE KEY UPDATE——其 UPDATE 权限诉求与 T7.3
-- CheckAuditAppendOnly 硬拒及 INSERT/SELECT-only 部署契约冲突):
--   - 崩溃窗零丢失:暂存行与状态迁移同事务落库 ⇒ 进程崩溃后仍在;flush 的
--     {INSERT, DELETE} 同事务原子 ⇒ 重放残行结构性不可达,预过滤核销为
--     防御臂(批量契约禁 INSERT IGNORE——分区越界 1526 会被静默降级,
--     T1.3 审计确认;裸 INSERT 保持响亮)。
--   - 行身份保持:id = 终行 id(snowflake,发射方生成,flush 原样搬运),
--     崩溃重放不产生第二身份。
--   - event_at = 暂存事务时刻(NOW(3),与状态迁移同钟系):异步批量下
--     「事件时刻 ≠ 落库时刻」分立——落库时刻 = audit_log.created_at(flush
--     期默认),本列是事件时刻的一半,flush 原样搬运。
--   - exec_date 不在本表:flush 期按 DISTINCT execution_id 批量解析
--     DATE_FORMAT(workflow_execution.created_at)(点查摊销,D-t26-F1
--     「批量去重归 T6.1」的兑现面)。
--   - pii_masked 已在发射侧计算(payload 脱敏先于任何落库——「原始永不入
--     payload」含本表;append-only 禁止事后 UPDATE 的 DDL 语义随列搬运)。
--
-- 无 execution_id 外键(镜像 audit_log 同判):FK 父行检查在 RR 下对
-- workflow_execution 取共享记录锁,会把审计写入器刚清零的锁面(D-t26-F1
-- lockfree-read)加回状态迁移热事务;execution 缺行的不可达面(无删除路径)
-- 由 flush 期批量解析的响亮失败兜底。
--
-- 方言与 0001 同规:MySQL 8.0.16+、InnoDB、utf8mb4/utf8mb4_0900_ai_ci、
-- DATETIME(3) UTC(连接契约)、Snowflake 主键。不分区:暂存行生命周期 =
-- flush 间隔(默认 1s),量级与留存均无分区诉求。
-- ============================================================================

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS audit_pending (
  id                BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake(应用生成,与 audit_log.id 同生成器):暂存行身份 = 终行身份,flush 保 id 搬运 ⇒ 崩溃重放幂等',
  event_at          DATETIME(3)     NOT NULL COMMENT '事件时刻 = 暂存事务时刻(NOW(3));flush 原样搬运,不重算——事件时刻≠落库时刻的分立半边',
  event_type        VARCHAR(64)     NOT NULL COMMENT '词表与 audit_log.event_type 同源(audit 包闭集校验);高频四类经本表异步',
  execution_id      BIGINT UNSIGNED NOT NULL COMMENT 'flush 期批量解析 exec_date 的关联键;无 FK(锁面判据见迁移头注释),缺行由 flush 响亮失败兜底',
  node_execution_id BIGINT UNSIGNED NULL COMMENT '同 audit_log 列语义',
  node_id           VARCHAR(64)     NULL COMMENT '同 audit_log 列语义',
  attempt_id        BIGINT UNSIGNED NULL COMMENT '同 audit_log 列语义:批量写对账核销键(attempt 级行必携)',
  actor_type        VARCHAR(16)     NOT NULL COMMENT '同 audit_log 列语义',
  actor_id          VARCHAR(128)    NOT NULL COMMENT '同 audit_log 列语义',
  trace_id          CHAR(32)        CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '同 audit_log 列语义(T6.2 前占位 NULL)',
  linked_trace_id   CHAR(32)        CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '同 audit_log 列语义(T6.2 trace link 占位;建列免 0005——代码路径 T6.2 接线)',
  pii_masked        BOOLEAN         NOT NULL DEFAULT 0 COMMENT '发射侧脱敏钩子已掩蔽标记(先于本表落库计算);flush 原样搬运',
  model             VARCHAR(100)    NULL COMMENT 'usage/cost 冗余列组(llm.call.completed 专属),与 audit_log 同形',
  prompt_tokens     INT UNSIGNED    NULL COMMENT '同 audit_log 列语义',
  completion_tokens INT UNSIGNED    NULL COMMENT '同 audit_log 列语义',
  total_tokens      INT UNSIGNED    NULL COMMENT '同 audit_log 列语义',
  cost_amount       DECIMAL(14,6)   NULL COMMENT '同 audit_log 列语义',
  cost_currency     CHAR(3)         CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '同 audit_log 列语义',
  cost_estimated    BOOLEAN         NOT NULL DEFAULT 0 COMMENT '同 audit_log 列语义',
  payload           JSON            NOT NULL COMMENT '已过脱敏钩子的 payload(密钥永不入内,铁律);无正文事件存 {}',
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '暂存时刻:滞留时长观测面(T6.4 积压信号);flush 恒按 id 排序,本列不进 flush 路径',
  PRIMARY KEY (id) COMMENT 'flush 扫描序(ORDER BY id)= snowflake 时序;行身份与 audit_log 终行一一对应',
  KEY idx_audit_pending_exec (execution_id) COMMENT '按执行观测暂存积压(T6.4);非查询热路径'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='审计异步批量 WAL 暂存表(T6.1):同事务落行、reaper 批量搬运、崩溃重放幂等;非 append-only(DELETE 是搬运半边)';
