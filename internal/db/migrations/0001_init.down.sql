-- braid 0001_init.down.sql — 0001_init.up.sql 的逆操作。
-- 按 FK 依赖逆序 DROP;audit_log 分区与各表索引随表消亡,无需单独处理。
-- 引擎域 P0 迁移不含存储过程/触发器/事件(逻辑全在应用层),无额外逆操作。
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS node_attempt;
DROP TABLE IF EXISTS node_execution;
DROP TABLE IF EXISTS workflow_execution;
DROP TABLE IF EXISTS workflow_definition;
