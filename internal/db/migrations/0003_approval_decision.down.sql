-- 0003 down:审批决定事实表整表 DROP + 复合 FK 载体键回收(无关联触发器/
-- 视图/分区;execution.resume 事件行(outbox)不 FK 本表,残留事件在 down 后
-- 由 worker 复核守卫响亮失败)。
SET NAMES utf8mb4;
DROP TABLE IF EXISTS approval_decision;
ALTER TABLE node_execution DROP KEY uk_node_id_exec;
