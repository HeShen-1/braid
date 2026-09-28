-- braid 0006_parallel_instances.down.sql — 逆序撤销(0001 契约:down 幂等、
-- 可重复)。单条 ALTER(对抗审计 MEDIUM 修):multiStatements DSN 下每条 DDL
-- 独立自动提交,拆多条会在后续子句失败(ENUM 收缩撞 'parallel' 值 / uk 重建
-- 撞实例行重复)时留下「uk 已回退两列但 instance_no/chk/ENUM 残留」的半应用
-- 态,该态下 re-down 与 re-up 双双失败;单条 ALTER 原子成败,失败(= 数据仍
-- 含并行面)整体回滚、re-up 恒可重入——「有并行数据不可逆」以显式失败表达,
-- 不做数据丢弃。
ALTER TABLE node_execution
  DROP KEY uk_node_exec,
  ADD UNIQUE KEY uk_node_exec (execution_id, node_id)
    COMMENT '每执行每节点恰一行:upsert 锚、CAS 定位键、T5.5 节点列表;fan-in 汇聚按本索引 node_id 字典序读前驱输出 = ROADMAP「确定性排序」的等价实现(node_id 在钉住 DSL 内唯一不可变,与到达顺序/重试无关;字典序 ≠ DSL 声明序,是有意选择)。P1 parallel 的 fan-out 多行度由此键扩展,(execution_id,node_id) 语义不变',
  MODIFY COLUMN type ENUM('llm','tool','branch','approval') NOT NULL
    COMMENT '自 DSL 冗余(schema 枚举逐字):状态机分发/按类型超时/T6.4 指标免 JSON 解析;P1 末尾追加 parallel/fan_in',
  DROP CONSTRAINT chk_node_instance,
  DROP COLUMN instance_no;
