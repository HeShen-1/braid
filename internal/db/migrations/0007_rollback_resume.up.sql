-- ============================================================================
-- braid 0007_rollback_resume.up.sql — M9 T9.2 回滚/断点续跑
-- (D-2026-09-28-t92-rollback-same-branch):同 execution 分支模型。
--   1. node_attempt +input_snapshot/+output 留档列:attempt 行成为旧轮证据
--      四件套(input/output/error_code/usage)全载体——node_execution 行是
--      「最新轮投影」,回滚重跑覆盖后旧轮输入/输出在此留档(「新 attempt 不
--      覆盖旧终态」的输入/输出维度物理载体);只写活行(claim 事务抄输入、
--      收口同语句写输出),零「终态禁改」违反。
--   2. node_execution +attempts_base:回滚新一轮的完整重试预算基线。
--      attempts 绝不清零(uk_attempt / att: TaskID / approval round 全依赖
--      单调),预算判定从 attempts<max_attempts 改为
--      attempts-attempts_base<max_attempts;存量行 base=0 = 全生命周期语义
--      逐字不变。同表:status 列注释与表注释补记 T9.2 重开边(ENUM 词表
--      零追加:D- t92 决策 2a 的修订面在注释与矩阵,不在词表)。
--   3. approval_decision +round,uk 重建 (node_execution_id, round, decided_by):
--      D-2026-09-24-t42 #9-③ 兑现——回滚重挂(同 node_execution 行第二轮
--      投票不再撞 1062)与 T14.2 会签(每审批人每轮恰一票)一次覆盖。
--      round = 决定事务锁行读 node_execution.attempts(挂起所属轮);存量行
--      round=0 恒小于任何真实轮(首轮挂起 attempts≥1),永不污染新轮。
--   4. workflow_execution status 列注释补记 FAILED→RUNNING 回滚复活边
--      (RestartExecution;仅 FAILED 可回滚,白名单决策见 D- t92 决策 4)。
-- 每表恰一条 ALTER(0006:17-19 单语句纪律,multiStatements DSN 下每条 DDL
--   独立自动提交,拆多条 = 半应用态 dirty);失败即整条回滚,re-up 可重入。
-- DDL 形态注记:带默认值加列/唯一索引重建/注释 MODIFY 在 MySQL 8 下均
--   INSTANT/INPLACE,不锁写。
-- ============================================================================

SET NAMES utf8mb4;

ALTER TABLE node_attempt
  ADD COLUMN input_snapshot JSON NULL
    COMMENT '本 attempt 的输入快照留档(T9.2,D-2026-09-28-t92):派发事务内自 node_execution.input_snapshot 抄入(该行此刻恰被本次 claim 写就,同事务一致);NULL=T9.2 前存量行。旧轮输入复现真相源——node_execution 行是最新轮投影,跨轮证据在本列' AFTER asynq_task_id,
  ADD COLUMN output JSON NULL
    COMMENT '本 attempt 的输出留档(T9.2):收口事务同语句写入(SUCCEEDED 携带节点 output;FAILED/DEAD 恒 NULL——未产出)。旧轮输出复现真相源,与 node_execution.output(最新成功轮投影)分立' AFTER input_snapshot;

ALTER TABLE node_execution
  ADD COLUMN attempts_base SMALLINT UNSIGNED NOT NULL DEFAULT 0
    COMMENT '本轮重试预算基线(T9.2):ResetNodeToPending/RerunNode 同语句写为当轮 attempts;预算判定 = attempts-attempts_base<max_attempts(旧轮不吃新预算);0=T9.2 前存量行,全生命周期语义逐字不变' AFTER attempts,
  MODIFY COLUMN status ENUM('PENDING','EXECUTING','WAITING_APPROVAL','SUCCEEDED','FAILED','SKIPPED','CANCELLED') NOT NULL DEFAULT 'PENDING'
    COMMENT '全集见表注释;迁移仅末尾追加',
  COMMENT='节点执行:状态机真相源,CAS 迁移,输入快照,attempt 计数,统一截止时刻。T9.2 重开边(D-2026-09-28-t92):SUCCEEDED|FAILED|SKIPPED|CANCELLED→PENDING 仅限 ResetNodeToPending(显式回滚,api 同步事务,同事务审计)——终态对自动路径(reaper/平台重试)恒无出边;FAILED→RUNNING 见 workflow_execution(执行复活)';

ALTER TABLE approval_decision
  ADD COLUMN round INT UNSIGNED NOT NULL DEFAULT 0
    COMMENT '投票轮次(T9.2,D-t42 #9-③ 兑现)= 决定事务锁行读 node_execution.attempts(产生本次挂起的 claim 序号;首轮挂起恒 ≥1):回滚重挂新轮新票、T14.2 会签每审批人每轮恰一票;0=T9.2 前存量行,恒小于任何真实轮,永不污染新轮' AFTER node_execution_id,
  DROP KEY uk_decision_approver,
  ADD UNIQUE KEY uk_decision_approver (node_execution_id, round, decided_by)
    COMMENT '每审批人每节点每轮恰一票:并发双投恰一胜、改判被 DB 拒(409 归应用层);round 维度 = 回滚重挂与 T14.2 会签的公共扩列(D-t42 #9-③「两半」)';

ALTER TABLE workflow_execution
  MODIFY COLUMN status ENUM('PENDING','RUNNING','WAITING_APPROVAL','SUCCEEDED','FAILED','CANCELLED') NOT NULL DEFAULT 'PENDING'
    COMMENT 'PENDING=已建未跑(覆盖 §3.1 QUEUED 语义)→ RUNNING →(WAITING_APPROVAL⇄RUNNING)*→ SUCCEEDED|FAILED|CANCELLED;PENDING 可直接→FAILED(reconciler 判死);FAILED→RUNNING 仅限显式回滚 RestartExecution(T9.2,D-2026-09-28-t92:同语句清 outputs/error_code/error_message/finished_at,终态列组仅终态携带、先前终局时刻在审计链);终态=SUCCEEDED/FAILED/CANCELLED(终态对自动路径恒无出边,回滚复活是唯一例外通道且仅 FAILED);迁移:仅末尾追加';
