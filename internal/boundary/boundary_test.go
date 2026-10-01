// Package boundary 是 engine/enterprise 分层划线的执法门(T16.1,
// D-2026-10-01-t161-engine-enterprise-split):
//
//   - R1 core-no-enterprise:internal/enterprise/ 子树之外的包(enterprise
//     自身与 cmd/ 组合根除外;含测试导入)不得 import braid/internal/
//     enterprise/*——分层是单向的(enterprise → core),反向即塌线。
//   - R2 engine 纯净:internal/engine 族(含 dsl)包级 Imports 零内部依赖
//     (Q7 底座纯净从既有事实锁成机械不变量);测试导入豁免——
//     sqltest/redistest 是测试基建,不构成运行期依赖。
//
// 实现为 go list 包图断言(真源包图,非文件名匹配);depguard
// (.golangci.yml)是 lint 腿的快速反馈面,本测试是精确网——两者同 PR
// 旅行,规则口径变更须双侧同步。
package boundary

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// pkg 是 go list -json 的消费子集。
type pkg struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	out, err := exec.Command("go", "list", "-json", "braid/internal/...", "braid/cmd/...").Output()
	if err != nil {
		t.Fatalf("boundary: go list: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var pkgs []pkg
	for {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("boundary: decode go list json: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// isCore 报告包是否属于核心面:enterprise 子树(实现侧)与 cmd/(组合根,
// 允许双向装配)豁免,其余全辖。
func isCore(importPath string) bool {
	return !strings.HasPrefix(importPath, "braid/internal/enterprise") &&
		!strings.HasPrefix(importPath, "braid/cmd/")
}

func TestCoreDoesNotImportEnterprise(t *testing.T) {
	for _, p := range listPackages(t) {
		if !isCore(p.ImportPath) {
			continue
		}
		groups := []struct {
			what string
			imps []string
		}{
			{"imports", p.Imports},
			{"test imports", p.TestImports},
			{"xtest imports", p.XTestImports},
		}
		for _, g := range groups {
			for _, imp := range g.imps {
				if strings.HasPrefix(imp, "braid/internal/enterprise") {
					t.Errorf("%s %s %s: core must not import enterprise (one-way boundary: enterprise -> core; cmd/ composition roots exempt)",
						p.ImportPath, g.what, imp)
				}
			}
		}
	}
}

func TestEnginePurity(t *testing.T) {
	for _, p := range listPackages(t) {
		if !strings.HasPrefix(p.ImportPath, "braid/internal/engine") {
			continue
		}
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, "braid/internal/") && !strings.HasPrefix(imp, "braid/internal/engine") {
				t.Errorf("%s imports %s: engine family must stay zero-internal-deps (Q7 core base; test imports exempt by design)",
					p.ImportPath, imp)
			}
		}
	}
}

// TestEnterpriseTreePresent 防 R1 空转:企业子树被整体删除/改名时,
// core-no-enterprise 恒真——门静默失守比没有门更糟(空安全边界先例)。
func TestEnterpriseTreePresent(t *testing.T) {
	n := 0
	for _, p := range listPackages(t) {
		if strings.HasPrefix(p.ImportPath, "braid/internal/enterprise") {
			n++
		}
	}
	if n == 0 {
		t.Fatal("no packages under braid/internal/enterprise: R1 boundary rule is vacuous (tree deleted or renamed?)")
	}
}
