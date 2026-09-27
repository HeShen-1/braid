// Package scheduler 实现 T2.4 DAG 调度器:入度推进、条件求值路由(T3.4 CondRouter)、执行完成回写,
// 以及执行起点(exec: 事件 → 入口派发)与 DSL 物化(Instantiate)。
//
// 引擎消费链上的位置(worker 包注释的「派发方」):
//
//	Instantiate:definition(dsl.Parse)→ execution(PENDING)+ 全部 node 行 +
//	    outbox(execution.start, task_id=exec:<id>)
//	→ relay 投递 → worker mux(execution.start)→ Start:PENDING→RUNNING +
//	    入口节点派发(ClaimNode + outbox att:<id>:<attempts>)
//	→ relay 投递 → worker 执行 attempt(T2.3)→ Complete(本包):CloseAttempt +
//	    节点收口(Succeed/Redispatch/Fail)+ 入度推进 + 执行终态回写
//
// 调度语义(DECISIONS D-2026-09-22-t24-scheduler-semantics,用户已确认):
//   - 派发条件 = 全部前驱 SUCCEEDED;任一前驱 SKIPPED ⇒ 本节点 SKIPPED(递归
//     传播)。推论:P0 的 branch 臂不可汇合(汇合节点恒被剪枝)。
//   - fail-fast:任一节点终败(attempts 耗尽/求值失败/路由失败)⇒ 立即
//     FinishExecution(FAILED);在飞兄弟自然跑完但不再推进。
//   - branch 内联求值:路由在 claim 之前(错误不留 deadline=NULL 的不可扫描
//     EXECUTING 行),ClaimNode(operands 快照)→ SucceedNode(output=NULL)
//     同事务,无 attempt 行;取路目标继续推进、未取直接后继剪枝。
//   - 工作流 outputs 引用被剪枝臂 → null(键保留)。
//
// 并发(防幻读与防死锁):
//   - Complete 事务第一条语句序列 = 按 node_id 升序逐行 FOR UPDATE 锁全执行
//     行集(当前读,不受 RR 快照污染)——并发完成兄弟前驱时在该序列上完全
//     串行,「数前驱」不可能读到过期快照。锁集全量相同 ⇒ 无部分交叠死锁;
//     残余交错(与 heartbeat 的 attempt→wf 序)由 engine.Transient(1213/1205)
//     内部重试兜住(≤3 次,只重试回写、不重跑 executor、不烧 asynq 重试预算)。
//   - 锁序 = node 行(node_id 升序)→ attempt → workflow_execution(最后),
//     与 T2.3 心跳(attempt→wf)一致;Start 例外先拿 wf(安全性即此备忘:
//     PENDING 执行不可能存在 claim/attempt,无并发 Complete)。
//   - 终态判定在提交后的新事务(新鲜快照):不相交臂的并发完成各自事务内
//     快照都看到对方在飞 ⇒ 都不判终态(事务内判定会漏判);提交后重查者必
//     见全部已提交 ⇒ 不漏判。双收口由 FinishExecution 的 status-only CAS 仲裁。
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/engine/dsl"
	"braid/internal/outbox"
	"braid/internal/snowflake"
)

// 事件词表:event_type 即 asynq task type(relay 直投约定)。
// node.task_enqueue 与 worker 侧 T2.3 定名逐字一致;execution.start 是本包新增
// (TaskID 词表 exec:<id>,outbox.append 校验通过)。execution.resume 是 T4.2
// 审批续跑事件(DDL §5 开放词表注释预留同名;TaskID 词表 resume:<exec>:<node>:<attempt>,
// 0003 头注释定稿)——发射点在 api 审批面(决策事务),消费面在 worker
// handleNodeResume → Scheduler.Resume。approval.notify(T4.3 起有发射点 = reaper
// 审批超时升级事务;TaskID notify:<node_exec_id>,T4.5 起消费面 = worker
// handleApprovalNotify → notify.Sender SMTP 发送;D-t41 #3 移交闭环)。
const (
	EventTypeNodeTask        = "node.task_enqueue"
	EventTypeExecutionStart  = "execution.start"
	EventTypeExecutionResume = "execution.resume"
	EventTypeApprovalNotify  = "approval.notify"
)

// NodeTaskPayload 是 node.task_enqueue 事件的自描述体(outbox 契约:仅 id 引用)。
// 与 worker 消费面共用(worker 包 import 本包,无第二定义)。
type NodeTaskPayload struct {
	ExecutionID     uint64 `json:"execution_id"`
	NodeExecutionID uint64 `json:"node_execution_id"`
	AttemptNo       uint16 `json:"attempt_no"`
	// TraceID 随事件交给 worker 续 OTel 链(T6.2 跨进程传播:payload 自描述
	// 携带,relay 直投不感知;worker middleware extract 注入 ctx 续 span)。
	// omitempty:无 trace 上下文的发射面(测试路径)键缺席 = 消费侧零注入。
	TraceID string `json:"trace_id,omitempty"`
}

// ExecStartPayload 是 execution.start 事件的自描述体。
type ExecStartPayload struct {
	ExecutionID uint64 `json:"execution_id"`
	// TraceID = 触发请求 root trace(InstantiateParams.TraceID 直传,
	// workflow_execution.trace_id 同值单源;T6.2)。
	TraceID string `json:"trace_id,omitempty"`
}

// ResumePayload 是 execution.resume 事件的自描述体(outbox 契约:仅 id 引用;
// 决定全文真相源在 approval_decision 行,worker 侧续跑事务按 DecisionID 复核)。
// AttemptNo 是本次续跑将创建的 attempt 号(= TaskID 第四段,自描述对账用;
// 权威值由 node 行锁内读出,事件重放不因它分叉)。
type ResumePayload struct {
	ExecutionID     uint64 `json:"execution_id"`
	NodeExecutionID uint64 `json:"node_execution_id"`
	DecisionID      uint64 `json:"decision_id"`
	AttemptNo       uint16 `json:"attempt_no"`
	// TraceID = 决策请求 trace(段首新链 Z;挂起前主链 X 经决策行
	// linked_trace_id 串联——T6.2 trace link;reaper 死信网重投拷原行值)。
	TraceID string `json:"trace_id,omitempty"`
}

// ApprovalNotifyPayload 是 approval.notify 事件的自描述体(T4.3 起有发射点:
// reaper 审批超时升级事务;T4.5 起消费面 = worker handleApprovalNotify →
// notify.Sender SMTP 发送,未配置 SMTP 时降级 log-only ack)。
// TaskID = notify:<node_execution_id>(恰好一次锚在 escalated_at)。Reason 是
// 成因码(词表单源见 ReasonApproval* 常量,后续成因在此扩词);payload 仅
// id 引用:审批人/策略/截止真相源在 node_execution 行,消费方回表。
type ApprovalNotifyPayload struct {
	ExecutionID     uint64 `json:"execution_id"`
	NodeExecutionID uint64 `json:"node_execution_id"`
	Reason          string `json:"reason"`
	// TraceID:T6.2 边界——notify 链 P0 无段 trace(发射面 reaper tick 无
	// span 上下文,approval.notified 行 trace NULL),D-t62 登记;P1 若
	// 回读 root 续链在此扩。
	TraceID string `json:"trace_id,omitempty"`
}

// approval.notify 的成因码词表(发射点 reaper 与消费面 worker 词表闸同源,
// 单一定义防两方漂移——review LOW④)。P0 唯一词;扩词 = 追加常量 +
// reaper 发射分支 + worker 词表闸同 PR 旅行。
const (
	// ReasonApprovalEscalated:审批超时升级(Q9 escalate 策略,恰好一次锚
	// escalated_at)。
	ReasonApprovalEscalated = "escalated"
)

