package worker

// T3.1 验收集成测试(t13 真 MySQL + 真 asynq 链,不可达自动 skip):
// 固定 mock usage 下,台账金额与 usage 断言一致(ROADMAP 验收原文)。
//
// 链路:fakeExec 返回携带 Usage 的 Outcome → completeNode →
// scheduler.Complete → completeOnce 单事务[CloseAttempt(usage 列)+
// llm.call.completed(audit 冗余列)]→ 两表逐字断言。金额走生产算术路径
// (llm.Cost 整数定点),非测试内重算——变异在算术/落列/发射任一环即红。
//
// 变异自查(实现错 → 哪条红):
//   - CloseAttempt 丢 usage 列/换列 → attempt 半边断言红;
//   - audit 发射点丢失或移出事务 → audit 半边断言红;
//   - 双录两半边取值不同源 → 一致性断言红;
//   - Usage 仅 SUCCEEDED 的守卫被删 → TestUsageOnFailedRejected 红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hibiken/asynq"

	"braid/internal/audit"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/llm"
	"braid/internal/redistest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// ledgerTable:2/8 CNY 精确单价 + 1/1 USD fallback(金额可手算核对)。
const ledgerTable = `{
  "glm-5.3":  {"input_per_mtok": "2", "output_per_mtok": "8", "currency": "CNY"},
  "fallback": {"input_per_mtok": "1", "output_per_mtok": "1", "currency": "USD"}
}`

// runLLMAttempt 走完整生产链(真 worker + 真 asynq)收口一个携带 Usage 的
// attempt,返回读回锚(直接断言用)。
func runLLMAttempt(t *testing.T, model string, u llm.Usage) (execID, nodeExecID uint64) {
	t.Helper()
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID = seedNodeExec(t, d)

	table, err := llm.ParsePriceTable(ledgerTable)
	if err != nil {
		t.Fatalf("price table: %v", err)
	}
	cost, err := table.Cost(model, u)
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	au := cost.AttemptUsage(model, u)

	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{
			Status: engine.AttemptSucceed,
			Output: json.RawMessage(`{"summary":"ok"}`),
			Usage:  &au,
		}, nil
	}}
	queue := startWorker(t, d, ex, DefaultConfig())

	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	taskID := fmt.Sprintf("att:%d:1", nodeExecID)
	if _, err := enqueue(t, client, queue, taskID,
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitAttemptStatus(t, d, nodeExecID, 1, engine.AttemptSucceed)
	return execID, nodeExecID
}

// usageSnapshot 是两表各半边的统一断言快照(model/tokens/cost/estimated)。
type usageSnapshot struct {
	model             string
	prompt, comp, tot uint32
	amount, currency  sql.NullString
	estimated         bool
}

func readAttemptUsage(t *testing.T, nodeExecID uint64) (uint64, usageSnapshot) {
	t.Helper()
	var s usageSnapshot
	var pt, ct, tt sql.NullInt64
	var attemptID uint64
	err := sqltest.MustOpenAt(t, workerSchema).QueryRow(`
SELECT id, model, prompt_tokens, completion_tokens, total_tokens,
       cost_amount, cost_currency, estimated
  FROM node_attempt WHERE node_execution_id=? AND attempt_no=?`, nodeExecID, 1).
		Scan(&attemptID, &s.model, &pt, &ct, &tt, &s.amount, &s.currency, &s.estimated)
	if err != nil {
		t.Fatalf("read attempt usage: %v", err)
	}
	s.prompt, s.comp, s.tot = uint32(pt.Int64), uint32(ct.Int64), uint32(tt.Int64)
	return attemptID, s
}

func readAuditLLMUsage(t *testing.T, attemptID uint64) (usageSnapshot, string, string, audit.LLMCallCompletedPayload) {
	t.Helper()
	// T6.1:llm.call.completed 高频异步——读前先排空暂存(reaper flush 等价)。
	wd := sqltest.MustOpenAt(t, workerSchema)
	if _, err := audit.FlushAll(context.Background(), wd, 500, 10000); err != nil {
		t.Fatalf("flush staged audit: %v", err)
	}
	var s usageSnapshot
	var pt, ct, tt sql.NullInt64
	var actorType, actorID string
	var payload []byte
	err := wd.QueryRow(`
SELECT model, prompt_tokens, completion_tokens, total_tokens,
       cost_amount, cost_currency, cost_estimated, actor_type, actor_id, payload
  FROM audit_log WHERE event_type=? AND attempt_id=?`,
		audit.EventLLMCallCompleted, attemptID).
		Scan(&s.model, &pt, &ct, &tt, &s.amount, &s.currency, &s.estimated,
			&actorType, &actorID, &payload)
	if err != nil {
		t.Fatalf("read audit llm row: %v", err)
	}
	s.prompt, s.comp, s.tot = uint32(pt.Int64), uint32(ct.Int64), uint32(tt.Int64)
	var p audit.LLMCallCompletedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return s, actorType, actorID, p
}

