package scheduler

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"braid/internal/engine/dsl"
	"braid/internal/snowflake"
)

// CondRouter(T3.4 完整条件求值)表驱动:op 词表全矩阵、字面量/引用比较、数值
// 归一(int64 精确优先 float64 兜底,1==1.0)、容器深相等、声明序 first-match、
// 类型错误族。运行值口径与 refEnv 一致:数值恒 json.Number(UseNumber)。
// 变异自查:去掉 kind 检查 → 类型错子测试红;命中后继续扫 → 顺序子测试红;
// exists 未命中改报错 → exists 穿透子测试红;int64 精确路径删掉 → 2^53 边界
// 子测试红(1 vs 1.0 在 float64 路径下同判相等,杀不掉该变异)。

// when 构造单 case;right 为原始 JSON 文本,空串 = 右值缺省(exists)。
func when(left, op, right, next string) dsl.BranchCase {
	c := dsl.Condition{Left: left, Op: op}
	if right != "" {
		c.Right = json.RawMessage(right)
	}
	return dsl.BranchCase{When: c, Next: next}
}

func resolveBy(m map[string]any) func(string) (any, bool) {
	return func(ref string) (any, bool) {
		v, ok := m[ref]
		return v, ok
	}
}

func TestCondRouter(t *testing.T) {
	const (
		aFlag  = "{{nodes.a.output.flag}}"
		aText  = "{{nodes.a.output.text}}"
		aScore = "{{nodes.a.output.score}}"
		bScore = "{{nodes.b.output.score}}"
		aArr   = "{{nodes.a.output.tags}}"
		aNull  = "{{nodes.a.output.nul}}"
	)
	num := func(s string) json.Number { return json.Number(s) }

	tests := []struct {
		name    string
		cases   []dsl.BranchCase
		values  map[string]any
		want    string
		wantErr string
	}{
		// ---- exists:存在性 = 引用可解析;未命中是 false(剪枝臂探测),不是错 ----
		{
			name:   "exists_resolved_routes",
			cases:  []dsl.BranchCase{when(aFlag, "exists", "", "x")},
			values: map[string]any{aFlag: true},
			want:   "x",
		},
		{
			name: "exists_unresolved_falls_through_not_error",
			cases: []dsl.BranchCase{
				when("{{nodes.cut.output.no}}", "exists", "", "x"),
				when(aFlag, "exists", "", "y"),
			},
			values: map[string]any{aFlag: false},
			want:   "y",
		},
		{
			name:   "exists_null_value_counts_as_present",
			cases:  []dsl.BranchCase{when(aNull, "exists", "", "x")},
			values: map[string]any{aNull: nil},
			want:   "x",
		},

		// ---- eq/ne:字面量与引用比较;数值归一;容器深相等 ----
		{
			name:   "eq_bool_literal_true",
			cases:  []dsl.BranchCase{when(aFlag, "eq", `true`, "c")},
			values: map[string]any{aFlag: true},
			want:   "c",
		},
		{
			name:   "eq_bool_literal_false_to_default",
			cases:  []dsl.BranchCase{when(aFlag, "eq", `true`, "c")},
			values: map[string]any{aFlag: false},
			want:   "dflt",
		},
		{
			name:   "eq_string_literal",
			cases:  []dsl.BranchCase{when(aText, "eq", `"hello"`, "x")},
			values: map[string]any{aText: "hello"},
			want:   "x",
		},
		{
			name:   "ne_string",
			cases:  []dsl.BranchCase{when(aText, "ne", `"hello"`, "x")},
			values: map[string]any{aText: "world"},
			want:   "x",
		},
		{
			name: "eq_ref_vs_ref_equal",
			cases: []dsl.BranchCase{
				when(aScore, "eq", `"`+bScore+`"`, "x"),
			},
			values: map[string]any{aScore: num("9"), bScore: num("9")},
			want:   "x",
		},
		{
			name: "eq_ref_vs_ref_unequal_to_default",
			cases: []dsl.BranchCase{
				when(aScore, "eq", `"`+bScore+`"`, "x"),
			},
			values: map[string]any{aScore: num("9"), bScore: num("10")},
			want:   "dflt",
		},
		{
			name:   "eq_number_normalizes_1_and_1.0",
			cases:  []dsl.BranchCase{when(aScore, "eq", `1.0`, "x")},
			values: map[string]any{aScore: num("1")},
			want:   "x",
		},
		{
			name:   "eq_float_decimal_form_normalizes",
			cases:  []dsl.BranchCase{when(aScore, "eq", `0.10`, "x")},
			values: map[string]any{aScore: num("0.1")},
			want:   "x",
		},
		{
			name:   "eq_int64_precision_2p53_boundary",
			cases:  []dsl.BranchCase{when(aScore, "eq", `9007199254740992`, "x")},
			values: map[string]any{aScore: num("9007199254740993")},
			// 2^53+1 ≠ 2^53:float64 会双双舍入为 2^53 误判相等,int64 精确路径钉死。
			want: "dflt",
		},
		{
			name:   "gt_int64_precision_2p53_boundary",
			cases:  []dsl.BranchCase{when(aScore, "gt", `9007199254740992`, "x")},
			values: map[string]any{aScore: num("9007199254740993")},
			want:   "x",
		},
		{
			name:   "ne_number_normalized_is_false",
			cases:  []dsl.BranchCase{when(aScore, "ne", `1.0`, "x")},
			values: map[string]any{aScore: num("1")},
			want:   "dflt",
		},
		{
			name:   "eq_object_deep_equal",
			cases:  []dsl.BranchCase{when(aNull, "eq", `{"a":1,"b":[true,"2"]}`, "x")},
			values: map[string]any{aNull: map[string]any{"a": num("1"), "b": []any{true, "2"}}},
			want:   "x",
		},
		{
			name:   "eq_array_normalizes_members",
			cases:  []dsl.BranchCase{when(aArr, "eq", `[1.0,2]`, "x")},
			values: map[string]any{aArr: []any{num("1"), num("2")}},
			want:   "x",
		},
		{
			name:    "eq_kind_mismatch_is_error",
			cases:   []dsl.BranchCase{when(aText, "eq", `true`, "x")},
			values:  map[string]any{aText: "hello"},
			wantErr: "string vs boolean",
		},
		{
			name:    "eq_left_null_is_error",
			cases:   []dsl.BranchCase{when(aNull, "eq", `"x"`, "x")},
			values:  map[string]any{aNull: nil},
			wantErr: "is null",
		},
		{
			name:    "eq_left_unresolved_is_error",
			cases:   []dsl.BranchCase{when(aFlag, "eq", `true`, "x")},
			values:  map[string]any{},
			wantErr: "unresolved",
		},
		{
			name:    "eq_right_ref_unresolved_is_error",
			cases:   []dsl.BranchCase{when(aScore, "eq", `"`+bScore+`"`, "x")},
			values:  map[string]any{aScore: num("9")},
			wantErr: "unresolved",
		},
		{
			name:    "eq_right_null_literal_is_error",
			cases:   []dsl.BranchCase{when(aFlag, "eq", `null`, "x")},
			values:  map[string]any{aFlag: true},
			wantErr: "right is null",
		},
		{
			name:    "eq_missing_right_is_error",
			cases:   []dsl.BranchCase{when(aFlag, "eq", ``, "x")},
			values:  map[string]any{aFlag: true},
			wantErr: "requires a right operand",
		},

		// ---- gt/gte/lt/lte:number×number(int64 精确→float64)或 string×string(字节序) ----
		{
			name:   "gt_number_true",
			cases:  []dsl.BranchCase{when(aScore, "gt", `9`, "x")},
			values: map[string]any{aScore: num("10")},
			want:   "x",
		},
		{
			name:   "gt_number_false_to_default",
			cases:  []dsl.BranchCase{when(aScore, "gt", `9`, "x")},
			values: map[string]any{aScore: num("9")},
			want:   "dflt",
		},
		{
			name:   "gte_boundary_equal_true",
			cases:  []dsl.BranchCase{when(aScore, "gte", `9`, "x")},
			values: map[string]any{aScore: num("9")},
			want:   "x",
		},
		{
			name:   "lt_boundary_equal_false_to_default",
			cases:  []dsl.BranchCase{when(aScore, "lt", `9`, "x")},
			values: map[string]any{aScore: num("9")},
			want:   "dflt",
		},
		{
			name:   "gt_mixed_int_and_float_true",
			cases:  []dsl.BranchCase{when(aScore, "gt", `8.5`, "x")},
			values: map[string]any{aScore: num("9")},
			want:   "x",
		},
		{
			name:   "lte_float_left_int_right_true",
			cases:  []dsl.BranchCase{when(aScore, "lte", `9`, "x")},
			values: map[string]any{aScore: num("8.5")},
			want:   "x",
		},
		{
			name:   "lt_string_lexicographic_true",
			cases:  []dsl.BranchCase{when(aText, "lt", `"abd"`, "x")},
			values: map[string]any{aText: "abc"},
			want:   "x",
		},
		{
			name:   "gte_string_boundary_equal_true",
			cases:  []dsl.BranchCase{when(aText, "gte", `"abc"`, "x")},
			values: map[string]any{aText: "abc"},
			want:   "x",
		},
		{
			name:    "gt_bool_vs_number_is_error",
			cases:   []dsl.BranchCase{when(aFlag, "gt", `9`, "x")},
			values:  map[string]any{aFlag: true},
			wantErr: "boolean vs number",
		},
		{
			name:    "gt_number_vs_string_is_error",
			cases:   []dsl.BranchCase{when(aScore, "gt", `"9"`, "x")},
			values:  map[string]any{aScore: num("9")},
			wantErr: "number vs string",
		},

		// ---- contains:string 子串;array 成员(元素相等走 eq 口径,类型不合不报错) ----
		{
			name:   "contains_string_substring_true",
			cases:  []dsl.BranchCase{when(aText, "contains", `"wor"`, "x")},
			values: map[string]any{aText: "hello world"},
			want:   "x",
		},
		{
			name:   "contains_string_no_match_to_default",
			cases:  []dsl.BranchCase{when(aText, "contains", `"zzz"`, "x")},
			values: map[string]any{aText: "hello"},
			want:   "dflt",
		},
		{
			name:   "contains_array_member_true",
			cases:  []dsl.BranchCase{when(aArr, "contains", `"a"`, "x")},
			values: map[string]any{aArr: []any{"a", num("1"), true}},
			want:   "x",
		},
		{
			name:   "contains_array_number_normalized",
			cases:  []dsl.BranchCase{when(aArr, "contains", `1.0`, "x")},
			values: map[string]any{aArr: []any{num("1"), num("2")}},
			want:   "x",
		},
		{
			name:   "contains_array_mixed_kind_no_member_no_error",
			cases:  []dsl.BranchCase{when(aArr, "contains", `"x"`, "x")},
			values: map[string]any{aArr: []any{num("1"), num("2")}},
			want:   "dflt",
		},
		{
			name:    "contains_left_number_is_error",
			cases:   []dsl.BranchCase{when(aScore, "contains", `"9"`, "x")},
			values:  map[string]any{aScore: num("9")},
			wantErr: "number vs string",
		},
		{
			name:    "contains_right_null_is_error",
			cases:   []dsl.BranchCase{when(aArr, "contains", `null`, "x")},
			values:  map[string]any{aArr: []any{num("1")}},
			wantErr: "right is null",
		},

		// ---- 多条件组合:声明序 first-match;命中短路;谓词错中止不续扫 ----
		{
			name: "first_match_wins_in_order",
			cases: []dsl.BranchCase{
				when(aScore, "gt", `5`, "first"),
				when(aScore, "gt", `1`, "second"),
			},
			values: map[string]any{aScore: num("9")},
			want:   "first",
		},
		{
			name: "match_short_circuits_later_case_error",
			cases: []dsl.BranchCase{
				when(aScore, "gt", `5`, "first"),
				when(aText, "eq", `true`, "boom"),
			},
			values: map[string]any{aScore: num("9"), aText: "hello"},
			want:   "first",
		},
		{
			name: "earlier_case_error_aborts_scan",
			cases: []dsl.BranchCase{
				when(aText, "eq", `true`, "boom"),
				when(aScore, "gt", `5`, "second"),
			},
			values:  map[string]any{aScore: num("9"), aText: "hello"},
			wantErr: "string vs boolean",
		},
		{
			name: "all_false_falls_to_default",
			cases: []dsl.BranchCase{
				when(aScore, "gt", `100`, "x"),
				when(aText, "eq", `"zzz"`, "y"),
			},
			values: map[string]any{aScore: num("9"), aText: "hello"},
			want:   "dflt",
		},

		// ---- 防御:静态已拦,运行时兜底响亮报错 ----
		{
			name:    "unknown_op_is_error",
			cases:   []dsl.BranchCase{when(aFlag, "matches", `true`, "x")},
			values:  map[string]any{aFlag: true},
			wantErr: `unknown op "matches"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &dsl.BranchNode{Cases: tt.cases, Default: "dflt"}
			next, err := CondRouter(b, resolveBy(tt.values))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err=%v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CondRouter: %v", err)
			}
			if next != tt.want {
				t.Fatalf("next=%q, want %q", next, tt.want)
			}
		})
	}
}

// TestNewDefaultRouter:Router 缝缺省换装 = CondRouter(T3.4 替换 T2.4 桩的
// 接线断言——桩已删,默认值必须钉在新实现上)。
func TestNewDefaultRouter(t *testing.T) {
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	s, err := New(Deps{DB: &sql.DB{}, IDs: ids})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if reflect.ValueOf(s.router).Pointer() != reflect.ValueOf(CondRouter).Pointer() {
		t.Fatalf("default router is not CondRouter")
	}
}