// MetricsSink 是调度器的指标缝(T6.4):由 cmd 装配注入(生产 = metrics 适配
// 器,测试默认 nil = 观测关闭)。窄面:本包唯一发射点 = Complete 让位丢弃。
type MetricsSink interface {
	IncLLMCallDiscarded()
}

// Deps 是运行依赖。Router 为 nil 用 CondRouter;Queue 为空用 "default"(必须在
// worker ServeQueues 内,否则事件永挂 pending——outbox 包注释的已知边界);
// Logger 为 nil 用 slog.Default();Metrics nil = 观测关闭。
type Deps struct {
	DB      *sql.DB
	IDs     *snowflake.Generator // execution/node/outbox 行 id(应用侧 Snowflake)
	Router  Router
	Queue   string
	Logger  *slog.Logger
	Metrics MetricsSink
}

// Scheduler 是调度器实例(无内部状态,字段在构造时定格)。
type Scheduler struct {
	deps   Deps
	router Router
	queue  string
	log    *slog.Logger
}

// New 构造调度器;依赖缺损响亮报错(不静默空转)。
func New(d Deps) (*Scheduler, error) {
	if d.DB == nil || d.IDs == nil {
		return nil, errors.New("scheduler: nil DB or IDs")
	}
	router := d.Router
	if router == nil {
		router = CondRouter
	}
	queue := d.Queue
	if queue == "" {
		queue = "default"
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{deps: d, router: router, queue: queue, log: log}, nil
}

// NodeTimeout 返回 claim 时的 deadline 偏移(DDL:进入 EXECUTING 时 = now+按类型
// 执行超时;T7.1 配置化的换装点)。branch 内联无驻留 → 零值(NULL deadline)。
// 导出供 reaper 重挂/重派计算同一词表(单源,D-2026-09-22-t25-reaper-semantics)。
var nodeTimeouts = map[string]time.Duration{
	"llm":      30 * time.Minute,
	"tool":     10 * time.Minute,
	"approval": 72 * time.Hour,
	"branch":   0,
}

func NodeTimeout(nodeType string) time.Duration {
	if d, ok := nodeTimeouts[nodeType]; ok {
		return d
	}
	return 10 * time.Minute
}

// ---- 定义与执行行的只读取数(事务外;定义行不可变,执行 input 不可变) ----

type definitionRow struct {
	name    string
	version sql.NullInt64
	dsl     []byte
	hash    string
}

func (s *Scheduler) loadDefinition(ctx context.Context, id uint64) (*definitionRow, error) {
	var r definitionRow
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT name, version, dsl, dsl_hash FROM workflow_definition WHERE id=?`, id).
		Scan(&r.name, &r.version, &r.dsl, &r.hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow_definition %d", engine.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("scheduler: read definition %d: %w", id, err)
	}
	if !r.version.Valid {
		return nil, fmt.Errorf("scheduler: definition %d has NULL version (unpublished draft)", id)
	}
	return &r, nil
}

// loadPublishedDefinition 是物化路径的定义读取:DDL 生命周期契约
// DRAFT→PUBLISHED→ARCHIVED,ARCHIVED = 禁新触发(运行中实例不受影响)——
// Instantiate 必须只接受 PUBLISHED。
func (s *Scheduler) loadPublishedDefinition(ctx context.Context, id uint64) (*definitionRow, error) {
	var r definitionRow
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT name, version, dsl, dsl_hash FROM workflow_definition WHERE id=? AND status='PUBLISHED'`, id).
		Scan(&r.name, &r.version, &r.dsl, &r.hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow_definition %d (missing or not PUBLISHED)", engine.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("scheduler: read definition %d: %w", id, err)
	}
	if !r.version.Valid {
		return nil, fmt.Errorf("scheduler: definition %d has NULL version (unpublished draft)", id)
	}
	return &r, nil
}

type executionRow struct {
	definitionID uint64
	input        json.RawMessage
}

