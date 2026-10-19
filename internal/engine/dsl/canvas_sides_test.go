package dsl

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// UX-F2:画布方向是显示元数据,与 case/default 语义锚点正交。合法词域
// 四向组合必须全部放行,非法值必须因准确字段的规则被拒,封闭对象仍生效。
func TestCanvasEdgeSides(t *testing.T) {
	sample := readSample(t, filepath.Join("testdata", "valid", "canvas_multi_anchor.json"))
	validate := func(t *testing.T, fields map[string]any, wantField string) {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal(sample, &doc); err != nil {
			t.Fatal(err)
		}
		canvas := doc["metadata"].(map[string]any)["canvas"].(map[string]any)
		edge := canvas["edges"].([]any)[0].(map[string]any)
		for key, value := range fields {
			edge[key] = value
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		err = Validate(raw)
		if wantField == "" {
			if err != nil {
				t.Fatalf("legal sides rejected: %v", err)
			}
			return
		}
		if err == nil {
			t.Fatalf("invalid field %s was accepted", wantField)
		}
		path := "/metadata/canvas/edges/0"
		cause := "additional properties"
		if wantField != "unknown" {
			path += "/" + wantField
			cause = "value must be one of"
			if _, isString := fields[wantField].(string); !isString {
				cause = "want string"
			}
			if wantField == "source_anchor" {
				cause = "does not match pattern"
			}
		}
		if !wantMatch(err.Error(), []string{path, cause}) {
			t.Fatalf("rejected for wrong cause; want %s %s, got %v", path, cause, err)
		}
	}
	sides := []string{"top", "right", "bottom", "left"}
	for _, source := range sides {
		for _, target := range sides {
			t.Run(source+"_to_"+target, func(t *testing.T) {
				validate(t, map[string]any{"source_side": source, "target_side": target}, "")
			})
		}
	}
	for _, field := range []string{"source_side", "target_side"} {
		for _, value := range []any{"diagonal", "", "RIGHT", "source_right", "case_0", nil, 1, map[string]any{}} {
			t.Run(field+"_reject_"+strings.ReplaceAll(fmt.Sprint(value), " ", "_"), func(t *testing.T) {
				// JSON Schema 先拒非字符串的类型;字符串再钉四向 enum,
				// 两者都必须在准确的方向字段上报错。
				validate(t, map[string]any{field: value}, field)
			})
		}
	}
	t.Run("direction_does_not_overload_source_anchor", func(t *testing.T) {
		validate(t, map[string]any{"source_anchor": "right", "target_side": "left"}, "source_anchor")
	})
	t.Run("record_stays_closed", func(t *testing.T) {
		validate(t, map[string]any{"target_side": "left", "target_point": map[string]any{"x": 1, "y": 2}}, "unknown")
	})
}
