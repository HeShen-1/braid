package reaper

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"braid/internal/metrics"
)

// newReaperMetrics = newReaper + Metrics 注入(T17.4:毒快照/对账/让位计数
// 的断言面)。
func newReaperMetrics(t *testing.T, cfg Config, m *metrics.ReaperSet) (*Reaper, *sql.DB) {
	t.Helper()
	return newReaperDeps(t, cfg, func(d *Deps) { d.Metrics = m })
}

// TestApprovalPoisonSnapshotCounted(T17.4):快照漂移三面(decode / mode /
// on_timeout)各计对应 counter 面;行零改写(永不自动通过语义不变,
// TestApprovalSnapshotDriftNeverAutoPass 的计数镜像)。
func TestApprovalPoisonSnapshotCounted(t *testing.T) {
	set, _ := metrics.NewSet()
	r, d := newReaperMetrics(t, testCfg(), set.Reaper)

	// on_timeout 词表外。
	s1 := seedApproval(t, d, "RUNNING", 5*time.Minute, "explode", false)
	// mode 词表外。
	s2 := seedApprovalMode(t, d, "RUNNING", 5*time.Minute, "fail", false, "quantum", "alice")
	// 形状漂移(decode 面):JSON 列校验挡住非法 JSON,decode 失败的真实
	// 形态 = 合法 JSON 但非对象形状(数组解进 struct 必败)。
	s3 := seedApproval(t, d, "RUNNING", 5*time.Minute, "fail", false)
	if _, err := d.Exec(`UPDATE node_execution SET input_snapshot='[1,2]' WHERE id=?`,
		s3.nodeExecID); err != nil {
		t.Fatalf("poison snapshot: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v(逐项错误隔离,漂移行不阻断整轮)", err)
	}
	for facet, want := range map[string]float64{
		metrics.PoisonReasonOnTimeout: 1,
		metrics.PoisonReasonMode:      1,
		metrics.PoisonReasonDecode:    1,
	} {
		if got := testutil.ToFloat64(set.Reaper.PoisonSnapshots.WithLabelValues(facet)); got != want {
			t.Errorf("poison{%s} = %v, want %v", facet, got, want)
		}
	}
	// 三行都零改写:漂移面永不自动通过。
	for _, s := range []approvalSeeded{s1, s2, s3} {
		ns, errCode, escalatedAt := readApprovalNode(t, d, s.nodeExecID)
		if ns != "WAITING_APPROVAL" || errCode.Valid || escalatedAt.Valid {
			t.Errorf("漂移行被改写: %s %v %v", ns, errCode, escalatedAt)
		}
	}
}