func (s *Scheduler) loadExecution(ctx context.Context, id uint64) (*executionRow, error) {
	var r executionRow
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT definition_id, input FROM workflow_execution WHERE id=?`, id).
		Scan(&r.definitionID, &r.input)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow_execution %d", engine.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("scheduler: read execution %d: %w", id, err)
	}
	return &r, nil
}

// NodeDefinition 是执行器的定义读取缝(T3.6 nodes 包消费):node_execution →
// 钉住定义 → 解析后的 DSL 节点。outputs 声明等执行期语义不在 input_snapshot
// 里(快照只含求值后的数据面),执行器经本缝回 DSL 取,定义单源。
// 定义不可变(钉版契约)⇒ 事务外只读无 TOCTOU;每 attempt 调用一次、不设
// 缓存(定义 KB 级、Parse 微秒级、attempt 级频次,缓存一致性复杂度不值)。
func (s *Scheduler) NodeDefinition(ctx context.Context, nodeExecID uint64) (*dsl.Node, error) {
	var nodeID string
	var execID uint64
	err := s.deps.DB.QueryRowContext(ctx,
		`SELECT node_id, execution_id FROM node_execution WHERE id=?`, nodeExecID).
		Scan(&nodeID, &execID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: node_execution %d", engine.ErrNotFound, nodeExecID)
	}
	if err != nil {
		return nil, fmt.Errorf("scheduler: read node_execution %d: %w", nodeExecID, err)
	}
	exec, err := s.loadExecution(ctx, execID)
	if err != nil {
		return nil, err
	}
	def, err := s.loadDefinition(ctx, exec.definitionID)
	if err != nil {
		return nil, err
	}
	wf, _, err := workflowFromDef(exec.definitionID, def)
	if err != nil {
		return nil, err
	}
	for i := range wf.Nodes {
		if wf.Nodes[i].ID == nodeID {
			return &wf.Nodes[i], nil
		}
	}
	// 行由钉住 DSL 物化而来,节点必然在文档内;到达即数据缺陷。
	return nil, fmt.Errorf("scheduler: node %q not in pinned definition %d", nodeID, exec.definitionID)
}

// loadWorkflow 读定义并解析为拓扑视图。DSL 落库前已经 T1.1/T1.2 双层校验
// (发布闸),此处 Parse 失败 = 数据缺陷,响亮上抛。Start/Complete 用本形态
// (状态不设限:定义钉住于 Instantiate,后续 ARCHIVED 不影响运行中实例——
// DDL 生命周期契约)。
func (s *Scheduler) loadWorkflow(ctx context.Context, definitionID uint64) (*dsl.Workflow, *dag, error) {
	def, err := s.loadDefinition(ctx, definitionID)
	if err != nil {
		return nil, nil, err
	}
	return workflowFromDef(definitionID, def)
}

// workflowFromDef 从已读定义行解析拓扑(Instantiate 复用同一行,免二次读)。
func workflowFromDef(definitionID uint64, def *definitionRow) (*dsl.Workflow, *dag, error) {
	wf, err := dsl.Parse(def.dsl)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: definition %d dsl: %w", definitionID, err)
	}
	g, err := buildDAG(wf)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: definition %d: %w", definitionID, err)
	}
	return wf, g, nil
}

// InstantiateParams 是物化参数。
type InstantiateParams struct {
	DefinitionID uint64
	Input        json.RawMessage // 零输入 → {}
	TriggeredBy  string          // 必填(审计事实源,T2.6)
	TriggerType  string          // 空 → "API"(DDL ENUM 词表)
	// TraceID = 触发请求 root trace(T6.2 起必填,api 触发路径应用层强制;
	// 空串 → NULL,内部测试路径允许——0001:76 注释口径)。非空必须 32 hex
	// 小写(落 workflow_execution.trace_id,与 audit/outbox 的 traceIDRe
	// 同式三处互指)。
	TraceID string
	// IdempotencyKey 是触发幂等键的落库值(T4.2 兑现 D-t55 §5;api 侧已完成
	// 主体+定义二维作用域哈希,此处只透传落列)。空 → NULL(不去重,现状);
	// 撞 uk_exec_idem → ErrConflict(调用方读原行做重放响应)。
	IdempotencyKey string
}

// Instantiate 从钉住定义物化执行:单事务 INSERT workflow_execution(PENDING,
// 钉版四件套)+ 每 DSL 节点一行 node_execution(PENDING,type 与 max_attempts
// 快照,version=1)+ outbox(execution.start,exec:<id>)。行集与拓扑的一致性
// 由本函数建立(Complete 依赖「钉住 DSL 的每个节点恰有一行」)。
func (s *Scheduler) Instantiate(ctx context.Context, p InstantiateParams) (uint64, error) {
	if p.DefinitionID == 0 {
		return 0, errors.New("scheduler: zero definition id")
	}
	if p.TriggeredBy == "" {
		return 0, errors.New("scheduler: empty triggered_by")
	}
	def, err := s.loadPublishedDefinition(ctx, p.DefinitionID)
	if err != nil {
		return 0, err
	}
	_, g, err := workflowFromDef(p.DefinitionID, def)
	if err != nil {
		return 0, err
	}
	execID, err := s.deps.IDs.Next()
	if err != nil {
		return 0, fmt.Errorf("scheduler: gen execution id: %w", err)
	}
	// trace 域闸(review LOW:与 audit/outbox 先例同式,≤32 非法字符不静默
	// 落列;空放行 = 内部测试路径)。
	if p.TraceID != "" && !traceIDRe.MatchString(p.TraceID) {
		return 0, fmt.Errorf("scheduler: instantiate trace_id %q", p.TraceID)
	}
	input := p.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	// 触发时拒非对象输入({{inputs.*}} 解析面 = 顶层对象):放行会在 Start
	// 处确定性失败,经 asynq 重试后 archived、执行永卡 PENDING(review M2)。
	var inputShape map[string]json.RawMessage
	if err := json.Unmarshal(input, &inputShape); err != nil {
		return 0, fmt.Errorf("scheduler: workflow input must be a JSON object: %w", err)
	}
	triggerType := p.TriggerType
	if triggerType == "" {
		triggerType = "API"
	}
	var trace any
	if p.TraceID != "" {
		trace = p.TraceID
	}

	err = engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		var idem any
		if p.IdempotencyKey != "" {
			idem = p.IdempotencyKey
		}
		if _, err := q.ExecContext(ctx, `
INSERT INTO workflow_execution
  (id, definition_id, definition_name, definition_version, definition_hash,
   status, trigger_type, triggered_by, trace_id, input, idempotency_key)
VALUES (?,?,?,?,?, 'PENDING', ?, ?, ?, ?, ?)`,
			execID, p.DefinitionID, def.name, def.version.Int64, def.hash,
			triggerType, p.TriggeredBy, trace, input, idem); err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) && me.Number == 1062 && strings.Contains(me.Message, "uk_exec_idem") {
				// 幂等键双投递(T8.1 收敛面):非缺陷,结构化上抛供调用方重放。
				return fmt.Errorf("%w: idempotency key already triggered an execution", engine.ErrConflict)
			}
			// 其余 1062(PK 撞 = 生成器缺陷)与普通错误同路:不映射 ErrConflict
			// ——那会诱发 T2.5 式重决策循环(append.go 同哲学)。
			return fmt.Errorf("scheduler: insert execution %d: %w", execID, err)
		}
		for _, nodeID := range sortedNodes(g.nodes) {
			n := g.nodes[nodeID]
			rowID, err := s.deps.IDs.Next()
			if err != nil {
				return fmt.Errorf("scheduler: gen node id: %w", err)
			}
			maxAttempts := 1
			if n.Retry != nil {
				maxAttempts = n.Retry.MaxAttempts
			}
			if maxAttempts < 1 || maxAttempts > 10 {
				return fmt.Errorf("scheduler: node %s max_attempts %d out of [1,10]", nodeID, maxAttempts)
			}
			if _, err := q.ExecContext(ctx, `
INSERT INTO node_execution
  (id, execution_id, node_id, type, status, max_attempts, version)
VALUES (?,?,?,?,'PENDING',?,1)`,
				rowID, execID, nodeID, n.Type, maxAttempts); err != nil {
				return fmt.Errorf("scheduler: insert node %s: %w", nodeID, err)
			}
		}
		return s.appendEvent(ctx, q, execID, EventTypeExecutionStart,
			fmt.Sprintf("exec:%d", execID), ExecStartPayload{ExecutionID: execID, TraceID: p.TraceID})
	})
	if err != nil {
		return 0, err
	}
	s.log.Info("execution.instantiated",
		"execution_id", execID, "definition_id", p.DefinitionID, "nodes", len(g.nodes))
	return execID, nil
}

// Start 处理 execution.start:PENDING→RUNNING 并派发入口节点。
// 幂等:已 RUNNING/终态(重复投递 exec:)→ nil。
func (s *Scheduler) Start(ctx context.Context, execID uint64, workerID string) error {
	if execID == 0 || workerID == "" {
		return errors.New("scheduler: zero exec id or empty worker id")
	}
	exec, err := s.loadExecution(ctx, execID)
	if err != nil {
		return err
	}
	wf, g, err := s.loadWorkflow(ctx, exec.definitionID)
	if err != nil {
		return err
	}
	return engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		// 锁序例外(先 wf 后 node 行)的安全性见包注释锁序节。
		var status engine.WorkflowState
		err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=? FOR UPDATE`, execID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: workflow_execution %d", engine.ErrNotFound, execID)
		}
		if err != nil {
			return fmt.Errorf("scheduler: lock execution %d: %w", execID, err)
		}
		if status != engine.WorkflowPending {
			return nil // 双投递幂等:已起跑/已终态
		}
		if err := engine.StartExecution(ctx, q, execID); err != nil {
			return err
		}
		rows, err := lockAllRows(ctx, q, execID, g)
		if err != nil {
			return err
		}
		st := newExecState(s, ctx, q, execID, wf, g, rows, workerID)
		if err := st.buildEnv(exec.input); err != nil {
			return err
		}
		return st.advance(g.entry)
	})
}

// CompletionRequest 是执行完成回写的请求(worker 适配 Outcome 产出)。
type CompletionRequest struct {
	ExecID     uint64
	NodeExecID uint64
	AttemptID  uint64
	Status     engine.AttemptState // SUCCEEDED | FAILED
	Code       string
	Msg        string
	Output     json.RawMessage // SUCCEEDED 时落 node_execution.output(nil → NULL)
	WorkerID   string          // 完成方(重试再派发时的派发者归因)
	// Usage 是 LLM attempt 的 usage/cost 台账参数(T3.1):随 CloseAttempt
	// 终写落 node_attempt 列,并同事务发 llm.call.completed 审计行(双录)。
	// 仅 SUCCEEDED 可携(失败路径 P0 不计费,Complete 入口拒绝)。
	Usage *engine.AttemptUsage
	// ToolCall 是 tool attempt 的调用观测参数(T3.2):随收口同事务发
	// tool.call.completed 审计行(actor=tool/工具名)。仅 tool 节点 SUCCEEDED
	// 可携(失败的观测由 node.state_changed 承载,P0 不重复)。
	ToolCall *ToolCallInfo
}

