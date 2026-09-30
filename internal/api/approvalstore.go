// approval 持久化原语(T4.2):node_execution 审批面的守卫读(POST 决定
// 事务的锁定读,防校验/落库 TOCTOU)、approval_decision 行的写入/读取、
// 幂等触发的按键读回。与 store.go 同形:连接与事务由调用方持有,原语永不
// 自管事务边界;守卫读起手 FOR UPDATE(写路径纪律)。
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"braid/internal/scheduler"
)

// 审批面哨兵(HTTP 映射见 approval.go mapApprovalError):词面语义与
// store.go 包级 sentinel 同风格。
var (
	// ErrNotApproval:目标节点存在但类型非 approval(决定/详情面对非审批
	// 节点的请求 = 客户端目标错,非不存在)。
	ErrNotApproval = errors.New("api: node is not an approval node")
	// ErrNotWaiting:审批节点已不在可决定态(已被决定/取消等)。
	ErrNotWaiting = errors.New("api: approval not waiting for decision")
	// ErrAlreadyDecided:或签首票已定局,后续票被拒(P0 语义:第一票定局)。
	ErrAlreadyDecided = errors.New("api: approval already decided")
	// ErrDecisionConflict:同一审批人改判(同决定重放除外)。
	ErrDecisionConflict = errors.New("api: conflicting decision by same approver")
	// ErrNotApprover:请求主体不在 DSL approvers[] 内(越权,403 + 审计)。
	ErrNotApprover = errors.New("api: subject not in approvers")
	// ErrExecTerminal:执行已终态(失败兄弟的 fail-fast/取消竞速),决定面
	// 收口(终态执行的迟到决定不产生新语义)。
	ErrExecTerminal = errors.New("api: execution terminal")
)

// ApprovalNode 是审批决定事务的锁定读面(POST 路径守卫读;GET 路径同结构
// 免锁读)。Snapshot 即审批人可读面(engine 不变量:WAITING_APPROVAL ⇒ NOT
// NULL;PENDING 未挂起 ⇒ NULL)。
type ApprovalNode struct {
	NodeExecID uint64
	NodeID     string
	Type       string
	Status     string
	Attempts   uint16
	Snapshot   []byte
	DeadlineAt *time.Time
}

// lockApprovalNode 锁定读审批节点行(FOR UPDATE,决定事务起手;写路径
// 纪律同 store.go:校验与落库之间零 TOCTOU 窗)。行不存在 → ErrNotFound
// (执行缺行与节点缺行同面 404,由调用方先读执行行区分)。
func (s *Store) lockApprovalNode(ctx context.Context, q DBTX, execID uint64, nodeID string, lock bool) (ApprovalNode, error) {
	stmt := `SELECT id, node_id, type, status, attempts, input_snapshot, deadline_at
	  FROM node_execution WHERE execution_id=? AND node_id=?`
	if lock {
		stmt += ` FOR UPDATE`
	}
	var (
		n        ApprovalNode
		deadline sql.NullTime
	)
	err := q.QueryRowContext(ctx, stmt, execID, nodeID).
		Scan(&n.NodeExecID, &n.NodeID, &n.Type, &n.Status, &n.Attempts, &n.Snapshot, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalNode{}, fmt.Errorf("%w: node %q in execution %d", ErrNotFound, nodeID, execID)
	}
	if err != nil {
		return ApprovalNode{}, fmt.Errorf("api: read approval node %q (exec %d): %w", nodeID, execID, err)
	}
	if deadline.Valid {
		t := deadline.Time
		n.DeadlineAt = &t
	}
	return n, nil
}

// Decision 是 approval_decision 一行的模型(0003 DDL 契约;round 列
// T9.2/0007,D-2026-09-28-t92 决策 3③:投票轮次 = 决定时锁行 attempts,
// 与轮次守卫同源单点——回滚重挂新轮新票、T14.2 会签每审批人每轮恰一票)。
type Decision struct {
	ID          uint64
	ExecutionID uint64
	NodeExecID  uint64
	NodeID      string
	Round       int
	DecidedBy   string
	Decision    string // APPROVED | REJECTED(DDL ENUM)
	Comment     *string
	DecidedAt   time.Time
}

// InsertDecisionParams 是决定行的写入参数(ID 应用生成,零值被拒)。
type InsertDecisionParams struct {
	ID          uint64
	ExecutionID uint64
	NodeExecID  uint64
	NodeID      string
	Round       int // 挂起所属轮 = 锁行 attempts(T9.2;0 仅存量行)
	DecidedBy   string
	Decision    string // APPROVED | REJECTED
	Comment     string // 空 → NULL
}

