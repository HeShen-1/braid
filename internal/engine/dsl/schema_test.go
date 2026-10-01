package dsl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantCause 钉住每个非法样例的拒绝原因:样例必须因"目标规则"被拒,
// 而不是碰巧被别的规则拦下。新增非法样例时必须同步登记,防止覆盖静默流失。
// 值是"同一行内必须齐含"的子串集(wantMatch):错误输出是缩进多行树,
// 跨行拼子串会命中无关错误位;jsonschema v6 聚合 additionalProperties 属性名
// 按实例 map 迭代序拼接、逐次序随机,凡钉多属性消息必须拆成逐属性子串,
// 禁止钉死一种次序(#128)。
var wantCause = map[string][]string{
	"host_in_tool_inputs.json":       {"propertyName 'host'"},
	"prompt_not_separated.json":      {"missing property 'template'"},
	"unknown_node_type.json":         {"match"},
	"outputs_literal.json":           {"pattern"},
	"retry_out_of_range.json":        {"maximum"},
	"branch_missing_default.json":    {"missing property 'default'"},
	"approval_empty_approvers.json":  {"minItems"},
	"bad_node_id.json":               {"pattern"},
	"missing_dsl_field.json":         {"missing property 'dsl'"},
	"bad_ref_shape.json":             {"pattern"},
	"host_case_variant.json":         {"propertyName 'Host'"},
	"nested_host.json":               {"propertyName 'remote_host'"},
	"url_value_in_inputs.json":       {"inputs/note': 'not' failed"},
	"bare_ipv4_value.json":           {"inputs/note': 'not' failed"},
	"template_embeds_ref.json":       {"prompt/template': 'not' failed"},
	"approval_dup_approvers.json":    {"items at 0 and 1 are equal"},
	"parallel_bad_on_failure.json":   {"value must be one of 'fail_fast', 'wait_all'"},
	"parallel_body_branch.json":      {"at '/nodes/0/parallel/body'", "additional properties", "'cases'", "'default'"},
	"parallel_foreach_node_ref.json": {"does not match pattern"},
	"parallel_missing_foreach.json":  {"missing property 'foreach'"},
}

// T1.1 验收:≥5 合法样例全部通过,≥5 非法样例全部被拒且原因正确。
func TestValidSamples(t *testing.T) {
	for _, name := range sampleFiles(t, filepath.Join("testdata", "valid")) {
		t.Run(name, func(t *testing.T) {
			if err := Validate(readSample(t, filepath.Join("testdata", "valid", name))); err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestWantCauseOrderInsensitive 钉死 #128 断言次序 flake 的回归:jsonschema v6
// 聚合 additionalProperties 属性名按实例 map 迭代序拼接,逐次序随机。
// parallel_body_branch 的子串集必须在两种次序下都命中,且不得被分布在
// 错误树不同行的碎片拼出假通过——行锚定(wantMatch)的存在理由。
func TestWantCauseOrderInsensitive(t *testing.T) {
	want := wantCause["parallel_body_branch.json"]
	const shape = "- at '/nodes/0/parallel/body': additional properties %s not allowed"
	for _, order := range []string{"'cases', 'default'", "'default', 'cases'"} {
		line := fmt.Sprintf(shape, order)
		if !wantMatch(line, want) {
			t.Errorf("order %s: want substrings %v do not match line %q", order, want, line)
		}
	}
	decoy := "- at '/nodes/0/parallel/body': 'oneOf' failed, none matched\n" +
		"- at '/nodes/0': missing properties 'cases', 'default'\n" +
		"- at '/nodes/0': additional properties 'parallel' not allowed"
	if wantMatch(decoy, want) {
		t.Errorf("want substrings %v matched fragments spread across lines:\n%s", want, decoy)
	}
}

func TestInvalidSamples(t *testing.T) {
	for _, name := range sampleFiles(t, filepath.Join("testdata", "invalid")) {
		t.Run(name, func(t *testing.T) {
			err := Validate(readSample(t, filepath.Join("testdata", "invalid", name)))
			if err == nil {
				t.Fatalf("expected schema violation, got nil")
			}
			want, ok := wantCause[name]
			if !ok {
				t.Fatalf("sample not registered in wantCause map; register it with the substrings of its intended rejection reason. full error:\n%s", err.Error())
			}
			if !wantMatch(err.Error(), want) {
				t.Fatalf("rejected, but not for the intended reason:\n want substrings (one line): %v\n got error:\n%s", want, err.Error())
			}
		})
	}
}

// wantMatch 报告错误文本中是否存在某一行齐含 want 的全部子串。
// 空 want 恒不匹配:防"登记了条目但没写子串"静默恒过(同 Covers 防流失立意)。
func wantMatch(errStr string, want []string) bool {
	if len(want) == 0 {
		return false
	}
	for _, line := range strings.Split(errStr, "\n") {
		all := true
		for _, sub := range want {
			if !strings.Contains(line, sub) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// 每个 wantCause 条目都必须对应一个真实样例文件,防止映射与样例漂移。
func TestWantCauseCoversAllInvalidSamples(t *testing.T) {
	files := sampleFiles(t, filepath.Join("testdata", "invalid"))
	for _, name := range files {
		if _, ok := wantCause[name]; !ok {
			t.Errorf("testdata/invalid/%s has no wantCause entry", name)
		}
	}
	known := make(map[string]bool, len(files))
	for _, name := range files {
		known[name] = true
	}
	for name, want := range wantCause {
		if !known[name] {
			t.Errorf("wantCause entry %q has no sample file", name)
		}
		if len(want) == 0 {
			t.Errorf("wantCause entry %q has empty substring set (would always pass)", name)
		}
	}
}

// 守住样例数量下限,防止后续维护中悄悄流失覆盖。
func TestSampleCounts(t *testing.T) {
	if n := len(sampleFiles(t, filepath.Join("testdata", "valid"))); n < 5 {
		t.Errorf("valid samples = %d, want >= 5", n)
	}
	if n := len(sampleFiles(t, filepath.Join("testdata", "invalid"))); n < 5 {
		t.Errorf("invalid samples = %d, want >= 5", n)
	}
}

func sampleFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no .json samples in %s", dir)
	}
	return names
}

func readSample(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