// ToolCallInfo 是工具调用的审计观测参数(nodes.ToolExecutor 产出)。
type ToolCallInfo struct {
	Name       string // 工具名(registry 注册名,审计 actor_id 同源)
	DurationMs uint64 // 调用耗时(毫秒;T6.4 指标字源)
}

// maxTransientRetries 是 Complete 对 1213/1205 的内部重试上限(engine 错误分类
// 契约:唯一允许抖动重试的一类)。只重试回写事务,不重跑 executor。
const maxTransientRetries = 3

// errAttemptAlreadyClosed 是事务内的哨兵:CloseAttempt 撞已收口行 ⇒ 收口权已
// 旁落,幂等成功。两种先行收口来源(D-2026-09-22-t25-reaper-semantics #7,
// W1 拍板「接受等价」):a) 早前一次 Complete 已原子提交(收口+推进同事务);
// b) reaper DEAD 收口——前提是 DEAD ⇒ 节点必已被重派或取消(reaper 同事务
// 约束),放行迟到推进会与重派链路双推进;若该前提被破坏需重审本哨兵。
// 经 errors.Is 在事务外识别。
var errAttemptAlreadyClosed = errors.New("scheduler: attempt already closed (converged)")

// discardedByReaperError 携带让位丢弃事实的哨兵包装(T6.4,D-t36 §7):
// 仅当 Observed==DEAD(丢弃成因 = reaper 判死,区别于早前 Complete 收口)
// 且 req 携 usage(已计费)时返回。errors.Is 命中 errAttemptAlreadyClosed
// 语义不变;Complete 层解包后在事务外另发 llm.call.discarded 审计行
// (发射点必须事务外:哨兵自 WithTx 闭包返回 ⇒ 让位事务整体回滚)。
type discardedByReaperError struct {
	req    CompletionRequest
	nodeID string
}

func (e *discardedByReaperError) Error() string { return errAttemptAlreadyClosed.Error() }
func (e *discardedByReaperError) Is(target error) bool {
	return target == errAttemptAlreadyClosed
}

