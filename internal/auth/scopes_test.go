package auth

import (
	"errors"
	"testing"
	"time"
)

// TestAllScopesVocabPinned 钉死作用域词表与声明序(D-2026-09-30-t131
// 决策 3 单源 + D-2026-10-01-t171 REST 执法扩表:消费面只认这五枚,
// 追加 = 本文件 + 映射表同 PR 旅行)。
func TestAllScopesVocabPinned(t *testing.T) {
	got := AllScopes()
	want := []Scope{ScopeWorkflowsRead, ScopeWorkflowsWrite, ScopeExecutionsRead, ScopeExecutionsWrite, ScopeApprovalsWrite}
	if len(got) != len(want) {
		t.Fatalf("AllScopes() = %v, want %v (len %d != %d)", got, want, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllScopes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAllScopesDefensiveCopy:返回快照,调用方改写不得回灌词表
// (镜像 AllRoles 防御性拷贝先例)。
func TestAllScopesDefensiveCopy(t *testing.T) {
	got := AllScopes()
	got[0] = "tampered:scope"
	if AllScopes()[0] != ScopeWorkflowsRead {
		t.Fatalf("vocab mutated via returned slice: %v", AllScopes())
	}
}

// TestValidScopes:词表内通过;词表外/重复拒绝;空集与 nil 合法——
// 零权限 token 是合法形态(scp 缺席 = 存量 token 兼容面),与 roles
// 非空约束刻意不同:角色是身份下限,作用域是能力上限,上限可为空。
func TestValidScopes(t *testing.T) {
	cases := []struct {
		name    string
		in      []Scope
		wantErr bool
	}{
		{"full vocab", []Scope{ScopeWorkflowsRead, ScopeExecutionsRead, ScopeExecutionsWrite}, false},
		{"single", []Scope{ScopeExecutionsWrite}, false},
		{"empty slice ok", []Scope{}, false},
		{"nil ok", nil, false},
		{"unknown scope", []Scope{ScopeWorkflowsRead, "audit:read"}, true},
		{"bare word", []Scope{"read"}, true},
		{"duplicate", []Scope{ScopeExecutionsRead, ScopeExecutionsRead}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validScopes(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("validScopes(%v) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validScopes(%v) = %v, want nil", tc.in, err)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validScopes(%v) err = %v, want ErrInvalidInput", tc.in, err)
			}
		})
	}
}

// TestDefaultScopesForRoles:角色默认映射钉死(admin|author 五枚全量,
// approver 三枚 = 读两枚 + 审批写一枚——approver 决定面 REST 现状兼容
// 刚需;多角色并集)。login 签发默认与 /auth/tokens 自窄化上界同吃这张
// 表(单源)。
func TestDefaultScopesForRoles(t *testing.T) {
	full := []Scope{ScopeWorkflowsRead, ScopeWorkflowsWrite, ScopeExecutionsRead, ScopeExecutionsWrite, ScopeApprovalsWrite}
	cases := []struct {
		name  string
		roles []Role
		want  []Scope
	}{
		{"admin full", []Role{RoleAdmin}, full},
		{"author full", []Role{RoleAuthor}, full},
		{"approver read+decide", []Role{RoleApprover}, []Scope{ScopeWorkflowsRead, ScopeExecutionsRead, ScopeApprovalsWrite}},
		{"author+approver union", []Role{RoleAuthor, RoleApprover}, full},
		{"no roles empty", nil, []Scope{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultScopesForRoles(tc.roles)
			if len(got) != len(tc.want) {
				t.Fatalf("DefaultScopesForRoles(%v) = %v, want %v", tc.roles, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("DefaultScopesForRoles(%v)[%d] = %q, want %q", tc.roles, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSignTokenScopedRoundTrip:scp 与 bnd 同携往返;保序逐项等;
// 显式 scopes 为签名面输入(签不出词表外 token,前置拒绝镜像 validRoles)。
func TestSignTokenScopedRoundTrip(t *testing.T) {
	scopes := []Scope{ScopeWorkflowsRead, ScopeExecutionsWrite}
	tok, err := SignTokenScoped(testSecret, time.Hour, "alice", []Role{RoleAuthor}, 7, "AAAAAAAAAAAAAAAAAAAAAA", scopes)
	if err != nil {
		t.Fatalf("SignTokenScoped: %v", err)
	}
	c, err := ParseToken(testSecret, tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if len(c.Scopes) != 2 || c.Scopes[0] != ScopeWorkflowsRead || c.Scopes[1] != ScopeExecutionsWrite {
		t.Fatalf("scopes = %v, want [workflows:read executions:write]", c.Scopes)
	}
	if c.Tenant != 7 {
		t.Fatalf("tnt = %d, want 7", c.Tenant)
	}
	if c.Binding != "AAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("bnd = %q, want carried binding", c.Binding)
	}
}

// TestSignTokenScopedRejectsBadInput:词表外 scopes / 重复 → 签发面
// 前置拒绝(不产出消费面不可信的 token);超 MaxTokenTTL 拒(吊销上界
// 在唯一导出签发原语处自洽,镜像 SignTokenWithBinding)。
func TestSignTokenScopedRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		scopes []Scope
		ttl    time.Duration
	}{
		{"unknown scope", []Scope{"nope:read"}, time.Hour},
		{"duplicate scope", []Scope{ScopeWorkflowsRead, ScopeWorkflowsRead}, time.Hour},
		{"ttl over revocation bound", nil, MaxTokenTTL + time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SignTokenScoped(testSecret, tc.ttl, "alice", []Role{RoleAuthor}, 0, "", tc.scopes)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("SignTokenScoped err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestSignTokenSugarNoScopesCompat:存量签发函数(SignToken /
// SignTokenWithBinding)语义冻结——不带 scp claim;解析合法且零作用域
// (存量 token 在 MCP 面 fail-closed 零权限,D-t131 决策 3)。
func TestSignTokenSugarNoScopesCompat(t *testing.T) {
	tok, err := SignTokenWithBinding(testSecret, time.Hour, "alice", []Role{RoleAdmin}, 0, "")
	if err != nil {
		t.Fatalf("SignTokenWithBinding: %v", err)
	}
	c, err := ParseToken(testSecret, tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if len(c.Scopes) != 0 {
		t.Fatalf("scopes = %v, want empty (sugar signers stay scope-free)", c.Scopes)
	}
	if err := RequireScope(c, ScopeWorkflowsRead); !errors.Is(err, ErrForbidden) {
		t.Fatalf("RequireScope on scope-free token = %v, want ErrForbidden", err)
	}
}

// TestParseTokenScopesFailClosed:正确密钥签发但携词表外/重复 scp →
// 解析拒绝(镜像 bnd 格式闸:防线针对签发面 bug + 消费面信任载荷)。
func TestParseTokenScopesFailClosed(t *testing.T) {
	for _, scp := range [][]Scope{
		{"audit:read"},
		{ScopeWorkflowsRead, ScopeWorkflowsRead},
	} {
		c := validClaims("alice", []Role{RoleAuthor})
		c.Scopes = scp
		tok, err := signClaims(testSecret, c)
		if err != nil {
			t.Fatalf("signClaims: %v", err)
		}
		if _, err := ParseToken(testSecret, tok); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("ParseToken(scp=%v) err = %v, want ErrTokenInvalid", scp, err)
		}
	}
}

// TestRequireScope:持有 nil / 未持有 ErrForbidden(附所需 scope 与现有
// 集)/ want 词表外 ErrInvalidInput(调用方 bug 前置拒绝,镜像 RequireRole)。
func TestRequireScope(t *testing.T) {
	c := validClaims("alice", []Role{RoleAuthor})
	c.Scopes = []Scope{ScopeWorkflowsRead}

	if err := RequireScope(c, ScopeWorkflowsRead); err != nil {
		t.Fatalf("RequireScope(held) = %v, want nil", err)
	}
	err := RequireScope(c, ScopeExecutionsWrite)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("RequireScope(unheld) = %v, want ErrForbidden", err)
	}
	if err := RequireScope(c, "audit:read"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("RequireScope(unknown want) = %v, want ErrInvalidInput", err)
	}
}

// TestScopesWithin:子集判定逐项包含(want 空 = 恒真;want ⊄ allowed =
// 假)。词表/重复校验归调用方前置,此处只比集合包含(签发自窄化上界
// 判定消费,T17.1 自 api 包收敛入 auth 与词表同源——D-t131 登记⑤收编)。
func TestScopesWithin(t *testing.T) {
	cases := []struct {
		name   string
		want   []Scope
		allow  []Scope
		expect bool
	}{
		{"subset", []Scope{ScopeWorkflowsRead}, []Scope{ScopeWorkflowsRead, ScopeExecutionsRead}, true},
		{"equal set", []Scope{ScopeExecutionsWrite}, []Scope{ScopeExecutionsWrite}, true},
		{"empty want always true", []Scope{}, []Scope{ScopeWorkflowsRead}, true},
		{"missing one", []Scope{ScopeWorkflowsRead, ScopeApprovalsWrite}, []Scope{ScopeWorkflowsRead}, false},
		{"disjoint", []Scope{ScopeExecutionsWrite}, []Scope{ScopeWorkflowsRead}, false},
		{"allowed empty", []Scope{ScopeWorkflowsRead}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScopesWithin(tc.want, tc.allow); got != tc.expect {
				t.Fatalf("ScopesWithin(%v, %v) = %v, want %v", tc.want, tc.allow, got, tc.expect)
			}
		})
	}
}

// TestIntersectScopes:交集保 a 序(输出序 = 声明序,词表单源序可预期);
// 空交集 = 空上界(非 nil 防御面由调用方判 len)。
func TestIntersectScopes(t *testing.T) {
	a := []Scope{ScopeWorkflowsRead, ScopeExecutionsWrite, ScopeApprovalsWrite}
	b := []Scope{ScopeApprovalsWrite, ScopeWorkflowsRead}
	got := IntersectScopes(a, b)
	if len(got) != 2 || got[0] != ScopeWorkflowsRead || got[1] != ScopeApprovalsWrite {
		t.Fatalf("IntersectScopes(%v, %v) = %v, want [workflows:read approvals:write] (保 a 序)", a, b, got)
	}
	if got := IntersectScopes(a, []Scope{ScopeExecutionsRead}); len(got) != 0 {
		t.Fatalf("IntersectScopes disjoint = %v, want empty", got)
	}
}
