package dsl

// T4.4 必带件(D-t42 §9-② 移交):approval 节点 approvers[] 发布期词域
// 校验。域外名单(非 username 形态)在 schema(^\S+$)是合法的,但运行
// 面永无人能命中——username 词域 ^[a-z][a-z0-9_-]{0,127}$ + 保留字
// system(internal/auth/users.go usernameRe 同则),域外名单 = 审批永
// 403 死面(GET 详情也无人能读),故语义校验期拒收、聚合报告。
// 词域镜像而非 import auth:语言核心不耦合账号域;四方一致(auth Go 侧 /
// DDL chk_user_username / schema items 词域 / 此处)由
// TestApproverDomainPinnedToDDL 逐腿锚死(对抗审计:原注释只锚 DDL 一腿
// 却宣称三方一致——auth Go 腿已补)。
//
// 变异自查:词域判定删除 → 全部负例红;保留字分支删除 → system 用例红;
// 正则上界缩到 64 → len_100 Parse 面红(schema 已放宽 128);fail-fast 化
// → 聚合计数红;schema maxLength 收回 64 → pin 腿红。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"strings"
	"testing"
)

// approvalDoc 拼一份仅 approvers 可变的合法 approval 文档(schema 全过,
// 语义发现只剩词域面——负例断言码集合精确相等的前提)。
func approvalDoc(approvers ...string) string {
	b, err := json.Marshal(approvers)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{
  "dsl": "1.0", "name": "approver-domain",
  "nodes": [
    {"id": "review", "type": "approval", "approvers": %s}
  ],
  "edges": []
}`, b)
}

func TestParseApproverDomainIssues(t *testing.T) {
	tests := []struct {
		name      string
		approvers []string
		wantBad   bool // 是否期望发现码集合恰为 {bad_approver}
	}{
		// —— 词域内(schema ^\S+$ 放行 + username 词域放行)——
		{"plain", []string{"alice"}, false},
		{"multi", []string{"carol", "bob-2", "d_e"}, false},
		// 65..128 区间:schema(128)与词域(128)对齐后 Parse 面可达——
		// 语义层上界误抄 64 在此红(对抗审计:对齐前该区间被 schema 64
		// 截断,词域承诺只在 ≤64 成立)。
		{"len_100_domain_legal", []string{"a" + strings.Repeat("b", 99)}, false},

		// —— 词域外(schema 放行、语义拒收)——
		{"uppercase", []string{"Alice"}, true},
		{"inner_dot", []string{"a.b"}, true},
		{"email", []string{"carol@example.com"}, true},
		{"unicode", []string{"张三"}, true},
		{"leading_digit", []string{"1alice"}, true},
		{"leading_hyphen", []string{"-alice"}, true},
		{"leading_underscore", []string{"_alice"}, true},
		{"reserved_system", []string{"system"}, true},
		// 名单混入:仅域外项被报告,域内项不连坐。
		{"mixed", []string{"alice", "Alice"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(approvalDoc(tt.approvers...)))
			if !tt.wantBad {
				if err != nil {
					t.Fatalf("合法名单被拒: %v", err)
				}
				return
			}
			got := issueCodesOf(t, err)
			if strings.Join(got, ",") != IssueBadApprover {
				t.Fatalf("发现码 = %v, want 恰 [%s]", got, IssueBadApprover)
			}
		})
	}
}

// TestParseApproverDomainAggregated:聚合并报(不 fail-fast)——同一名单
// 两个域外项 ⇒ 两条 bad_approver;Ref 携域外原文;跨节点同样聚合
// (两个 approval 节点各带一个域外项 ⇒ 恰两条,对抗审计补锚)。
func TestParseApproverDomainAggregated(t *testing.T) {
	t.Run("same_node", func(t *testing.T) {
		_, err := Parse([]byte(approvalDoc("alice", "Alice", "Bob", "carol")))
		if err == nil {
			t.Fatalf("域外名单未被报告")
		}
		var hits []Issue
		for _, is := range IssuesFrom(err) {
			if is.Code == IssueBadApprover {
				hits = append(hits, is)
			}
		}
		if len(hits) != 2 {
			t.Fatalf("bad_approver 条数 = %d, want 2(聚合不 fail-fast): %v", len(hits), IssuesFrom(err))
		}
		wantRefs := map[string]bool{"Alice": false, "Bob": false}
		for _, h := range hits {
			if _, ok := wantRefs[h.Ref]; !ok {
				t.Fatalf("意外 Ref %q", h.Ref)
			}
			wantRefs[h.Ref] = true
		}
		for ref, seen := range wantRefs {
			if !seen {
				t.Fatalf("Ref %q 未报告", ref)
			}
		}
	})
	t.Run("across_nodes", func(t *testing.T) {
		doc := `{
  "dsl": "1.0", "name": "approver-domain-multi",
  "nodes": [
    {"id": "review1", "type": "approval", "approvers": ["Alice"]},
    {"id": "review2", "type": "approval", "approvers": ["Bob"]}
  ],
  "edges": []
}`
		_, err := Parse([]byte(doc))
		if err == nil {
			t.Fatalf("跨节点域外名单未被报告")
		}
		n, nodes := 0, map[string]bool{}
		for _, is := range IssuesFrom(err) {
			if is.Code == IssueBadApprover {
				n++
				nodes[is.Node] = true
			}
		}
		if n != 2 || !nodes["review1"] || !nodes["review2"] {
			t.Fatalf("跨节点聚合 = %d 条 %v, want 各节点一条", n, nodes)
		}
	})
}

// TestApproverDomainDirectValidate:越过 schema 的防御深度(直调 validate;
// 129 字符被 schema 128 先拦,语义层词域 128 界自身必须正确——直接锚,
// 防 schema 与语义层一起漂时无声漏过)。
func TestApproverDomainDirectValidate(t *testing.T) {
	doc := fmt.Sprintf(`{"dsl":"1.0","name":"d","nodes":[{"id":"review","type":"approval","approvers":[%q]}]}`,
		"a"+strings.Repeat("b", 128))
	var wf Workflow
	if err := json.Unmarshal([]byte(doc), &wf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !HasIssue(&ParseError{Issues: wf.validate(parseConfig{})}, IssueBadApprover) {
		t.Fatalf("129 字符名单未在语义层拒收(词域上界漂移?)")
	}
}

// TestApproverDomainPinnedToDDL:词域镜像的漂移锚,逐腿比对——①DDL
// chk_user_username;②auth Go 侧 usernameRe/reservedUsernames(读源文件
// 抠字面,不 import);③schema.json approvers items 的长度界(=128,与
// users.username VARCHAR(128) 同值域;对齐前是 64,把词域承诺截断在
// ≤64 ——对抗审计 MEDIUM);④dsl 侧 approverRe/reservedApprovers。
// 任何一腿单独漂移即红。
func TestApproverDomainPinnedToDDL(t *testing.T) {
	const wantPattern = `^[a-z][a-z0-9_-]{0,127}$`

	// 腿①:DDL。
	raw, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "0002_auth_users.up.sql"))
	if err != nil {
		t.Fatalf("read DDL: %v", err)
	}
	ddl := string(raw)
	if !strings.Contains(ddl, "REGEXP_LIKE(username, '"+wantPattern+"')") {
		t.Fatalf("DDL 词域漂移:chk_user_username 不再含 %q", wantPattern)
	}
	if !strings.Contains(ddl, "username <> 'system'") {
		t.Fatalf("DDL 保留字闸漂移:chk_user_username 不再拒绝 system")
	}

	// 腿②:auth Go 侧(源文件字面锚;重命名/搬移 = 漂移信号,更新此锚)。
	authSrc, err := os.ReadFile(filepath.Join("..", "..", "auth", "users.go"))
	if err != nil {
		t.Fatalf("read auth source: %v", err)
	}
	wantAuthLine := "usernameRe = regexp.MustCompile(`" + wantPattern + "`)"
	if !strings.Contains(string(authSrc), wantAuthLine) {
		t.Fatalf("auth usernameRe 漂移:users.go 不再含 %q", wantAuthLine)
	}
	if !strings.Contains(string(authSrc), `"system": true`) {
		t.Fatalf("auth 保留字集漂移:reservedUsernames 不再含 system")
	}

	// 腿③:schema.json approvers items 长度界。
	schemaSrc, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	if !strings.Contains(string(schemaSrc),
		`"pattern": "^\\S+$", "minLength": 1, "maxLength": 128`) {
		t.Fatalf("schema approvers 长度界漂移:不再与 username 词域 128 对齐")
	}

	// 腿④:dsl 侧本体。
	if got := approverRe.String(); got != wantPattern {
		t.Fatalf("dsl approver 词域 = %q, want %q(与 DDL/auth 逐字一致)", got, wantPattern)
	}
	if !reservedApprovers["system"] || len(reservedApprovers) != 1 {
		t.Fatalf("dsl 保留字集 = %v, want 恰 {system}(与 DDL/auth 逐字一致)", reservedApprovers)
	}
}