// Complete 是执行完成回写:单事务 = 锁全行集 → CloseAttempt → 节点收口
// (Succeed/Redispatch/Fail)→ 入度推进(派发/剪枝/fail-fast)→ TouchExecution;
// 提交后新事务判执行终态(SUCCEEDED + outputs 物化)。
func (s *Scheduler) Complete(ctx context.Context, req CompletionRequest) error {
	switch {
	case req.ExecID == 0 || req.NodeExecID == 0 || req.AttemptID == 0:
		return errors.New("scheduler: zero id field(s) in completion request")
	case req.Status != engine.AttemptSucceed && req.Status != engine.AttemptFailed:
		return fmt.Errorf("%w: attempt →%s (completion wants SUCCEEDED|FAILED)",
			engine.ErrIllegalTransition, req.Status)
	case req.WorkerID == "":
		return errors.New("scheduler: empty worker id in completion request")
	case req.Usage != nil && req.Status != engine.AttemptSucceed:
		return errors.New("scheduler: usage requires SUCCEEDED (failed LLM calls are unbilled in P0)")
	case req.ToolCall != nil && req.Status != engine.AttemptSucceed:
		return errors.New("scheduler: tool call info requires SUCCEEDED (failed calls ride node.state_changed)")
	}
	exec, err := s.loadExecution(ctx, req.ExecID)
	if err != nil {
		return err
	}
	wf, g, err := s.loadWorkflow(ctx, exec.definitionID)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := s.completeOnce(ctx, req, wf, g, exec.input)
		if err == nil {
			return nil
		}
		if errors.Is(err, errAttemptAlreadyClosed) {
			var d *discardedByReaperError
			if errors.As(err, &d) {
				s.emitLLMCallDiscarded(ctx, d.req, d.nodeID)
			}
			return nil
		}
		if !engine.Transient(err) || attempt >= maxTransientRetries {
			return err
		}
		backoff := time.Duration(float64(time.Millisecond*25<<(attempt+1)) * (1 + rand.Float64()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (s *Scheduler) completeOnce(ctx context.Context, req CompletionRequest,
	wf *dsl.Workflow, g *dag, wfInput json.RawMessage) error {
	var termGuard bool
	err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		// 1. 有序全行锁(当前读):本执行全部节点行,node_id 升序。
		rows, err := lockAllRows(ctx, q, req.ExecID, g)
		if err != nil {
			return err
		}
		n := rows.byID[req.NodeExecID]
		if n == nil {
			return fmt.Errorf("%w: node_execution %d not in execution %d",
				engine.ErrNotFound, req.NodeExecID, req.ExecID)
		}

		// 2. 终态守卫(快照读):已终态则跳过推进与重试/失败决策(在飞兄弟的
		// 迟到收口仍执行——fail-fast 语义;节点留 EXECUTING 归 T2.5 扫描)。
		// 窄竞态(守卫后他方才终态)无害:后续完成的提交后判定撞 CAS 良性收敛。
		var wfStatus engine.WorkflowState
		if err := q.QueryRowContext(ctx,
			`SELECT status FROM workflow_execution WHERE id=?`, req.ExecID).Scan(&wfStatus); err != nil {
			return fmt.Errorf("scheduler: read execution status %d: %w", req.ExecID, err)
		}
		termGuard = wfStatus.Terminal()

		// 3. attempt 收口(attempt 行锁;usage 非 nil 时同一条终写落台账列,
		// T3.1)。Conflict = 收口权已旁落(早前 Complete 或 reaper DEAD)
		// ⇒ 幂等;Observed==DEAD 时记结构化 warn——成功结果被
		// 丢弃 = 发生过判死双执行的观测信号(W1 哨兵,零行为变更)。
		if err := engine.CloseAttempt(ctx, q, req.AttemptID, req.Status, req.Code, req.Msg, req.Usage); err != nil {
			if errors.Is(err, engine.ErrConflict) {
				var ce *engine.ConflictError
				if errors.As(err, &ce) && ce.Observed == engine.NodeState(engine.AttemptDead) {
					s.log.Warn("complete.discarded_by_reaper",
						"execution_id", req.ExecID, "node_execution_id", req.NodeExecID,
						"attempt_id", req.AttemptID, "outcome", string(req.Status))
					// T6.4:已计费丢弃(携 usage)走哨兵包装,Complete 层事务外
					// 另发 llm.call.discarded;无 usage 维持裸哨兵(旧行为)。
					if req.Usage != nil {
						if n := rows.byID[req.NodeExecID]; n != nil {
							return &discardedByReaperError{req: req, nodeID: n.nodeID}
						}
					}
				}
				return errAttemptAlreadyClosed
			}
			return err
		}

		st := newExecState(s, ctx, q, req.ExecID, wf, g, rows, req.WorkerID)

		// 3a. LLM 台账审计行(T3.1 双录 audit 半边):usage/cost 列已随
		// CloseAttempt 终写落 node_attempt,这里同事务发 llm.call.completed
		// 携同组冗余列(合规证据链独立于状态表,列 = T6.4 聚合免解 JSON)。
		if req.Usage != nil {
			if err := st.auditLLMCall(n, req); err != nil {
				return err
			}
		}

		// 3b. 工具调用审计行(T3.2):tool.call.completed,actor=tool/工具名。
		// 类型校验在此(节点行此时已持有):非 tool 节点携带 ToolCall = 调用方
		// 装配缺陷,响亮拒绝防 llm/branch 冒发工具事件。
		if req.ToolCall != nil {
			if n.nodeType != "tool" {
				return fmt.Errorf("scheduler: tool call info on non-tool node %q (type %q)",
					n.nodeID, n.nodeType)
			}
			if err := st.auditToolCall(n, req); err != nil {
				return err
			}
		}

		// 4. 节点收口三分支。worker_id 回传行内值(派发方 ≠ 执行方,version 是
		// 真正裁决——见 nodecas.go SucceedNode 注释;行已 FOR UPDATE 持有,回传
		// 值不会陈旧)。
		switch req.Status {
		case engine.AttemptSucceed:
			newVer, err := engine.SucceedNode(ctx, q, n.id, n.version, n.workerID.String, req.Output)
			if err != nil {
				return err
			}
			if err := st.stageRow(audit.EventNodeStateChanged, n, req.AttemptID, audit.NodeStateChangedPayload{
				From: string(engine.NodeExecuting), To: string(engine.NodeSucceeded),
				NodeID: n.nodeID, Version: newVer, AttemptNo: n.attempts, WorkerID: n.workerID.String,
			}); err != nil {
				return err
			}
			n.status = engine.NodeSucceeded
			n.version = newVer
			n.output = sql.NullString{String: string(req.Output), Valid: len(req.Output) > 0}
			if termGuard {
				break
			}
			// env 必须在节点收口之后构建:后继引用本节点 output。
			if err := st.buildEnv(wfInput); err != nil {
				return err
			}
			if err := st.advanceFrom(n.nodeID); err != nil {
				return err
			}
		case engine.AttemptFailed:
			switch {
			case termGuard:
				// 终态执行内的迟到失败:不再重试/失败收口。
			case int(n.attempts) < int(n.maxAttempts):
				// 节点级重试:平台重试权威(新 attempt = 新 TaskID),节点留 EXECUTING。
				newVer, err := engine.RedispatchNode(ctx, q, n.id, n.version, engine.RedispatchInput{
					WorkerID:   req.WorkerID,
					DeadlineAt: time.Now().Add(NodeTimeout(n.nodeType)),
				})
				if err != nil {
					return err
				}
				n.version = newVer
				n.attempts++
				n.workerID = sql.NullString{String: req.WorkerID, Valid: true}
				if err := st.auditRow(audit.EventNodeRedispatched, n, 0, audit.NodeRedispatchedPayload{
					NodeID: n.nodeID, AttemptNo: n.attempts, Trigger: audit.TriggerAttemptFailed,
				}); err != nil {
					return err
				}
				if err := st.appendNodeTask(n); err != nil {
					return err
				}
				s.log.Info("node.retry_scheduled",
					"execution_id", req.ExecID, "node_execution_id", n.id,
					"attempt_no", n.attempts, "max_attempts", n.maxAttempts)
			default:
				code := req.Code
				if code == "" {
					code = "attempts_exhausted"
				}
				failVer, err := engine.FailNode(ctx, q, n.id, n.version, n.workerID.String, code, req.Msg)
				if err != nil {
					return err
				}
				if err := st.stageRow(audit.EventNodeStateChanged, n, req.AttemptID, audit.NodeStateChangedPayload{
					From: string(engine.NodeExecuting), To: string(engine.NodeFailed),
					NodeID: n.nodeID, Version: failVer, Code: code,
					AttemptNo: n.attempts, WorkerID: n.workerID.String,
				}); err != nil {
					return err
				}
				n.status = engine.NodeFailed
				if err := st.failExecution("node_failed",
					fmt.Sprintf("node %s failed: %s: %s", n.nodeID, code, req.Msg)); err != nil {
					return err
				}
			}
		}

		// 5. 活跃时刻刷新(wf 最后,DDL 契约:任一节点/attempt 推进同事务刷新)。
		return engine.TouchExecution(ctx, q, req.ExecID)
	})
	if err != nil {
		return err
	}
	if termGuard {
		return nil
	}
	// 提交后新事务判终态:新鲜快照见全部已提交(含本事务);失败只记日志不
	// 上抛(task 已完成,卡终态留 T2.5 扫描兜底——与任意崩溃同级)。
	if ferr := s.finishIfDone(ctx, req.ExecID, wf, wfInput); ferr != nil {
		s.log.Error("execution.finish_check_failed", "execution_id", req.ExecID, "err", ferr)
	}
	return nil
}

// FinishIfDone 导出终态补判入口(reaper 扫描④b 委托,D-2026-09-22-t25-reaper-
// semantics #9):RUNNING 执行全节点终态但提交后判定未落时收敛——Complete 的
// 提交后 finishIfDone 失败只记日志(sched.go「卡终态留 T2.5 扫描兜底」预留点)。
// 零锁路径,与 Complete 提交后判定完全同款;双收口由 status-only CAS 仲裁。
func (s *Scheduler) FinishIfDone(ctx context.Context, execID uint64) error {
	exec, err := s.loadExecution(ctx, execID)
	if err != nil {
		return err
	}
	wf, _, err := s.loadWorkflow(ctx, exec.definitionID)
	if err != nil {
		return err
	}
	return s.finishIfDone(ctx, execID, wf, exec.input)
}

// finishIfDone 在提交后新事务判定 SUCCEEDED 收口:无 PENDING/EXECUTING/
// WAITING_APPROVAL 行且无 FAILED 行 → 物化 outputs → FinishExecution。
// 不提前:行集在 Instantiate 已全量落库,在飞兄弟已提交为非终态,新鲜快照必见。
// 不漏判:最后一个提交者的重查必见全部终态。双收口由 status-only CAS 仲裁
// (Conflict 良性)。fail-fast 路径已在 Complete 事务内收口 FAILED,此处跳过。
func (s *Scheduler) finishIfDone(ctx context.Context, execID uint64, wf *dsl.Workflow, wfInput json.RawMessage) error {
	type nodeOut struct {
		nodeID string
		output sql.NullString
	}
	outRows, err := s.deps.DB.QueryContext(ctx,
		`SELECT node_id, status, output FROM node_execution WHERE execution_id=?`, execID)
	if err != nil {
		return fmt.Errorf("scheduler: finish check %d: %w", execID, err)
	}
	defer func() { _ = outRows.Close() }()
	var succeeded []nodeOut
	nonTerminal, failed := 0, 0
	for outRows.Next() {
		var nodeID string
		var status engine.NodeState
		var output sql.NullString
		if err := outRows.Scan(&nodeID, &status, &output); err != nil {
			return fmt.Errorf("scheduler: finish scan %d: %w", execID, err)
		}
		switch {
		case status == engine.NodeSucceeded:
			succeeded = append(succeeded, nodeOut{nodeID: nodeID, output: output})
		case status == engine.NodeFailed:
			failed++
		case !status.Terminal():
			nonTerminal++ // PENDING/EXECUTING/WAITING_APPROVAL
		} // SKIPPED/CANCELLED:终态,不阻塞 SUCCEEDED 判定
	}
	if err := outRows.Err(); err != nil {
		return fmt.Errorf("scheduler: finish rows %d: %w", execID, err)
	}
	if nonTerminal > 0 || failed > 0 {
		return nil
	}
	outputs := make(map[string]json.RawMessage, len(succeeded))
	for _, r := range succeeded {
		if r.output.Valid {
			outputs[r.nodeID] = json.RawMessage(r.output.String)
		}
	}
	env, err := newRefEnv(wfInput, outputs)
	if err != nil {
		return err
	}
	out := materializeOutputs(wf.Outputs, env)
	if err := engine.FinishExecution(ctx, s.deps.DB, execID, engine.WorkflowSucceeded, out, "", ""); err != nil {
		if errors.Is(err, engine.ErrConflict) {
			return nil // 他方已收口(双 finish 竞态的 CAS 仲裁)
		}
		return err
	}
	s.log.Info("execution.succeeded", "execution_id", execID)
	return nil
}

// ---- 事务内推进状态 ----

// nodeRow 是锁读到的节点行快照(事务内随 CAS 就地更新;前驱判定只读本副本,
// 锁序保证副本即最新已提交 + 本事务写)。
type nodeRow struct {
	id          uint64
	nodeID      string
	version     uint32
	status      engine.NodeState
	nodeType    string
	attempts    uint16
	maxAttempts uint16
	workerID    sql.NullString
	output      sql.NullString
}

// lockedRows 是全行集的两个索引。
type lockedRows struct {
	byNodeID map[string]*nodeRow
	byID     map[uint64]*nodeRow
}

// lockAllRows 按 node_id 升序逐行 FOR UPDATE 锁全执行行集(当前读)。逐行而非
// 单语句多行:engine.DBTX 契约最小面只有 QueryRowContext,零 API 改动;锁序
// 与 uk_node_exec(execution_id, node_id) 索引序一致。批量形态留待 DBTX 演进
// (dbtx.go「以新小接口嵌入扩展」注记)。行集与拓扑不一致(缺行)是数据缺陷,
// 响亮报错;DB 有而 DSL 无的行不锁不判(遗留行,归对账)。
func lockAllRows(ctx context.Context, q engine.DBTX, execID uint64, g *dag) (*lockedRows, error) {
	ids := sortedNodes(g.nodes)
	rows := &lockedRows{
		byNodeID: make(map[string]*nodeRow, len(ids)),
		byID:     make(map[uint64]*nodeRow, len(ids)),
	}
	for _, nodeID := range ids {
		var r nodeRow
		err := q.QueryRowContext(ctx, `
SELECT id, node_id, version, status, type, attempts, max_attempts, worker_id, output
  FROM node_execution WHERE execution_id=? AND node_id=? FOR UPDATE`,
			execID, nodeID).Scan(&r.id, &r.nodeID, &r.version, &r.status, &r.nodeType,
			&r.attempts, &r.maxAttempts, &r.workerID, &r.output)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("scheduler: node %q has no row in execution %d (definition/rows drift)", nodeID, execID)
		}
		if err != nil {
			return nil, fmt.Errorf("scheduler: lock node %q (exec %d): %w", nodeID, execID, err)
		}
		if r.nodeID != nodeID {
			return nil, fmt.Errorf("scheduler: node %q locked wrong row %q", nodeID, r.nodeID)
		}
		rows.byNodeID[nodeID] = &r
		rows.byID[r.id] = &r
	}
	return rows, nil
}