// InsertDecision 落一行决定(调用方决定事务内)。uk_decision_approver 撞
// (同一审批人并发双投恰一胜/重复 POST)→ ErrConflict,由调用方读回裁决
// 重放或改判。业务账号对本表 INSERT-only(append-only 事实表,无 UPDATE 面)。
func (s *Store) InsertDecision(ctx context.Context, q DBTX, p InsertDecisionParams) error {
	switch {
	case p.ID == 0 || p.ExecutionID == 0 || p.NodeExecID == 0:
		return errors.New("api: zero id field(s) in decision insert")
	case p.DecidedBy == "":
		return errors.New("api: empty decided_by in decision insert")
	case p.Decision != scheduler.DecisionApproved && p.Decision != scheduler.DecisionRejected:
		return fmt.Errorf("api: decision %q outside vocabulary", p.Decision)
	}
	var comment any
	if p.Comment != "" {
		comment = p.Comment
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO approval_decision
  (id, execution_id, node_execution_id, node_id, round, decided_by, decision, comment)
VALUES (?,?,?,?,?,?,?,?)`,
		p.ID, p.ExecutionID, p.NodeExecID, p.NodeID, p.Round, p.DecidedBy, p.Decision, comment)
	if err != nil {
		var me *mysql.MySQLError
		// 键名判别(镜像 sched.go Instantiate 的 1062 纪律):只有 uk 撞是
		// 并发双投的结构化收敛面;PRIMARY 撞 = 生成器缺陷(api 副本雪花节点
		// 重号/时钟回拨),映射 ErrConflict 会把它误诊成「同审批人竞速」→
		// 读回 404,重号告警面消失——与普通错误同路上抛。
		if errors.As(err, &me) && me.Number == 1062 && strings.Contains(me.Message, "uk_decision_approver") {
			return fmt.Errorf("%w: decision by %q on node %d: %s",
				ErrConflict, p.DecidedBy, p.NodeExecID, me.Message)
		}
		return fmt.Errorf("api: insert decision %d: %w", p.ID, err)
	}
	return nil
}

// GetDecisionByApprover 读某审批人对某节点某轮的既存决定(uk 命中面;
// 行不存在 → ErrNotFound)。重放/改判裁决的读回。round 维度是 T9.2 下沉点
// (D-t92 决策 6):同轮重放/跨轮新票的判据由谓词承载,消费点不漏改的结构锚。
func (s *Store) GetDecisionByApprover(ctx context.Context, q DBTX, nodeExecID uint64, round int, decidedBy string) (Decision, error) {
	d, err := scanDecision(q.QueryRowContext(ctx, `
SELECT id, execution_id, node_execution_id, node_id, round, decided_by, decision, comment, decided_at
  FROM approval_decision WHERE node_execution_id=? AND round=? AND decided_by=?`, nodeExecID, round, decidedBy))
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, fmt.Errorf("%w: decision by %q on node %d", ErrNotFound, decidedBy, nodeExecID)
	}
	return d, err
}

// ListDecisions 列某审批节点的全部决定(decided_at, id 序 = 投票时间线;
// 跨轮全历史按 Round 分组呈现——回滚重挂后旧轮票是新轮的对照证据,过滤
// 由读面消费方按 Round 承载,本原语不做第二轮次谓词)。
func (s *Store) ListDecisions(ctx context.Context, q DBTX, nodeExecID uint64) ([]Decision, error) {
	rows, err := q.QueryContext(ctx, `
SELECT id, execution_id, node_execution_id, node_id, round, decided_by, decision, comment, decided_at
  FROM approval_decision WHERE node_execution_id=? ORDER BY decided_at, id`, nodeExecID)
	if err != nil {
		return nil, fmt.Errorf("api: list decisions of node %d: %w", nodeExecID, err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	var out []Decision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("api: scan decision: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: iterate decisions: %w", err)
	}
	return out, nil
}

// CountRoundDecisions 数某审批节点某轮 APPROVED/REJECTED 票数(定局判据的
// 数据面,T14.2 会签换装 CountDecisions:D-t42 预告、D-t92 决策 6 钉点的
// 判据改法在此落定——方向维度进判据,round 谓词不变。判据语义单点 =
// scheduler.ApprovalOutcome,本原语只供数,不做裁决)。
func (s *Store) CountRoundDecisions(ctx context.Context, q DBTX, nodeExecID uint64, round int) (approved, rejected int, err error) {
	if err := q.QueryRowContext(ctx, scheduler.ApprovalRoundCountSQL, nodeExecID, round).
		Scan(&approved, &rejected); err != nil {
		return 0, 0, fmt.Errorf("api: count round decisions of node %d: %w", nodeExecID, err)
	}
	return approved, rejected, nil
}

// GetExecutionByIdempotencyKey 按 (tenant, 幂等键) 读回执行(触发重放响应;
// 键唯一 ⇒ 至多一行)。tenant 谓词是纵深冗余(D-t112):键值域已含主体
// 作用域哈希(scopedIdempotencyKey),跨租户撞键结构性不可达——谓词
// 防的是作用域哈希升版/键空间错乱时的静默错回显面。行不存在 →
// ErrNotFound(哈希碰撞之外的不可达:uk 撞必有一行)。
func (s *Store) GetExecutionByIdempotencyKey(ctx context.Context, q DBTX, tenant uint64, key string) (Execution, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+execCols+" FROM workflow_execution WHERE idempotency_key = ? AND tenant_id = ?", key, tenant)
	exec, err := scanExecution(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Execution{}, fmt.Errorf("%w: idempotency key", ErrNotFound)
	}
	return exec, err
}

// decisionScanner 是 scanDecision 的最小扫描面(*sql.Row/*sql.Rows 均满足)。
type decisionScanner interface {
	Scan(dest ...any) error
}

func scanDecision(r decisionScanner) (Decision, error) {
	var (
		d       Decision
		comment sql.NullString
	)
	if err := r.Scan(&d.ID, &d.ExecutionID, &d.NodeExecID, &d.NodeID, &d.Round,
		&d.DecidedBy, &d.Decision, &comment, &d.DecidedAt); err != nil {
		return Decision{}, err
	}
	if comment.Valid {
		c := comment.String
		d.Comment = &c
	}
	return d, nil
}
