-- braid 0012_snowflake_node_lease.down.sql — 0012 回退:单表 DROP。
-- 边界如实登记:down 时若仍有活体组件持有租约,下一次续期即 UPDATE
-- 落空走 lost 路径(停止签发 + 优雅下线,决策 2),不产生孤儿数据——
-- 本表是运行态簿记非业务事实。回退 = 回到「部署层静态分配节点号」世界:
-- compose 显式 env 面不受影响;k8s auto 面失去仲裁源,auto 副本拒启
-- (响亮),先缩容回单副本/显式 env 再 down。
-- 镜像共存窗(k8s):自 0012 回滚到旧静态号二进制时,旧 pod 的静态号
-- 与新副本租约行互不可见(旧二进制不读本表),滚动共存窗同号双活 =
-- PK 1062——回滚与首次升级同构,先排空旧 pod(UPGRADE.md §2 T15.2
-- 例外条款)。

DROP TABLE IF EXISTS snowflake_node_lease;