// execState 是单事务内的推进状态。
type execState struct {
	s        *Scheduler
	ctx      context.Context
	q        engine.DBTX
	execID   uint64
	wf       *dsl.Workflow
	dag      *dag
	rows     *lockedRows
	env      *refEnv
	workerID string // 派发方归因(完成方 worker)
	failed   bool   // fail-fast 已触发,停止一切派发
}

func newExecState(s *Scheduler, ctx context.Context, q engine.DBTX, execID uint64,
	wf *dsl.Workflow, g *dag, rows *lockedRows, workerID string) *execState {
	return &execState{s: s, ctx: ctx, q: q, execID: execID, wf: wf, dag: g, rows: rows, workerID: workerID}
}

// buildEnv 从工作流输入与当前 SUCCEEDED 行的 output 构建求值环境。
// 必须在节点收口之后调用(后继引用刚成功节点的 output)。
func (st *execState) buildEnv(wfInput json.RawMessage) error {
	outputs := make(map[string]json.RawMessage, len(st.rows.byNodeID))
	for id, r := range st.rows.byNodeID {
		if r.status == engine.NodeSucceeded && r.output.Valid {
			outputs[id] = json.RawMessage(r.output.String)
		}
	}
	env, err := newRefEnv(wfInput, outputs)
	if err != nil {
		return err
	}
	st.env = env
	return nil
}

// advanceFrom 处理刚 SUCCEEDED 节点的后继。
func (st *execState) advanceFrom(nodeID string) error {
	return st.advance(st.dag.succs[nodeID])
}

// advance 处理一批候选(去重 + node_id 升序):对每个仍 PENDING 的候选判前驱
// 全集(锁读当前值)——任一 FAILED → fail-fast;任一 SKIPPED → 剪枝并递归
// 传播其后续;全部 SUCCEEDED → 派发(branch 内联路由);尚有非终态前驱 → 等待。
func (st *execState) advance(candidates []string) error {
	if st.failed {
		return nil
	}
	seen := map[string]bool{}
	for _, id := range candidates {
		seen[id] = true
	}
	for _, id := range sortedKeys(seen) {
		row := st.rows.byNodeID[id]
		if row == nil {
			return fmt.Errorf("scheduler: node %q has no locked row", id)
		}
		if row.status != engine.NodePending {
			continue // 已被本事务更早步骤/他方处理(状态即裁决)
		}
		var failedPred, skippedPred string
		ready := true
		for _, p := range st.dag.preds[id] {
			switch st.rows.byNodeID[p].status {
			case engine.NodeFailed:
				failedPred = p
			case engine.NodeSkipped:
				skippedPred = p
			case engine.NodeSucceeded:
			default:
				ready = false // PENDING/EXECUTING/WAITING_APPROVAL:前驱未定
			}
		}
		switch {
		case failedPred != "":
			if err := st.failExecution("node_failed",
				fmt.Sprintf("node %s unreachable: predecessor %s failed", id, failedPred)); err != nil {
				return err
			}
		case skippedPred != "":
			if err := st.skipOne(row); err != nil {
				return err
			}
		case ready:
			if err := st.dispatchOne(row); err != nil {
				return err
			}
		}
		if st.failed {
			return nil
		}
	}
	return nil
}

// skipOne 剪枝一个 PENDING 节点并递归传播其后续(任一前驱 SKIPPED 的传递闭包)。
func (st *execState) skipOne(row *nodeRow) error {
	newVer, err := engine.SkipNode(st.ctx, st.q, row.id, row.version)
	if err != nil {
		return err
	}
	if err := st.stageRow(audit.EventNodeStateChanged, row, 0, audit.NodeStateChangedPayload{
		From: string(engine.NodePending), To: string(engine.NodeSkipped),
		NodeID: row.nodeID, Version: newVer,
	}); err != nil {
		return err
	}
	row.status = engine.NodeSkipped
	row.version = newVer
	st.s.log.Info("node.skipped", "execution_id", st.execID, "node_id", row.nodeID)
	return st.advance(st.dag.succs[row.nodeID])
}

