package dsl

import (
	"regexp"
	"strings"
)

// 引用形态与 schema.json $defs.reference/inputRef/nodeRef/itemRef 逐字钉死,不放宽、不模糊匹配:
// 纯引用位置必须是整个字符串恰为一个引用;tool.inputs 的字符串值内允许内嵌引用。
// item 引用({{item}} / {{item.<field>}})只在 parallel body 语境合法,语境裁决归
// 语义校验(checkRef 的 inBody 闸)与运行时求值器(实例 env 注入)——本文件只认形态。
var (
	inputRefRe = regexp.MustCompile(`^\{\{inputs\.([a-z][a-z0-9_]{0,63})\}\}$`)
	nodeRefRe  = regexp.MustCompile(`^\{\{nodes\.([a-z][a-z0-9_]{0,63})\.output\.([a-z][a-z0-9_]{0,63})\}\}$`)
	itemRefRe  = regexp.MustCompile(`^\{\{item(?:\.([a-z][a-z0-9_]{0,63}))?\}\}$`)
	// 内嵌扫描用(无锚点):{{inputs.<name>}} / {{nodes.<id>.output.<field>}} /
	// {{item}} / {{item.<field>}}(组序:1=input,2/3=node id/字段,4=item 字段可空)
	embeddedRefRe = regexp.MustCompile(`\{\{(?:inputs\.([a-z][a-z0-9_]{0,63})|nodes\.([a-z][a-z0-9_]{0,63})\.output\.([a-z][a-z0-9_]{0,63})|item(?:\.([a-z][a-z0-9_]{0,63}))?)\}\}`)
)

// Ref 是解析后的一个引用。
type Ref struct {
	Kind  string // "input" | "node" | "item"
	Name  string // input 名,或节点 id;item 引用为空
	Field string // node 引用的输出字段;item 引用的元素字段(空 = 整个元素);input 引用为空
	Text  string // 原文
}

// ParsePureRef 是 parsePureRef 的导出面:调度器求值器(T2.4)对同一钉死形态
// 的复用入口——纯引用位置的判定不允许出现第二份实现。
func ParsePureRef(s string) (Ref, bool) { return parsePureRef(s) }

// ExpandStringRefs 把 s 中每个内嵌引用替换为 resolve 返回的文本(引用按原文
// 整体替换);未命中的引用原样保留并置 miss=true。形态钉死同 scanStringRefs:
// 每个 "{{" 必须开启一个合法完整引用(schema 已拒,此处信任输入,不做 bad 检测)。
// 调用方(T2.4 求值器)决定 miss 的语义(快照求值 = 错误)。
func ExpandStringRefs(s string, resolve func(Ref) (string, bool)) (string, bool) {
	miss := false
	out := embeddedRefRe.ReplaceAllStringFunc(s, func(m string) string {
		g := embeddedRefRe.FindStringSubmatch(m)
		r := Ref{Text: m}
		switch {
		case g[1] != "":
			r.Kind, r.Name = "input", g[1]
		case g[2] != "":
			r.Kind, r.Name, r.Field = "node", g[2], g[3]
		default:
			r.Kind, r.Field = "item", g[4]
		}
		if v, ok := resolve(r); ok {
			return v
		}
		miss = true
		return m
	})
	return out, miss
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
	if m := itemRefRe.FindStringSubmatch(s); m != nil {
		return Ref{Kind: "item", Field: m[1], Text: s}, true
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
			switch {
			case m[2] >= 0:
				r.Kind, r.Name = "input", s[j+m[2]:j+m[3]]
			case m[4] >= 0:
				r.Kind, r.Name, r.Field = "node", s[j+m[4]:j+m[5]], s[j+m[6]:j+m[7]]
			default:
				r.Kind = "item"
				if m[8] >= 0 {
					r.Field = s[j+m[8] : j+m[9]]
				}
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