// assertDoubleWritten:两半边逐字一致 + 与期望金额一致(验收原文)。
func assertDoubleWritten(t *testing.T, nodeExecID uint64,
	wantAmount, wantCurrency string, wantEstimated bool) {
	t.Helper()
	attemptID, a := readAttemptUsage(t, nodeExecID)
	b, actorType, actorID, payload := readAuditLLMUsage(t, attemptID)

	if a != b {
		t.Fatalf("double-write diverged: node_attempt=%+v audit_log=%+v", a, b)
	}
	if a.amount.String != wantAmount || !a.amount.Valid {
		t.Errorf("cost_amount = %+v, want %q(usage × 单价表精确直出)", a.amount, wantAmount)
	}
	if a.currency.String != wantCurrency || !a.currency.Valid {
		t.Errorf("cost_currency = %+v, want %q", a.currency, wantCurrency)
	}
	if a.estimated != wantEstimated {
		t.Errorf("estimated = %v, want %v", a.estimated, wantEstimated)
	}
	if actorType != audit.ActorLLM {
		t.Errorf("actor_type = %q, want llm", actorType)
	}
	if actorID != a.model {
		t.Errorf("actor_id = %q, want model %q", actorID, a.model)
	}
	if payload.Model != a.model || payload.PromptTokens != a.prompt ||
		payload.CompletionTokens != a.comp || payload.TotalTokens != a.tot ||
		payload.CostAmount != a.amount.String || payload.CostCurrency != a.currency.String ||
		payload.CostEstimated != a.estimated {
		t.Errorf("payload = %+v, want 与列同源", payload)
	}
}

// TestLLMLedgerDoubleWrittenKnownModel:known model 精确计费。
// (1000×2 + 150×8)/1e6 = 3200 micro = 0.003200 CNY,estimated=false。
func TestLLMLedgerDoubleWrittenKnownModel(t *testing.T) {
	_, nodeExecID := runLLMAttempt(t, "glm-5.3", llm.Usage{PromptTokens: 1000, CompletionTokens: 150, TotalTokens: 1150})
	attemptID, a := readAttemptUsage(t, nodeExecID)
	if a.model != "glm-5.3" || a.prompt != 1000 || a.comp != 150 || a.tot != 1150 {
		t.Fatalf("usage 原值断言失败: %+v(want glm-5.3 1000/150/1150 不求和)", a)
	}
	_ = attemptID
	assertDoubleWritten(t, nodeExecID, "0.003200", "CNY", false)
}

// TestLLMLedgerEstimatedFallback:未知模型走 fallback 估算,estimated=true
// (T3.1「未知模型 fallback 标 estimated=true」承载列)。
// (1000×1 + 1000×1)/1e6 = 0.002000 USD。
func TestLLMLedgerEstimatedFallback(t *testing.T) {
	_, nodeExecID := runLLMAttempt(t, "mystery-model", llm.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000})
	assertDoubleWritten(t, nodeExecID, "0.002000", "USD", true)
}

// TestNonLLMAttemptLeavesUsageNull:不携 Usage 的常规收口,usage/cost 列
// 保持 NULL(回归锚:扩展不污染 tool/branch 语义,NULL=不计费)。
func TestNonLLMAttemptLeavesUsageNull(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	execID, nodeExecID := seedNodeExec(t, d)
	ex := &fakeExec{fn: func(req Request, callNo int) (Outcome, error) {
		return Outcome{Status: engine.AttemptSucceed, Output: json.RawMessage(`{}`)}, nil
	}}
	queue := startWorker(t, d, ex, DefaultConfig())
	client := asynq.NewClient(redistest.MustOpt(t))
	t.Cleanup(func() { _ = client.Close() })
	if _, err := enqueue(t, client, queue, fmt.Sprintf("att:%d:1", nodeExecID),
		nodeTaskPayload{ExecutionID: execID, NodeExecutionID: nodeExecID, AttemptNo: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitAttemptStatus(t, d, nodeExecID, 1, engine.AttemptSucceed)

	var model, amount, currency sql.NullString
	var pt, ct, tt sql.NullInt64
	var estimated bool
	if err := d.QueryRow(`
SELECT model, prompt_tokens, completion_tokens, total_tokens,
       cost_amount, cost_currency, estimated
  FROM node_attempt WHERE node_execution_id=? AND attempt_no=?`, nodeExecID, 1).
		Scan(&model, &pt, &ct, &tt, &amount, &currency, &estimated); err != nil {
		t.Fatalf("read: %v", err)
	}
	if model.Valid || pt.Valid || ct.Valid || tt.Valid || amount.Valid || currency.Valid || estimated {
		t.Fatalf("non-llm attempt carries usage: model=%+v tokens=%v/%v/%v cost=%+v/%+v estimated=%v",
			model, pt, ct, tt, amount, currency, estimated)
	}
}

// TestUsageOnFailedRejected:FAILED + Usage 在 Complete 入口即拒(失败路径
// P0 不计费;调用方 bug 不静默丢弃)。
func TestUsageOnFailedRejected(t *testing.T) {
	d := sqltest.MustOpenAt(t, workerSchema)
	ids, err := snowflake.New(0)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: "ledger-reject"})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	err = sched.Complete(context.Background(), scheduler.CompletionRequest{
		ExecID: 1, NodeExecID: 2, AttemptID: 3,
		Status: engine.AttemptFailed, WorkerID: "w",
		Usage: &engine.AttemptUsage{Model: "m"},
	})
	if err == nil {
		t.Fatal("FAILED + Usage accepted, want rejection")
	}
}
