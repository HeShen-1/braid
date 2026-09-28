-- ============================================================================
-- braid 0006_parallel_instances.up.sql — T9.1 并行节点(Scatter-Gather,
-- D-2026-09-28-t91-parallel-scatter-gather):node_execution 承载同 node_id
-- 多实例。0001 uk_node_exec 注释的预告在此兑现:「P1 parallel 的 fan-out
-- 多行度由此键扩展,(execution_id,node_id) 语义不变」。
--   1. type ENUM 末尾追加 'parallel'(0001 注释同款预告;末尾追加保序,
--      存量行存储值不受影响)。控制行 type='parallel';实例行落 body 类型
--      (llm|tool),worker 侧零改动(实例行 = 普通节点行)。
--   2. instance_no 列:控制行/单实例节点恒 0(存量语义不变);实例行
--      1..N = foreach 数组输入序(gather results 的排序锚,与完成顺序无关)。
--   3. uk_node_exec 重建为 (execution_id, node_id, instance_no):「每执行
--      每节点恰一行」→「每执行每节点每实例恰一行」。存量行 instance_no=0
--      且无重复 (execution_id,node_id),唯一性对存量完全等价。
--   4. chk_node_instance:BETWEEN 0 AND 1000 镜像应用层 fan-out 常量上限
--      (sched.go MaxParallelInstances;chk_node_max_attempts 镜像 schema
--      上限的同款防御)。
-- 单语句形态(对抗审计 MEDIUM 修):全部子句合入一条 ALTER——multiStatements
--   DSN 下每条 DDL 独立自动提交,拆两条会在第二条失败时留下半应用态(本 up
--   面上不可达,但与 down 对称钉死);失败即整条回滚,re-up 恒可重入。
-- DDL 形态注记:ENUM 末尾追加/带默认值加列/唯一索引重建在 MySQL 8 下均
--   自动选 INSTANT/INPLACE,不锁写;测试栈量级无需 online DDL 仪式。
-- ============================================================================

SET NAMES utf8mb4;

ALTER TABLE node_execution
  MODIFY COLUMN type ENUM('llm','tool','branch','approval','parallel') NOT NULL
    COMMENT '自 DSL 冗余(schema 枚举逐字):状态机分发/按类型超时/T6.4 指标免 JSON 解析;parallel = T9.1 控制行(gather 锚,无 attempt),实例行落 body 类型;词表仅末尾追加',
  ADD COLUMN instance_no SMALLINT UNSIGNED NOT NULL DEFAULT 0
    COMMENT '实例号:T9.1 fan-out。控制行/单实例节点恒 0;实例行 1..N = foreach 输入序(gather results 排序锚);与 uk_node_exec 组成每执行每节点每实例恰一行' AFTER node_id,
  DROP KEY uk_node_exec,
  ADD UNIQUE KEY uk_node_exec (execution_id, node_id, instance_no)
    COMMENT '每执行每节点每实例恰一行(T9.1 扩展,(execution_id,node_id) 语义不变——单实例节点 instance_no 恒 0):upsert 锚、CAS 定位键、T5.5 节点列表;fan-in 汇聚按本索引 node_id 字典序读前驱输出 = ROADMAP「确定性排序」的等价实现(node_id 在钉住 DSL 内唯一不可变,与到达顺序/重试无关;字典序 ≠ DSL 声明序,是有意选择);parallel 控制行 instance_no=0 是 gather 锚与 {{nodes.<id>.output.*}} 引用面,实例行 1..N 不进引用面',
  ADD CONSTRAINT chk_node_instance CHECK (instance_no BETWEEN 0 AND 1000);
