package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/metrics"
	"braid/internal/redistest"
	"braid/internal/scheduler"
)

// TestResumeRejectFacetVocabulary(T17.4):scheduler 5a 哨兵 ↔ metrics 词面
// 的唯一映射点四面全锚——漂移在这里红,不在告警规则里静默失配;非哨兵
// (infra/外族)恒空串不计数。
func TestResumeRejectFacetVocabulary(t *testing.T) {
	pairs := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrap: %w", scheduler.ErrResumeStaleRound), metrics.ResumeRejectStaleRound},
		{fmt.Errorf("wrap: %w", scheduler.ErrResumeFabricatedEvent), metrics.ResumeRejectFabricated},
		{fmt.Errorf("wrap: %w", scheduler.ErrResumePrematureEvent), metrics.ResumeRejectPremature},
		{fmt.Errorf("wrap: %w", scheduler.ErrResumeAnchorDrift), metrics.ResumeRejectAnchorDrift},
	}
	for _, p := range pairs {
		if got := resumeRejectFacet(p.err); got != p.want {
			t.Errorf("facet(%v) = %q, want %q", p.err, got, p.want)
		}
	}
	if got := resumeRejectFacet(errors.New("dial tcp: refused")); got != "" {
		t.Errorf("infra error classified as 5a facet %q", got)
	}
	if got := resumeRejectFacet(nil); got != "" {
		t.Errorf("nil error classified as 5a facet %q", got)
	}
}

// seedResumeTarget 直插一个挂起审批现场(真 DSL 进 definition,Resume 的
// spec 查找需要钉版图)。行形态镜像 e2e_notify 的 seedWaitingApprovalEscalated,
// attempts=1 ⇒ 4a 守卫期望 attempt_no=2。
func seedResumeTarget(t *testing.T, d *sql.DB, defID, execID, nodeExecID uint64) {
	t.Helper()
	dslDoc := `{"dsl":"1.0","name":"resume-metrics-t",
 "edges":[{"from":"a","to":"b"}],
 "nodes":[
  {"id":"a","type":"llm","prompt":{"template":"gen"},"outputs":{"text":"string"}},
  {"id":"b","type":"approval","approvers":["carol"],"mode":"any","timeout_hours":24,
   "on_timeout":"fail","instructions":"请审批","outputs":{"comment":"string"}}],
 "outputs":{"doc":"{{nodes.b.output.comment}}"}}`
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(`INSERT INTO workflow_definition
	  (id, name, version, status, dsl_version, dsl, dsl_hash, created_by, updated_by, published_by, published_at)
	  VALUES (?, ?, 2, 'PUBLISHED', '1.0', ?, ?, 'seed', 'seed', 'seed', NOW(3))`,
		defID, fmt.Sprintf("resume-metrics-%d", defID), dslDoc, string(make([]byte, 64)))
	ins(`INSERT INTO workflow_execution
	  (id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
	  VALUES (?, ?, ?, 2, ?, 'WAITING_APPROVAL', 'seed', '{}')`,
		execID, defID, fmt.Sprintf("resume-metrics-%d", defID), string(make([]byte, 64)))
	snapshot := `{"approvers":["carol"],"mode":"any","timeout_hours":24,"on_timeout":"fail","instructions":"请审批"}`
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, worker_id, version, input_snapshot, deadline_at)
	  VALUES (?, ?, 'b', 'approval', 'WAITING_APPROVAL', 1, 'seed-dispatcher', 2, ?,
	          NOW(3) + INTERVAL 48 HOUR)`, nodeExecID, execID, snapshot)
	// 图内全节点必须有行(lockAllRows 的 definition/rows drift 守卫):a 是
	// b 挂起前的前驱,已收口。
	ins(`INSERT INTO node_execution
	  (id, execution_id, node_id, type, status, attempts, worker_id, version, input_snapshot, deadline_at)
	  VALUES (?, ?, 'a', 'llm', 'SUCCEEDED', 1, 'seed-dispatcher', 3, '{}', NULL)`,
		nodeExecID+1, execID)
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM node_execution WHERE execution_id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_execution WHERE id=?`, execID)
		_, _ = d.Exec(`DELETE FROM workflow_definition WHERE id=?`, defID)
	})
}

// TestResumeEventRejectionCounted(T17.4):5a 拒绝面链路级断言——resume 事件
// attempt 号错位(4a 守卫)→ 哨兵拒绝 → counter{stale_round} 恰一次 + 上抛
// 照旧(重试穷尽 archived = 该面既定死信可见性,语义不变)。
func TestResumeEventRejectionCounted(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	set, _ := metrics.NewSet()
	now := uint64(time.Now().UnixNano()) & 0x7FFFFFFFFFF
	seedResumeTarget(t, d, now, now, now+1)
	execID, nodeExecID := now, now+1

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed}, nil
	}}
	cfg := DefaultConfig()
	cfg.RetryDelay = 50 * time.Millisecond
	queue := startWorkerOptMetrics(t, d, ex, cfg, set.Worker)

	// attempt_no=9 ≠ attempts+1=2 → ErrResumeStaleRound。
	payload := fmt.Sprintf(`{"execution_id":%d,"node_execution_id":%d,"decision_id":9527,"attempt_no":9}`,
		execID, nodeExecID)
	taskID := fmt.Sprintf("resume:%d:b:9", nodeExecID)
	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.EnqueueContext(ctx,
		asynq.NewTask(scheduler.EventTypeExecutionResume, []byte(payload)),
		asynq.TaskID(taskID), asynq.Queue(queue), asynq.MaxRetry(1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	insp := asynq.NewInspector(redistest.MustOpt(t))
	t.Cleanup(func() { _ = insp.Close() })
	waitTaskState(t, insp, queue, taskID, asynq.TaskStateArchived)

	// 计数口径 = 每投递一次(MaxRetry=1 ⇒ 首投 + 重试 = 2):重放多次即
	// 多次观测,告警语义是「面出现」(increase > 0)非精确普查。
	if got := testutil.ToFloat64(set.Worker.ResumeRejected.WithLabelValues(metrics.ResumeRejectStaleRound)); got != 2 {
		t.Fatalf("resume_rejected{stale_round} = %v, want 2(首投 + MaxRetry=1 重试各计一次)", got)
	}
	for _, facet := range []string{metrics.ResumeRejectFabricated,
		metrics.ResumeRejectPremature, metrics.ResumeRejectAnchorDrift} {
		if got := testutil.ToFloat64(set.Worker.ResumeRejected.WithLabelValues(facet)); got != 0 {
			t.Errorf("resume_rejected{%s} = %v, want 0(单面单计数)", facet, got)
		}
	}
	var status engine.NodeState
	if err := d.QueryRow(`SELECT status FROM node_execution WHERE id=?`, nodeExecID).Scan(&status); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if status != engine.NodeWaitingApproval {
		t.Fatalf("node = %s, want WAITING_APPROVAL(拒绝零动作)", status)
	}
}
