package dsl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantCause 钉住每个非法样例的拒绝原因:样例必须因"目标规则"被拒,
// 而不是碰巧被别的规则拦下。新增非法样例时必须同步登记,防止覆盖静默流失。
var wantCause = map[string]string{
	"host_in_tool_inputs.json":       "propertyName 'host'",
	"prompt_not_separated.json":      "missing property 'template'",
	"unknown_node_type.json":         "match",
	"outputs_literal.json":           "pattern",
	"retry_out_of_range.json":        "maximum",
	"branch_missing_default.json":    "missing property 'default'",
	"approval_empty_approvers.json":  "minItems",
	"bad_node_id.json":               "pattern",
	"missing_dsl_field.json":         "missing property 'dsl'",
	"bad_ref_shape.json":             "pattern",
	"host_case_variant.json":         "propertyName 'Host'",
	"nested_host.json":               "propertyName 'remote_host'",
	"url_value_in_inputs.json":       "inputs/note': 'not' failed",
	"bare_ipv4_value.json":           "inputs/note': 'not' failed",
	"template_embeds_ref.json":       "prompt/template': 'not' failed",
	"approval_dup_approvers.json":    "items at 0 and 1 are equal",
	"parallel_bad_on_failure.json":   "value must be one of 'fail_fast', 'wait_all'",
	"parallel_body_branch.json":      "additional properties 'cases', 'default' not allowed",
	"parallel_foreach_node_ref.json": "does not match pattern",
	"parallel_missing_foreach.json":  "missing property 'foreach'",
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

func TestInvalidSamples(t *testing.T) {
	for _, name := range sampleFiles(t, filepath.Join("testdata", "invalid")) {
		t.Run(name, func(t *testing.T) {
			err := Validate(readSample(t, filepath.Join("testdata", "invalid", name)))
			if err == nil {
				t.Fatalf("expected schema violation, got nil")
			}
			want, ok := wantCause[name]
			if !ok {
				t.Fatalf("sample not registered in wantCause map; register it with the substring of its intended rejection reason. full error:\n%s", err.Error())
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("rejected, but not for the intended reason:\n want substring: %s\n got error:\n%s", want, err.Error())
			}
		})
	}
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
	for name := range wantCause {
		if !known[name] {
			t.Errorf("wantCause entry %q has no sample file", name)
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
