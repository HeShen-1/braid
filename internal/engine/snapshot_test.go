package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// 验收 2:每次状态迁移写入输入快照,任意节点可从快照复现输入。
// MySQL JSON 列会规范化存储(键按长度+字典序重排、去空白),字节级不变从不被
// 承诺;复现的合同是**语义相等**:decode→深比较(防键序侥幸)+ 读回确定性
// (读两次字节相同,防驱动/charset 失真)。单键文档字节也稳定,用于字节级抽查。

var roundtripDoc = json.RawMessage(`{
  "文本": "日本語 emoji 🚀",
  "nested": {"z": [1, 2, {"deep": true}], "a": 0.1},
  "arr": [null, false, ""],
  "num": 1e3
}`)

func TestSnapshotRoundtripAndReproduce(t *testing.T) {
	f := newFixture(t)
	id := f.node()
	if _, err := ClaimNode(f.ctx(), f.db, id, 1, ClaimInput{WorkerID: "w1", InputSnapshot: roundtripDoc}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got1, err := ReadNodeInput(f.ctx(), f.db, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got2, _ := ReadNodeInput(f.ctx(), f.db, id)
	if string(got1) != string(got2) {
		t.Fatalf("读回不确定: %s vs %s", got1, got2)
	}
	// 语义相等(验收口径)
	var want, got any
	if err := json.Unmarshal(roundtripDoc, &want); err != nil {
		t.Fatalf("want json: %v", err)
	}
	if err := json.Unmarshal(got1, &got); err != nil {
		t.Fatalf("got json: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("快照语义不等: want %s got %s", roundtripDoc, got1)
	}
	// 复现 = 读回即用:快照可直驱节点执行(此处以再序列化为节点输入形态验证)
	var re any
	if err := json.Unmarshal(got1, &re); err != nil {
		t.Fatalf("复现输入不可用: %v", err)
	}
}

// 快照不变量全称断言(多节点全状态矩阵后逐行校验):
// status∈{PENDING,SKIPPED} ⇒ NULL;status∈{EXECUTING,WAITING_APPROVAL,SUCCEEDED,FAILED}
// ⇒ 非空且等于该节点最后一次进入 EXECUTING 时携带的解析输入;CANCELLED 继承不约束。
func TestSnapshotInvariantSweep(t *testing.T) {
	f := newFixture(t)

	_ = f.node() // PENDING:从未进入 EXECUTING(行本身即断言载体,无需引用)
	skipped := f.node()
	if _, err := SkipNode(f.ctx(), f.db, skipped, 1); err != nil {
		t.Fatalf("skip: %v", err)
	}

	executing := f.node()
	s4 := json.RawMessage(`{"node":"executing"}`)
	if _, err := ClaimNode(f.ctx(), f.db, executing, 1, ClaimInput{WorkerID: "w", InputSnapshot: s4}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	suspended := f.node()
	s5 := json.RawMessage(`{"node":"wa"}`)
	if _, err := ClaimNode(f.ctx(), f.db, suspended, 1, ClaimInput{WorkerID: "w", InputSnapshot: s5}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := SuspendNode(f.ctx(), f.db, suspended, 2, "w", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	succeeded := f.node()
	s6 := json.RawMessage(`{"node":"ok"}`)
	if _, err := ClaimNode(f.ctx(), f.db, succeeded, 1, ClaimInput{WorkerID: "w", InputSnapshot: s6}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := SucceedNode(f.ctx(), f.db, succeeded, 2, "w", json.RawMessage(`{"o":1}`)); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	failed := f.node()
	s7 := json.RawMessage(`{"node":"bad"}`)
	if _, err := ClaimNode(f.ctx(), f.db, failed, 1, ClaimInput{WorkerID: "w", InputSnapshot: s7}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := FailNode(f.ctx(), f.db, failed, 2, "w", "x", "y"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	requeued := f.node() // requeue 后 PENDING:快照必须归 NULL(DDL 契约)
	if _, err := ClaimNode(f.ctx(), f.db, requeued, 1, ClaimInput{WorkerID: "w", InputSnapshot: json.RawMessage(`{"node":"again"}`)}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := RequeueNode(f.ctx(), f.db, requeued, 2); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	want := map[uint64]json.RawMessage{
		executing: s4,
		suspended: s5,
		succeeded: s6,
		failed:    s7,
	}
	rows, err := f.db.QueryContext(f.ctx(),
		`SELECT id, status, input_snapshot FROM node_execution WHERE execution_id=?`, f.execID)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var rid uint64
		var st NodeState
		var snap *string // NULL → nil
		if err := rows.Scan(&rid, &st, &snap); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		switch st {
		case NodePending, NodeSkipped:
			if snap != nil {
				t.Errorf("node %d %s 应 NULL 快照,got %s", rid, st, *snap)
			}
		case NodeExecuting, NodeWaitingApproval, NodeSucceeded, NodeFailed:
			if snap == nil {
				t.Errorf("node %d %s 快照缺失", rid, st)
				continue
			}
			jsonEqStr(t, *snap, want[rid])
		case NodeCancelled:
			// 继承来源行,不约束
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 7 {
		t.Fatalf("扫到 %d 行, want 7(全称断言必须遍历每一行)", seen)
	}
}

func TestReadNodeInputMissing(t *testing.T) {
	f := newFixture(t)
	if _, err := ReadNodeInput(f.ctx(), f.db, newID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}
