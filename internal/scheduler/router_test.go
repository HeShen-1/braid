package scheduler

import (
	"strings"
	"testing"

	"braid/internal/engine/dsl"
)

// StubRouter(T2.4 桩谓词路由)表驱动:显式布尔字段路由的全部判定。
// 变异自查:去掉类型检查 → 非布尔子测试红;顺序遍历错 → 多 case 首真子测试红。

func stubBranch(cases ...[2]string) *dsl.BranchNode {
	b := &dsl.BranchNode{Default: "fallback"}
	for _, c := range cases {
		b.Cases = append(b.Cases, dsl.BranchCase{
			When: dsl.Condition{Left: c[0], Op: "eq"},
			Next: c[1],
		})
	}
	return b
}

func TestStubRouter(t *testing.T) {
	resolve := func(m map[string]any) func(string) (any, bool) {
		return func(ref string) (any, bool) {
			v, ok := m[ref]
			return v, ok
		}
	}
	tests := []struct {
		name    string
		branch  *dsl.BranchNode
		values  map[string]any
		want    string
		wantErr string
	}{
		{
			name:   "first_case_true",
			branch: stubBranch([2]string{"{{nodes.a.output.flag}}", "yes"}, [2]string{"{{nodes.a.output.other}}", "no"}),
			values: map[string]any{"{{nodes.a.output.flag}}": true, "{{nodes.a.output.other}}": false},
			want:   "yes",
		},
		{
			name:   "second_case_true_in_order",
			branch: stubBranch([2]string{"{{nodes.a.output.flag}}", "yes"}, [2]string{"{{nodes.a.output.other}}", "no"}),
			values: map[string]any{"{{nodes.a.output.flag}}": false, "{{nodes.a.output.other}}": true},
			want:   "no",
		},
		{
			name:   "all_false_falls_to_default",
			branch: stubBranch([2]string{"{{nodes.a.output.flag}}", "yes"}),
			values: map[string]any{"{{nodes.a.output.flag}}": false},
			want:   "fallback",
		},
		{
			name:    "non_boolean_left_is_error",
			branch:  stubBranch([2]string{"{{nodes.a.output.text}}", "yes"}),
			values:  map[string]any{"{{nodes.a.output.text}}": "hello"},
			wantErr: "want boolean",
		},
		{
			name:    "unresolvable_left_is_error",
			branch:  stubBranch([2]string{"{{nodes.a.output.missing}}", "yes"}),
			values:  map[string]any{},
			wantErr: "unresolved",
		},
		{
			name:    "number_left_is_type_error",
			branch:  stubBranch([2]string{"{{nodes.a.output.n}}", "yes"}),
			values:  map[string]any{"{{nodes.a.output.n}}": float64(1)},
			wantErr: "want boolean", // 桩不做 truthy 转换:显式布尔字段,类型即语义
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, err := StubRouter(tt.branch, resolve(tt.values))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("StubRouter: %v", err)
			}
			if next != tt.want {
				t.Fatalf("next=%q, want %q", next, tt.want)
			}
		})
	}
}
