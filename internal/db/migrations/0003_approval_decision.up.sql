-- ============================================================================
-- braid 0003_approval_decision.up.sql — M4 T4.2 审批决定持久化:审批 API 的
-- 事实表(ARCHITECTURE §3.3「审批 API 持久化结果」)。一行 = 一名审批人对一个
-- 审批节点的一次投票;append-only 事实表(无 UPDATE 面,改判由 uk 拒绝而非改行)。
--
-- 语义契约:
--   - 或签(P0 唯一 mode):第一票定局(APPROVED/REJECTED 任一方向);
--     「已有人投票」由应用层守卫(事务内锁 node_execution 行后查本表),
--     uk_decision_approver 兜底同一审批人的并发双投恰一胜。
--   - decided_by = JWT sub(T5.1;字符串主体,与 triggered_by/audit.actor_id 同域,
--     非 FK——Q13 IdentityProvider 换源兼容,镜像 workflow_definition.created_by)。
--   - comment 是审批人业务备注(T4.4 审批页输入),非审计 payload 自由文本面
--     (D-t26 §4 纪律管 audit payload,本表是业务事实存储)。
--   - 决定的下游消费 = outbox execution.resume 事件(T4.2);本表行经事件
--     payload.decision_id 被引用,worker 侧续跑事务复核(防伪事件)。
--
-- TaskID 词表扩展(契约记录:0001 §5 表注释的 resume: 形态由本迁移定稿扩展,
-- 0001 已应用不可改):审批续跑事件 task_id = resume:<execution_id>:<node_id>:<attempt_no>
-- (attempt_no = 本次续跑将创建的 attempt 号,node 行锁内读出,确定性)。恢复
-- append.go 词表不变量「同一 task_id 永不表达需要再投一次的新意图」:同一续跑
-- 意图的对账重投复用同 ID(ErrTaskIDConflict 幂等收敛);新续跑轮次(P1 回滚
-- 重挂→再挂起)携带新 attempt ⇒ 新 ID,asynq archived 滞留副本不再挡死。
-- 决策记录:D-2026-09-24-t42-approval-resume。
--
-- 方言与 0001 同规:MySQL 8.0.16+、InnoDB、utf8mb4/utf8mb4_0900_ai_ci、
-- DATETIME(3) UTC(连接契约)、Snowflake 主键(续跑事件引用 decision id 于
-- 事务提交前可知)。
-- ============================================================================

SET NAMES utf8mb4;

-- 复合 FK 载体(先于引用表建立):node_execution 的 (id, execution_id)
-- 唯一键使 approval_decision 的复合外键把「execution_id 必与
-- node_execution_id 同链」钉死为 DB 不变量(对抗审计 LOW 修:两条独立
-- 单列 FK 各自只证存在、不证配对,跨链污染行可静默入库)。id 已是 PK,
-- 本键纯为约束载体,零查询语义变化。
ALTER TABLE node_execution
  ADD UNIQUE KEY uk_node_id_exec (id, execution_id);

CREATE TABLE IF NOT EXISTS approval_decision (
  id               BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake,应用生成(outbox 事件 payload 引用于提交前可知)',
  execution_id     BIGINT UNSIGNED NOT NULL COMMENT '所属执行(冗余 node_execution.execution_id:按执行列审批时间线免 join);与 node_execution_id 的同链配对由复合 FK 钉死',
  node_execution_id BIGINT UNSIGNED NOT NULL COMMENT '目标审批节点行(uk_node_exec 定位);复合 FK 连同 execution_id 一起钉死归属链',
  node_id          VARCHAR(64)     NOT NULL COMMENT 'DSL 节点 id 冗余(行自描述,镜像 audit_log.node_id);与 node_execution.node_id 同值',
  decided_by       VARCHAR(128)    NOT NULL COMMENT '审批人主体(JWT sub;与 triggered_by/audit.actor_id 同字符串域,非 FK——Q13 换源兼容)',
  decision         ENUM('APPROVED','REJECTED') NOT NULL COMMENT '投票方向;或签下首票定局。ENUM 仅末尾追加(DDL 演进纪律)',
  comment          VARCHAR(2000)   NULL COMMENT '审批人备注(T4.4 审批页);NULL=未填写;进入 node output 的 comment 字段(声明可引用)',
  decided_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '投票落库时刻(库端默认=写入路径零遗漏);append-only 表无 updated_at',
  PRIMARY KEY (id),
  UNIQUE KEY uk_decision_approver (node_execution_id, decided_by) COMMENT '每审批人对每节点恰一票:并发双投恰一胜、改判被 DB 拒(409 归应用层)',
  KEY idx_decision_exec (execution_id, decided_at) COMMENT '按执行列审批时间线(T4.4 详情页/审计对账);snowflake 时序近似 decided_at',
  CONSTRAINT fk_decision_node FOREIGN KEY (node_execution_id, execution_id)
    REFERENCES node_execution (id, execution_id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT fk_decision_exec FOREIGN KEY (execution_id) REFERENCES workflow_execution (id) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='审批决定事实表(T4.2):或签首票定局,append-only,uk 拒改判;消费面=execution.resume 事件';