// dispatchOne 派发一个就绪节点:branch 内联(dispatchBranch);其余求值快照 →
// Claim → outbox att 事件。求值失败 = 执行级 input_eval 失败(节点留 PENDING,
// 定义/数据级缺陷的落点)。
func (st *execState) dispatchOne(row *nodeRow) error {
	n := st.dag.nodes[row.nodeID]
	if n.Branch != nil {
		return st.dispatchBranch(row, n.Branch)
	}
	snap, err := buildSnapshot(n, st.env)
	if err != nil {
		return st.failExecution("input_eval", fmt.Sprintf("node %s: %v", row.nodeID, err))
	}
	newVer, err := engine.ClaimNode(st.ctx, st.q, row.id, row.version, engine.ClaimInput{
		WorkerID:      st.workerID,
		InputSnapshot: snap,
		DeadlineAt:    time.Now().Add(NodeTimeout(row.nodeType)),
	})
	if err != nil {
		return err
	}
	row.version = newVer
	row.status = engine.NodeExecuting
	row.attempts++
	row.workerID = sql.NullString{String: st.workerID, Valid: true}
	if err := st.stageRow(audit.EventNodeStateChanged, row, 0, audit.NodeStateChangedPayload{
		From: string(engine.NodePending), To: string(engine.NodeExecuting),
		NodeID: row.nodeID, Version: newVer, AttemptNo: row.attempts, WorkerID: st.workerID,
	}); err != nil {
		return err
	}
	if err := st.appendNodeTask(row); err != nil {
		return err
	}
	st.s.log.Info("node.dispatched",
		"execution_id", st.execID, "node_id", row.nodeID,
		"node_execution_id", row.id, "attempt_no", row.attempts)
	return nil
}

// dispatchBranch 内联执行 branch:路由先于 claim(错误不留 deadline=NULL 的
// 不可扫描 EXECUTING 行——T2.5 的 node 扫描谓词对 NULL 恒假),ClaimNode
// (operands 快照)→ SucceedNode(output=NULL,DDL:branch 恒 NULL)同事务;
// 取路目标进入推进,未取直接后继剪枝 + 传播。路由决定落 audit branch.evaluated
// 是 T2.6(T2.4 期间取路仅可从后继状态推断)。
func (st *execState) dispatchBranch(row *nodeRow, b *dsl.BranchNode) error {
	next, err := st.s.router(b, func(ref string) (any, bool) { return st.env.resolveText(ref) })
	if err != nil {
		return st.failExecution("branch_eval", fmt.Sprintf("node %s: %v", row.nodeID, err))
	}
	snap := branchOperands(b, st.env)
	ver1, err := engine.ClaimNode(st.ctx, st.q, row.id, row.version, engine.ClaimInput{
		WorkerID:      st.workerID,
		InputSnapshot: snap,
		// branch 无驻留:零值 deadline → NULL,不进任何超时扫描
	})
	if err != nil {
		return err
	}
	if err := st.stageRow(audit.EventNodeStateChanged, row, 0, audit.NodeStateChangedPayload{
		From: string(engine.NodePending), To: string(engine.NodeExecuting),
		NodeID: row.nodeID, Version: ver1, AttemptNo: row.attempts + 1, WorkerID: st.workerID,
	}); err != nil {
		return err
	}
	ver2, err := engine.SucceedNode(st.ctx, st.q, row.id, ver1, st.workerID, nil)
	if err != nil {
		return err
	}
	if err := st.stageRow(audit.EventNodeStateChanged, row, 0, audit.NodeStateChangedPayload{
		From: string(engine.NodeExecuting), To: string(engine.NodeSucceeded),
		NodeID: row.nodeID, Version: ver2, AttemptNo: row.attempts + 1, WorkerID: st.workerID,
	}); err != nil {
		return err
	}
	if err := st.stageRow(audit.EventBranchEvaluated, row, 0, audit.BranchEvaluatedPayload{
		NodeID: row.nodeID, Next: next,
	}); err != nil {
		return err
	}
	row.status = engine.NodeSucceeded
	row.version = ver2
	row.attempts++
	row.workerID = sql.NullString{String: st.workerID, Valid: true}
	st.s.log.Info("node.branch_routed",
		"execution_id", st.execID, "node_id", row.nodeID, "next", next)

	for _, u := range st.dag.succs[row.nodeID] {
		if u == next {
			continue
		}
		if urow := st.rows.byNodeID[u]; urow != nil && urow.status == engine.NodePending {
			if err := st.skipOne(urow); err != nil {
				return err
			}
		}
	}
	return st.advance([]string{next})
}

// failExecution 触发 fail-fast:FinishExecution(FAILED)(code 开放词表:
// node_failed/input_eval/branch_eval)。他方已收口(Conflict)是良性收敛。
func (st *execState) failExecution(code, msg string) error {
	st.failed = true
	err := engine.FinishExecution(st.ctx, st.q, st.execID, engine.WorkflowFailed, nil, code, msg)
	if errors.Is(err, engine.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	st.s.log.Warn("execution.failed_fast",
		"execution_id", st.execID, "code", code, "msg", msg)
	return nil
}

func (st *execState) appendNodeTask(row *nodeRow) error {
	return st.s.appendEvent(st.ctx, st.q, st.execID, EventTypeNodeTask,
		fmt.Sprintf("att:%d:%d", row.id, row.attempts),
		NodeTaskPayload{
			ExecutionID: st.execID, NodeExecutionID: row.id, AttemptNo: row.attempts,
			TraceID: traceIDFromCtx(st.ctx),
		})
}

// traceIDFromCtx 提取当段活跃 trace id(T6.2 跨进程传播:发射点把 ctx 的
// trace 塞进 payload,worker 消费侧 extract 续链;空 = 无 trace 上下文,
// payload 键缺席)。scheduler 事务由 worker/api 进程调用,ctx 即当段链。
func traceIDFromCtx(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

// traceIDRe 与 internal/audit/writer.go、internal/outbox/append.go 的同式
// 定义三处互指(32 hex 小写 = W3C trace id;W3C 头形态归 P1)。
var traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// auditRow 落一条审计行(T2.6 契约:每次状态迁移/调度裁决同事务必有审计行,
// D-2026-09-23-t26-audit-events)。ID 复用调度器雪花生成器;actor = 本事务
// 所在 worker 进程(调度逻辑运行于 worker,归因到进程,非 DSL 语义角色)。
// attemptID 仅观察者路径(Complete)携带——attempt 行此时已存在可锚定;
// 派发路径 attempt 行尚未由 worker 创建,只携 payload.attempt_no。
//
// T6.1 双路径:本 helper 走 Append 同步直写,仅低频事件(node.redispatched、
// approval.requested)使用;高频四类(node.state_changed/branch.evaluated/
// llm.call.completed/tool.call.completed)走 stageRow(Enqueue 暂存
// audit_pending,reaper flush 批量搬运)——分阶段口径见 audit/stage.go。
func (st *execState) auditRow(eventType string, row *nodeRow, attemptID uint64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("scheduler: marshal audit %s payload: %w", eventType, err)
	}
	id, err := st.s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("scheduler: gen audit id: %w", err)
	}
	return audit.Append(st.ctx, st.q, audit.Event{
		ID: id, EventType: eventType, ExecutionID: st.execID,
		NodeExecID: row.id, NodeID: row.nodeID, AttemptID: attemptID,
		ActorType: audit.ActorWorker, ActorID: st.workerID, Payload: raw,
	})
}

