package scheduler

// ApprovalOutcome 表驱动锚(T14.2 会签定局判据单点):api/worker/reaper
// 三方消费的行为矩阵在此钉死——any 首票定局(既有语义)、all 驳回优先、
// 票齐才 approve、防御分支(approvers<=0 永不自动通过)。

import "testing"

func TestApprovalOutcome(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		approvers  int
		approved   int
		rejected   int
		wantFinal  bool
		wantDirect string
	}{
		// any(或签,既有 T4.2 语义):任一票定局,方向随首票。
		{"any first approve finalizes", "any", 3, 1, 0, true, DecisionApproved},
		{"any first reject finalizes", "any", 3, 0, 1, true, DecisionRejected},
		{"any no votes waits", "any", 3, 0, 0, false, ""},
		{"any empty mode defaults to any", "", 3, 1, 0, true, DecisionApproved},

		// all(会签):任一驳回即败;全部批准才过;部分批准 = 中间态。
		{"all empty waits", "all", 3, 0, 0, false, ""},
		{"all partial approve is not final", "all", 3, 1, 0, false, ""},
		{"all one short of quorum not final", "all", 3, 2, 0, false, ""},
		{"all full approve finalizes", "all", 3, 3, 0, true, DecisionApproved},
		{"all single reject finalizes immediately", "all", 3, 0, 1, true, DecisionRejected},
		{"all reject beats earlier approves", "all", 3, 2, 1, true, DecisionRejected},
		{"all full approve plus late reject stays rejected", "all", 3, 3, 1, true, DecisionRejected},

		// 防御:schema minItems=1 使名单空不可达;若达,永不判 approve
		// 定局(Q9 永不自动通过的安全侧),reject 方向仍可达。
		{"all zero approvers never auto-approves", "all", 0, 0, 0, false, ""},
		{"all zero approvers reject still final", "all", 0, 0, 1, true, DecisionRejected},

		// 单人名单:all 退化为 any 形状(一人一票,首票即全票)。
		{"all single approver approve final", "all", 1, 1, 0, true, DecisionApproved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			final, direction := ApprovalOutcome(tc.mode, tc.approvers, tc.approved, tc.rejected)
			if final != tc.wantFinal || direction != tc.wantDirect {
				t.Fatalf("ApprovalOutcome(%q, %d, %d, %d) = (%v, %q), want (%v, %q)",
					tc.mode, tc.approvers, tc.approved, tc.rejected,
					final, direction, tc.wantFinal, tc.wantDirect)
			}
		})
	}
}
