package dsl

import (
	"regexp"
	"strings"
)

// 引用形态与 schema.json $defs.reference/inputRef/nodeRef 逐字钉死,不放宽、不模糊匹配:
// 纯引用位置必须是整个字符串恰为一个引用;tool.inputs 的字符串值内允许内嵌引用。
var (
	inputRefRe = regexp.MustCompile(`^\{\{inputs\.([a-z][a-z0-9_]{0,63})\}\}$`)
	nodeRefRe  = regexp.MustCompile(`^\{\{nodes\.([a-z][a-z0-9_]{0,63})\.output\.([a-z][a-z0-9_]{0,63})\}\}$`)
	// 内嵌扫描用(无锚点):{{inputs.<name>}} 或 {{nodes.<id>.output.<field>}}
	embeddedRefRe = regexp.MustCompile(`\{\{(?:inputs\.([a-z][a-z0-9_]{0,63})|nodes\.([a-z][a-z0-9_]{0,63})\.output\.([a-z][a-z0-9_]{0,63}))\}\}`)
)

// Ref 是解析后的一个引用。
type Ref struct {
	Kind  string // "input" | "node"
	Name  string // input 名,或节点 id
	Field string // node 引用的输出字段;input 引用为空
	Text  string // 原文
}

// parsePureRef 解析纯引用位置(user_data.ref、branch.when.left/right、workflow outputs)。
// 整个字符串必须恰为一个引用;不匹配返回 false。
func parsePureRef(s string) (Ref, bool) {
	if m := inputRefRe.FindStringSubmatch(s); m != nil {
		return Ref{Kind: "input", Name: m[1], Text: s}, true
	}
	if m := nodeRefRe.FindStringSubmatch(s); m != nil {
		return Ref{Kind: "node", Name: m[1], Field: m[2], Text: s}, true
	}
	return Ref{}, false
}

// scanStringRefs 扫描 tool.inputs 字符串值中出现的全部引用。
// 引用形态钉死的延伸规则:字符串里每个 "{{" 都必须紧接着一个合法且完整的引用,
// 否则记为 bad(防止作者以为会替换、运行时却按字面量发出的隐性 bug)。
func scanStringRefs(s string) (refs []Ref, bad []string) {
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "{{")
		if j < 0 {
			break
		}
		j += i
		m := embeddedRefRe.FindStringSubmatchIndex(s[j:])
		if m != nil && m[0] == 0 { // 合法引用恰从 j 开始
			r := Ref{Text: s[j : j+m[1]]}
			if g := m[2]; g >= 0 {
				r.Kind, r.Name = "input", s[j+g : j+m[3]]
			} else {
				r.Kind, r.Name, r.Field = "node", s[j+m[4]:j+m[5]], s[j+m[6]:j+m[7]]
			}
			refs = append(refs, r)
			i = j + m[1]
			continue
		}
		bad = append(bad, badSnippet(s, j))
		i = j + 2 // 跳过这个 "{{",继续扫后面
	}
	return refs, bad
}

// badSnippet 截取从 pos 起的片段用于错误信息,遇 "}}" 或 32 字符截断。
func badSnippet(s string, pos int) string {
	end := pos + 32
	if end > len(s) {
		end = len(s)
	}
	if k := strings.Index(s[pos:end], "}}"); k >= 0 {
		end = pos + k + 2
	}
	return s[pos:end]
}
