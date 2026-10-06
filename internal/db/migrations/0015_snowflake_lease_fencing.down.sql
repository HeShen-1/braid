-- ============================================================================
-- braid 0015_snowflake_lease_fencing.down.sql — T20.1 回退(D-2026-10-06-t201)。
-- 回退镜像共存窗(0012 down 同判):新二进制的 fence 锁定读显式引用
-- fencing_epoch 列,schema 先降列 = 新二进制一切租约面 SQL 报错拒启;
-- 回退顺序必须 = 先降二进制(回旧版,不写 epoch 不查 fence)再降 schema。
-- 降列即抹代际历史:回滚后重新升级,存量行 epoch 从 DEFAULT 0 重新起代
-- ——旧代际 token 全部失效方向(0 起代 + 首次持有变化 bump),不产生
-- 「旧 token 复活」面;混跑窗语义见 D-2026-10-06-t201 决策 6。
-- ============================================================================

SET NAMES utf8mb4;

ALTER TABLE snowflake_node_lease
  DROP COLUMN fencing_epoch;
