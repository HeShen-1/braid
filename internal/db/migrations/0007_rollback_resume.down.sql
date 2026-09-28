-- braid 0007_rollback_resume.down.sql — 逆序撤销(0001 契约:down 幂等、可重复)。
-- 每表恰一条 ALTER(0006 down 同款纪律):失败即整条回滚,re-down 恒可重入。
-- down 前置:approval_decision 含 round>0 行(回滚重挂已发生)时 uk 收缩撞
-- 重复 → 显式失败表达「有回滚轮数据不可逆」,不做数据丢弃(镜像 0006 down)。

ALTER TABLE workflow_execution
  MODIFY COLUMN status ENUM('PENDING','RUNNING','WAITING_APPROVAL','SUCCEEDED','FAILED','CANCELLED') NOT NULL DEFAULT 'PENDING'
    COMMENT 'PENDING=已建未跑(覆盖 §3.1 QUEUED 语义)→ RUNNING →(WAITING_APPROVAL⇄RUNNING)*→ SUCCEEDED|FAILED|CANCELLED;PENDING 可直接→FAILED(reconciler 判死);终态=SUCCEEDED/FAILED/CANCELLED;迁移:仅末尾追加';

ALTER TABLE approval_decision
  DROP KEY uk_decision_approver,
  ADD UNIQUE KEY uk_decision_approver (node_execution_id, decided_by)
    COMMENT '每审批人对每节点恰一票:并发双投恰一胜、改判被 DB 拒(409 归应用层)',
  DROP COLUMN round;

ALTER TABLE node_execution
  MODIFY COLUMN status ENUM('PENDING','EXECUTING','WAITING_APPROVAL','SUCCEEDED','FAILED','SKIPPED','CANCELLED') NOT NULL DEFAULT 'PENDING'
    COMMENT '全集见表注释;迁移仅末尾追加',
  COMMENT='节点执行:状态机真相源,CAS 迁移,输入快照,attempt 计数,统一截止时刻',
  DROP COLUMN attempts_base;

ALTER TABLE node_attempt
  DROP COLUMN output,
  DROP COLUMN input_snapshot;
