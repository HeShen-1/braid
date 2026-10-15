// F05:asInvalidDSL 对 jsonschema 结构违例的结构化拆解——空 LLM 节点
// 首存的唯一有用事实(minLength)不得埋在 oneOf 全臂转储里。纯函数面,
// 零栈依赖(HTTP 端到端锚在 server_test.go)。
package api

import (
	"errors"
	"strings"
	"testing"

	"braid/internal/engine/dsl"
)

func TestAsInvalidDSLSchemaIssues(t *testing.T) {
	tests := []struct {
		name            string
		doc             string
		wantCount       int
		wantRefs        []string // 每个都必须出现在 ref 集合中(顺序不敏感)
		wantDetailSubstr string   // 所有 detail 拼接后必含(空 = 跳过)
		notWant         []string // 任一 issue 的 detail+ref 不得包含(oneOf 全臂转储探针)
	}{
		{
			// 审计 repro:画布新建空 LLM 节点点「保存草稿」。
			name:            "empty llm node first save: single minLength fact, no oneOf arm dump",
			doc:             `{"dsl":"1.0","name":"untitled-workflow","edges":[],"nodes":[{"id":"n1","type":"llm","title":"","prompt":{"template":""},"outputs":{}}]}`,
			wantCount:       1,
			wantRefs:        []string{"/nodes/0/prompt/template"},
			wantDetailSubstr: "minLength",
			notWant:         []string{"oneOf", "value must be", "additional propert"},
		},
		{
			// 嵌套联合(parallel.body)递归剪枝;foreach 的 anyOf 两臂
			// 都无判别式,取首臂一条可读 pattern 违例。
			name:      "nested union (parallel body) prunes recursively",
			doc:       `{"dsl":"1.0","name":"wf-one","nodes":[{"id":"p1","type":"parallel","title":"","parallel":{"foreach":"","body":{"type":"llm","prompt":{"template":""},"outputs":{}}}}]}`,
			wantCount: 2,
			wantRefs:  []string{"/nodes/0/parallel/foreach", "/nodes/0/parallel/body/prompt/template"},
			notWant:   []string{"oneOf", "'anyOf' failed"},
		},
		{
			// 判别式本身非法(未知 type):全臂都被拒,退化取叶子最少
			// 的臂,输出有界且仍不含 oneOf 噪音。
			name:      "unknown discriminator degrades to bounded single-arm output",
			doc:       `{"dsl":"1.0","name":"wf-one","nodes":[{"id":"x1","type":"widget"}]}`,
			wantCount: 2,
			wantRefs:  []string{"/nodes/0/type"},
			notWant:   []string{"oneOf"},
		},
		{
			// 根层级违例:ref 为空串(Issue.Ref 空槽位契约,前端自动省略)。
			name:            "root-level violation carries empty ref",
			doc:             `{"dsl":"1.0","nodes":[{"id":"n1","type":"llm","title":"t","prompt":{"template":"hello"},"outputs":{}}]}`,
			wantCount:       1,
			wantRefs:        []string{""},
			wantDetailSubstr: "missing property 'name'",
		},
		{
			// 非 JSON 文档:非 ValidationError,走 invalid_document 兜底
			// (既有行为不回退)。
			name:            "invalid JSON keeps single invalid_document fallback",
			doc:             `{`,
			wantCount:       1,
			wantRefs:        []string{""},
			wantDetailSubstr: "invalid JSON",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := dsl.Parse([]byte(tt.doc))
			if err == nil {
				t.Fatalf("dsl.Parse(%s) = nil error, want violation", tt.doc)
			}
			var ide *InvalidDSLError
			if !errors.As(asInvalidDSL(err), &ide) {
				t.Fatal("asInvalidDSL did not yield *InvalidDSLError")
			}
			if len(ide.Issues) != tt.wantCount {
				t.Fatalf("issues = %d (%+v), want %d", len(ide.Issues), ide.Issues, tt.wantCount)
			}
			refs := make(map[string]bool, len(ide.Issues))
			var details strings.Builder
			for _, is := range ide.Issues {
				refs[is.Ref] = true
				details.WriteString(is.Detail)
				details.WriteString("\n")
				if is.Code != "invalid_document" {
					t.Errorf("issue code = %q, want invalid_document", is.Code)
				}
			}
			for _, ref := range tt.wantRefs {
				if !refs[ref] {
					t.Errorf("refs %v missing %q", refs, ref)
				}
			}
			if tt.wantDetailSubstr != "" && !strings.Contains(details.String(), tt.wantDetailSubstr) {
				t.Errorf("details %q missing substring %q", details.String(), tt.wantDetailSubstr)
			}
			for _, noise := range tt.notWant {
				if strings.Contains(details.String(), noise) {
					t.Errorf("details contain noise %q:\n%s", noise, details.String())
				}
			}
		})
	}
}

// TestAsInvalidDSLSemanticPassthrough:语义发现(ParseError)不经结构
// 拆解,原样透传(码/节点/引用字段保持引擎原貌)。
func TestAsInvalidDSLSemanticPassthrough(t *testing.T) {
	_, err := dsl.Parse([]byte(`{"dsl":"1.0","name":"wf-one","edges":[{"from":"a","to":"ghost"}],"nodes":[{"id":"a","type":"llm","title":"t","prompt":{"template":"hello"},"outputs":{}}]}`))
	if err == nil {
		t.Fatal("dsl.Parse = nil error, want semantic violation")
	}
	var ide *InvalidDSLError
	if !errors.As(asInvalidDSL(err), &ide) {
		t.Fatal("asInvalidDSL did not yield *InvalidDSLError")
	}
	if len(ide.Issues) != 1 || ide.Issues[0].Code != dsl.IssueEdgeEndpoint {
		t.Fatalf("issues = %+v, want single %s passthrough", ide.Issues, dsl.IssueEdgeEndpoint)
	}
	if ide.Issues[0].Node != "a" || ide.Issues[0].Ref != "ghost" {
		t.Fatalf("issue = %+v, want node=a ref=ghost untouched", ide.Issues[0])
	}
}
