package engine

import (
	"regexp"
	"testing"

	"braid/internal/db/sqltest"
)

// T0 ENUM 词表钉死:DDL 冻结下唯一能机器钉住的契约面——Go 常量集与
// information_schema 实际 ENUM 词表比对。**含序**:ENUM「只许末尾追加」的演化
// 契约依赖定义序(序号即存储值),中途插值/重排必须红。
func TestEnumVocabularyPinned(t *testing.T) {
	d := sqltest.MustOpen(t)
	cases := []struct {
		table string
		col   string
		want  []string
	}{
		// 顺序 = DDL 定义序,与 states.go 常量声明序一致
		{"node_execution", "status", stateStrs(nodeStates)},
		{"workflow_execution", "status", stateStrs(wfStates)},
		{"node_attempt", "status", stateStrs(attemptStates)},
	}
	re := regexp.MustCompile(`'([^']+)'`)
	for _, tc := range cases {
		var columnType string
		if err := d.QueryRow(`
SELECT COLUMN_TYPE FROM information_schema.COLUMNS
 WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?`,
			tc.table, tc.col).Scan(&columnType); err != nil {
			t.Fatalf("%s.%s: %v", tc.table, tc.col, err)
		}
		matches := re.FindAllStringSubmatch(columnType, -1)
		if len(matches) != len(tc.want) {
			t.Errorf("%s.%s 词表长度 %d ≠ Go %d(%s)", tc.table, tc.col, len(matches), len(tc.want), columnType)
			continue
		}
		for i, m := range matches {
			if m[1] != tc.want[i] {
				t.Errorf("%s.%s 第 %d 位 = %q, want %q(中途插值/重排违反末尾追加契约)",
					tc.table, tc.col, i, m[1], tc.want[i])
			}
		}
	}
}

func stateStrs[S ~string](xs []S) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = string(x)
	}
	return out
}