// stageRow 暂存一条高频审计行(T6.1 异步批量,同事务落 audit_pending;
// 形参与 auditRow 对称,调用点按事件类分流)。
func (st *execState) stageRow(eventType string, row *nodeRow, attemptID uint64, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("scheduler: marshal audit %s payload: %w", eventType, err)
	}
	id, err := st.s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("scheduler: gen audit id: %w", err)
	}
	return audit.Enqueue(st.ctx, st.q, audit.Event{
		ID: id, EventType: eventType, ExecutionID: st.execID,
		NodeExecID: row.id, NodeID: row.nodeID, AttemptID: attemptID,
		ActorType: audit.ActorWorker, ActorID: st.workerID, Payload: raw,
	})
}

// emitLLMCallDiscarded 发 llm.call.discarded(T6.4,D-t36 §7):迟到 SUCCEEDED
// 携 usage 被 reaper 判死让位丢弃——供应商已计费而 node_attempt 台账列与
// llm.call.completed 行双缺,本行是该调用唯一留下的持久凭证(已计费零台账
// 的观测闭环)。独立小事务同步直写(低频:判死竞态窗;不走暂存——对账面
// 需要 read-your-writes 同款纪律)。best-effort:Complete 已幂等成功,审计
// 失败不翻转业务结果,counter 先于审计发射(计数记事实,不依赖 DB 成败)。
func (s *Scheduler) emitLLMCallDiscarded(ctx context.Context, req CompletionRequest, nodeID string) {
	if s.deps.Metrics != nil {
		s.deps.Metrics.IncLLMCallDiscarded()
	}
	raw, err := json.Marshal(audit.LLMCallDiscardedPayload{
		NodeID:           nodeID,
		Model:            req.Usage.Model,
		PromptTokens:     req.Usage.PromptTokens,
		CompletionTokens: req.Usage.CompletionTokens,
		TotalTokens:      req.Usage.TotalTokens,
		CostAmount:       req.Usage.CostAmount,
		CostCurrency:     req.Usage.CostCurrency,
		CostEstimated:    req.Usage.Estimated,
		ObservedAttempt:  string(engine.AttemptDead),
	})
	if err != nil {
		s.log.Error("marshal llm.call.discarded payload",
			"execution_id", req.ExecID, "node_execution_id", req.NodeExecID,
			"attempt_id", req.AttemptID, "err", err)
		return
	}
	// 幂等锚(对抗审计 A2):asynq at-least-once 重投会让同一迟到
	// attempt 多次走让位路径——先查同 attempt 的 discarded 行,在场即
	// 跳过(审计行与 counter 都不重复;行 = 资金观测面,双记即假账)。
	existing, err := s.discardedRowExists(ctx, req.AttemptID)
	if err != nil {
		s.log.Error("probe llm.call.discarded dedup",
			"execution_id", req.ExecID, "attempt_id", req.AttemptID, "err", err)
		return
	}
	if existing {
		return
	}
	id, err := s.deps.IDs.Next()
	if err != nil {
		s.log.Error("gen audit id for llm.call.discarded",
			"execution_id", req.ExecID, "err", err)
		return
	}
	evt := audit.Event{
		ID: id, EventType: audit.EventLLMCallDiscarded, ExecutionID: req.ExecID,
		NodeExecID: req.NodeExecID, NodeID: nodeID, AttemptID: req.AttemptID,
		ActorType: audit.ActorWorker, ActorID: req.WorkerID,
		Payload: raw, Usage: req.Usage,
	}
	if err := engine.WithTx(ctx, s.deps.DB, func(q engine.DBTX) error {
		return audit.Append(ctx, q, evt)
	}); err != nil {
		s.log.Error("audit llm.call.discarded",
			"execution_id", req.ExecID, "node_execution_id", req.NodeExecID,
			"attempt_id", req.AttemptID, "err", err)
	}
}

// discardedRowExists 判同 attempt 的 llm.call.discarded 行是否已在场
// (emitLLMCallDiscarded 的幂等锚;audit_log 按执行日分区,此处走
// idx_audit_attempt 点查,attempt_id 全表唯一无需分区谓词)。
func (s *Scheduler) discardedRowExists(ctx context.Context, attemptID uint64) (bool, error) {
	var one int
	err := s.deps.DB.QueryRowContext(ctx, `
SELECT 1 FROM audit_log WHERE attempt_id=? AND event_type='llm.call.discarded' LIMIT 1`,
		attemptID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// auditLLMCall 发 llm.call.completed(T3.1):usage/cost 冗余列组的唯一携带者,
// 与 node_attempt 半边同事务双录。actor = llm/模型名(归因主体 = 实际执行
// 调用的模型;调度进程归因由并行的 node.state_changed 行承载,不重复)。
func (st *execState) auditLLMCall(row *nodeRow, req CompletionRequest) error {
	raw, err := json.Marshal(audit.LLMCallCompletedPayload{
		NodeID:           row.nodeID,
		Model:            req.Usage.Model,
		PromptTokens:     req.Usage.PromptTokens,
		CompletionTokens: req.Usage.CompletionTokens,
		TotalTokens:      req.Usage.TotalTokens,
		CostAmount:       req.Usage.CostAmount,
		CostCurrency:     req.Usage.CostCurrency,
		CostEstimated:    req.Usage.Estimated,
	})
	if err != nil {
		return fmt.Errorf("scheduler: marshal llm.call.completed payload: %w", err)
	}
	id, err := st.s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("scheduler: gen audit id: %w", err)
	}
	// T6.1:高频四类走暂存(Enqueue),reaper flush 批量搬运。
	return audit.Enqueue(st.ctx, st.q, audit.Event{
		ID: id, EventType: audit.EventLLMCallCompleted, ExecutionID: st.execID,
		NodeExecID: row.id, NodeID: row.nodeID, AttemptID: req.AttemptID,
		ActorType: audit.ActorLLM, ActorID: req.Usage.Model,
		Payload: raw, Usage: req.Usage,
	})
}

// auditToolCall 发 tool.call.completed(T3.2):与 llm.call.completed 对称的
// 成功收口证据行。actor = tool/工具名(归因主体 = 实际执行的工具;调度进程
// 归因由并行的 node.state_changed 行承载,不重复)。payload 无 token/凭证
// (铁律:审计行不含平台主凭证,也不含短时凭证本体)。
func (st *execState) auditToolCall(row *nodeRow, req CompletionRequest) error {
	raw, err := json.Marshal(audit.ToolCallCompletedPayload{
		NodeID:     row.nodeID,
		Tool:       req.ToolCall.Name,
		DurationMs: req.ToolCall.DurationMs,
	})
	if err != nil {
		return fmt.Errorf("scheduler: marshal tool.call.completed payload: %w", err)
	}
	id, err := st.s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("scheduler: gen audit id: %w", err)
	}
	// T6.1:高频四类走暂存(Enqueue),reaper flush 批量搬运。
	return audit.Enqueue(st.ctx, st.q, audit.Event{
		ID: id, EventType: audit.EventToolCallCompleted, ExecutionID: st.execID,
		NodeExecID: row.id, NodeID: row.nodeID, AttemptID: req.AttemptID,
		ActorType: audit.ActorTool, ActorID: req.ToolCall.Name,
		Payload: raw,
	})
}

func (s *Scheduler) appendEvent(ctx context.Context, q engine.DBTX, execID uint64,
	eventType, taskID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("scheduler: marshal %s payload: %w", eventType, err)
	}
	id, err := s.deps.IDs.Next()
	if err != nil {
		return fmt.Errorf("scheduler: gen event id: %w", err)
	}
	return outbox.Append(ctx, q, outbox.Event{
		ID:          id,
		ExecutionID: execID,
		EventType:   eventType,
		TaskID:      taskID,
		Queue:       s.queue,
		Payload:     raw,
	})
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
