-- ============================================================================
-- braid 0008_audit_hash_chain.down.sql — T9.3 回退:拆链锚表 + 还原暂存表。
-- 语义:audit_log 的 prev_hash/row_hash 两列 0001 建表即留,本迁移未触,
-- 回退后 audit_log 原样(已启链行的 hash 值留存,只是不再有校验与执法面);
-- audit_pending 两列随列消亡(在途暂存行的 hash 丢弃 = 回到未启链期形态)。
-- chain_heads 整表消亡:链尾/水位/断链闩状态放弃(重升 0008 后从零重建,
-- 校验器对无 head 的已启链执行行按「无锚」处理——重升后需人工重校)。
-- ============================================================================

SET NAMES utf8mb4;

DROP TABLE IF EXISTS chain_heads;

ALTER TABLE audit_pending
  DROP COLUMN prev_hash,
  DROP COLUMN row_hash;
