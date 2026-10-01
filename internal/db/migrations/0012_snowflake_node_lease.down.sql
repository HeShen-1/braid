-- braid 0012_snowflake_node_lease.down.sql — 0012 回退:单表 DROP。
-- 边界如实登记:down 时若仍有活体组件持有租约,下一次续期即 UPDATE
-- 落空走 lost 路径(停止签发 + 优雅下线,决策 2),不产生孤儿数据——
-- 本表是运行态簿记非业务事实。回退 = 回到「部署层静态分配节点号」世界:
-- compose 显式 env 面不受影响;k8s auto 面失去仲裁源,auto 副本拒启
-- (响亮),先缩容回单副本/显式 env 再 down。

DROP TABLE IF EXISTS snowflake_node_lease;
