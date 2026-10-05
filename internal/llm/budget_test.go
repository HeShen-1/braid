package llm

// 单测:token 预算纯闸(T18.2,D-2026-10-05-t182-token-budget-gate 决策 1/2)。
// 每执行口径的判定半边:Σ已计费 + 当次 prompt 估算 + 完成预留 > 限额即拒。
// 纯函数零 DB 零网络;消费点接线(internal/nodes)归 llm_test/栈测试。

import "testing"

func TestTokenBudgetEnabled(t *testing.T) {
	if (TokenBudget(0)).Enabled() {
		t.Fatalf("0 budget Enabled() = true, want false (缺省关)")
	}
	if !(TokenBudget(1)).Enabled() {
		t.Fatalf("1 budget Enabled() = false, want true")
	}
}

func TestTokenBudgetExceeded(t *testing.T) {
	cases := []struct {
		name    string
		budget  TokenBudget
		spent   uint64
		est     uint64
		reserve uint64
		want    bool
	}{
		{"gate off never exceeds", 0, ^uint64(0), ^uint64(0), ^uint64(0), false},
		{"all zero under positive budget", 100, 0, 0, 0, false},
		{"exact boundary is allowed", 1000, 400, 500, 100, false},
		{"one over is denied", 1000, 400, 500, 101, true},
		{"reserve counted", 1000, 0, 600, 401, true},
		{"spent alone over", 100, 101, 0, 0, true},
		{"est alone over", 100, 0, 101, 0, true},
		{"saturating add no wrap false-negative", 100, ^uint64(0), 1, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.budget.Exceeded(tc.spent, tc.est, tc.reserve); got != tc.want {
				t.Fatalf("Exceeded(%d,%d,%d) on budget %d = %v, want %v",
					tc.spent, tc.est, tc.reserve, tc.budget, got, tc.want)
			}
		})
	}
}
