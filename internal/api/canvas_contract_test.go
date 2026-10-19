package api

// T14.1 画布跨语言契约测试(D-2026-09-30-t141-canvas-pipeline #2):
// 「画布 → DSL 导出过发布校验」验收的可执行断言——前端导出 fixtures
// (web/canvas/testdata/exports/,由 vitest 逐字节锚定 = 导出器真产物)
// 在 Go 侧走发布同口径校验(dsl.Parse + 工具白名单),红 = 画布导出
// 与发布契约漂移,两侧任一改动必须同 commit 过两道门。
//
// 零栈依赖:纯 dsl 包解析,不起 DB/Redis。目录空/缺失 = 响亮失败
// (契约面存在性本身受锚,不 skip)。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"braid/internal/engine/dsl"
)

// canvasExportFixturesGlob 从仓根定位前端导出 fixtures(测试工作目录 =
// internal/api,上行两级到仓根)。
func canvasExportFixturesGlob(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "web", "canvas", "testdata", "exports")
	entries, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", root, err)
	}
	if len(entries) == 0 {
		t.Fatalf("no canvas export fixtures under %s (contract surface missing)", root)
	}
	return entries
}

func TestCanvasExportFixturesPassPublishValidation(t *testing.T) {
	// 发布同口径:结构(schema)+ 语义校验 + 工具白名单。名单是内置
	// registry 真实名的静态快照(已知边界:registry 增删时此处需同改,
	// 偏宽/偏窄都在保存/发布真口径处兜住,响亮方向优先)。
	allowlist := []string{"file_read", "http"}

	for _, path := range canvasExportFixturesGlob(t) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !json.Valid(raw) {
				t.Fatalf("not valid JSON")
			}
			if _, err := dsl.Parse(raw, dsl.WithToolAllowlist(allowlist...)); err != nil {
				// IssuesFrom(内部 errors.As)而非裸类型断言:Parse 将来
				// 改为 wrap 错误时逐 issue 明细不降级(对抗审查 LOW)。
				if issues := dsl.IssuesFrom(err); len(issues) > 0 {
					for _, issue := range issues {
						t.Errorf("issue %s: %s", issue.Code, issue.Detail)
					}
					t.Fatalf("canvas export fixture failed publish-level validation (%d issues)", len(issues))
				}
				t.Fatalf("parse: %v", err)
			}
		})
	}
}

// TestCanvasExportFixturesCarryCanvasMeta:画布元数据在导出产物中真实
// 存在且覆盖全部节点(位置驱动编辑的往返前提;schema 词域由 dsl.Parse
// 承担,此处锚结构存在性)。
func TestCanvasExportFixturesCarryCanvasMeta(t *testing.T) {
	for _, path := range canvasExportFixturesGlob(t) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var doc struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
				Metadata struct {
					Canvas struct {
						Nodes map[string]struct {
							Position struct {
								X float64 `json:"x"`
								Y float64 `json:"y"`
							} `json:"position"`
						} `json:"nodes"`
					} `json:"canvas"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(doc.Nodes) == 0 {
				t.Fatalf("fixture has no nodes")
			}
			for _, n := range doc.Nodes {
				pos, ok := doc.Metadata.Canvas.Nodes[n.ID]
				if !ok {
					t.Errorf("node %q missing canvas position", n.ID)
					continue
				}
				// 位置为有限数(NaN/Inf 会被 JSON 编码拒绝,防御锚)。
				if pos.Position.X != pos.Position.X || pos.Position.Y != pos.Position.Y {
					t.Errorf("node %q has non-finite position", n.ID)
				}
			}
		})
	}
}

// T23.4:前端导出真产物保留部分容忍阈值,Go 发布解析不能降回未启用。
// 缺席对照同时过 schema(minimum=1),因此 0 无法冒充合法禁用字段。
func TestCanvasExportFixturesPreserveMinSuccess(t *testing.T) {
	tests := []struct {
		fixture    string
		minSuccess int
	}{
		{fixture: "parallel-min-success", minSuccess: 2},
		{fixture: "edge-sides", minSuccess: 2},
		{fixture: "all-node-types", minSuccess: 0},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "web", "canvas", "testdata", "exports", tt.fixture+".json"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			wf, err := dsl.Parse(raw, dsl.WithToolAllowlist("file_read", "http"))
			if err != nil {
				t.Fatalf("publish parse: %v", err)
			}
			for _, n := range wf.Nodes {
				if n.ID != "fan" {
					continue
				}
				if n.Parallel == nil {
					t.Fatal("fan is not a parallel node")
				}
				if n.Parallel.MinSuccess != tt.minSuccess {
					t.Errorf("min_success = %d, want %d", n.Parallel.MinSuccess, tt.minSuccess)
				}
				if n.Parallel.OnFailure != "wait_all" {
					t.Errorf("on_failure = %q, want wait_all", n.Parallel.OnFailure)
				}
				if tt.minSuccess > 0 && wf.Outputs["failed_count"] != "{{nodes.fan.output.failed_count}}" {
					t.Errorf("failed_count output contract lost: %v", wf.Outputs)
				}
				return
			}
			t.Fatal("fixture is missing fan node")
		})
	}
}

// UX-F2:同一份前端导出 fixture 必须实际承载方向与语义绑定,不能仅靠
// schema 接受新增键来宣称导出已保存;全量 fixture 测试另走发布解析口径。
func TestCanvasExportFixturesPreserveEdgeSides(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "canvas", "testdata", "exports", "edge-sides.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata struct {
			Canvas struct {
				Edges []map[string]string `json:"edges"`
			} `json:"canvas"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"from": "read_doc", "to": "parse", "source_side": "right", "target_side": "left"},
		{"from": "parse", "to": "fan", "source_side": "top", "target_side": "bottom"},
		{"from": "fan", "to": "review", "source_side": "left", "target_side": "right"},
		{"from": "review", "to": "verdict", "source_side": "right", "target_side": "left"},
		{"from": "verdict", "to": "report", "source_anchor": "case_0", "target_side": "right"},
		{"from": "verdict", "to": "revise", "target_side": "bottom"},
	}
	if !reflect.DeepEqual(doc.Metadata.Canvas.Edges, want) {
		t.Fatalf("canvas semantic anchors / directions lost or changed: got %v, want %v", doc.Metadata.Canvas.Edges, want)
	}
}
