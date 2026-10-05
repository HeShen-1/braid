package dsl

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
)

// 静态污点分析(T18.3,D-2026-10-05-t183-taint-tracking):污点源 = 工作流触发
// 输入,单位 = `inputs.<声明名>` 路径;沿 DSL 引用图保守闭包传播——节点引用了
// 任一污点源(直接 inputs 引用,或经 {{nodes.X.output.*}} / parallel item 引用
// 消费了带污点节点的输出)⇒ 该节点全部输出整染。保守过近似是安全追踪口径:
// 只求无双漏报,不求区分输出字段(细分 = 值级,PII 红线否决,D-t183 决策 1)。
//
// 结构性保证:引用形态被 refs.go 正则钉死为单段标识符(嵌套触发值只能整体经
// 声明名引用),路径宇宙 ⊆ DSL 声明 inputs 词表——调用方可控字节不可达,
// 零运行时过滤(D-t183 决策 3 的词表开放面依据)。
//
// 本分析是钉版定义的纯函数:同版本 DSL 恒同图,不落库(D-t183 决策 2),发射点
// (scheduler)进程内现算。分析不依赖 schema/语义校验(上游已拒的形态在此
// 防御性容忍:未声明 inputs 名照录、未知节点引用零贡献——校验口径归
// validate.go,此处只描述引用事实)。

// TaintSets 是静态污点分析的只读视图:仅收录路径集非空的节点。
type TaintSets struct {
	nodes map[string][]string
}

// Sources 返回 nodeID 的传递源路径集(升序确定性序);nil = 无污点或未知节点。
// 返回防御拷贝(导出面纪律:内部切片不外别名)。
func (t TaintSets) Sources(nodeID string) []string {
	src := t.nodes[nodeID]
	if src == nil {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// taintPathRe 钉死污点路径形态:inputs.<声明名>,名段字符集与 refs.go
// inputRefRe 逐字同源(单段 ^[a-z][a-z0-9_]{0,63}$)。audit 边界形状闸经
// ValidTaintPath 消费本单源。
var taintPathRe = regexp.MustCompile(`^inputs\.[a-z][a-z0-9_]{0,63}$`)

// ValidTaintPath 判 s 是否合法污点路径(audit 层 PII 红线形状闸的消费面)。
func ValidTaintPath(s string) bool { return taintPathRe.MatchString(s) }

// MaxTaintPaths 是单事件路径条数上界(与 schema inputs maxItems=64 对齐;
// 传递集 ⊆ 引用到的声明名全集,天然不越此界,上界是防御闸不是收缩)。
const MaxTaintPaths = 64

// nodeTaint 是不动点求解的中间形态:paths = 直引输入路径,preds = 被引用
// 节点(整染传播——输出字段不细分,引用任一字段即继承全量源集)。
type nodeTaint struct {
	paths map[string]struct{}
	preds map[string]struct{}
}

func (nt *nodeTaint) addPath(p string) { nt.paths[p] = struct{}{} }
func (nt *nodeTaint) addPred(id string) {
	if id != "" {
		nt.preds[id] = struct{}{}
	}
}

// itemSource 是 parallel 语境里 item 引用的解析靶:foreach 数组的来源
// (输入路径或节点依赖,恰一非空)。
type itemSource struct {
	path string
	pred string
}

// ComputeTaintSets 对钉版 DSL 文档计算 per-node 传递源路径集。
// 图无环由 T1.2 语义校验保证;不动点迭代在无环前提下必收敛,防御口径下
// (校验被跳过、图带环)至多按环上全集收敛,不发散、不死循环。
func ComputeTaintSets(wf *Workflow) TaintSets {
	raw := make(map[string]*nodeTaint, len(wf.Nodes))
	for i := range wf.Nodes {
		n := &wf.Nodes[i]
		nt := &nodeTaint{paths: map[string]struct{}{}, preds: map[string]struct{}{}}
		collectNodeTaint(n, nt, nil)
		raw[n.ID] = nt
	}
	// 不动点:preds 的可达路径并回自身,直至稳定。
	for changed := true; changed; {
		changed = false
		for _, nt := range raw {
			for pred := range nt.preds {
				p, ok := raw[pred]
				if !ok {
					continue // 未知节点引用:零贡献(防御)
				}
				for path := range p.paths {
					if _, dup := nt.paths[path]; !dup {
						nt.paths[path] = struct{}{}
						changed = true
					}
				}
			}
		}
	}
	out := make(map[string][]string, len(raw))
	for id, nt := range raw {
		if len(nt.paths) == 0 {
			continue
		}
		paths := make([]string, 0, len(nt.paths))
		for p := range nt.paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		out[id] = paths
	}
	return TaintSets{nodes: out}
}

// collectNodeTaint 抽取单个节点的直引事实到 nt。itemRes 非 nil 时为 parallel
// body 语境:item 引用归并到 foreach 来源(整染——数组元素与其字段同源)。
func collectNodeTaint(n *Node, nt *nodeTaint, itemRes *itemSource) {
	apply := func(r Ref) {
		switch r.Kind {
		case "input":
			nt.addPath("inputs." + r.Name)
		case "node":
			nt.addPred(r.Name)
		case "item":
			if itemRes != nil {
				if itemRes.path != "" {
					nt.addPath(itemRes.path)
				}
				if itemRes.pred != "" {
					nt.addPred(itemRes.pred)
				}
			}
		}
	}
	applyScanned := func(s string) {
		refs, _ := scanStringRefs(s) // bad 形态归校验面,此处零关注
		for _, r := range refs {
			apply(r)
		}
	}
	switch n.Type {
	case "llm":
		if n.LLM == nil {
			return // 手工构造的类型载荷失配:防御(发布面 schema 必不一致)
		}
		for _, ud := range n.LLM.Prompt.UserData {
			if r, ok := parsePureRef(ud.Ref); ok {
				apply(r)
			}
		}
	case "tool":
		if n.Tool == nil {
			return
		}
		for _, v := range n.Tool.Inputs {
			walkJSONStrings(v, applyScanned)
		}
	case "branch":
		if n.Branch == nil {
			return
		}
		for _, c := range n.Branch.Cases {
			if r, ok := parsePureRef(c.When.Left); ok {
				apply(r)
			}
			// 注:Right 为内嵌(非纯)引用串时运行时按字面量比较不解析——
			// 此处扫描会产生保守方向的假阳性路径(过近似,无双漏报,D-t183)。
			walkJSONStrings(c.When.Right, applyScanned)
		}
	// approval 无数据引用面:input_snapshot = 静态声明面(approvers/mode/
	// timeout/on_timeout/instructions,buildSnapshot 单源),outputs 值是类型
	// 声明枚举非引用——结构性恒净,不进污点模型(D-t183 决策 1 修正口径)。
	case "parallel":
		if n.Parallel == nil {
			return
		}
		var res itemSource
		if fr, ok := parsePureRef(n.Parallel.Foreach); ok {
			switch fr.Kind {
			case "input":
				res.path = "inputs." + fr.Name
			case "node":
				res.pred = fr.Name
			}
			apply(fr) // 数组本体也流经本节点
		}
		// body 是 llm|tool 单节点模板(schema 钉死;ID 恒空),引用并入父节点;
		// 防御口径下 body 为其他类型时零抽取。
		if n.Parallel.Body.LLM != nil {
			collectNodeTaint(&Node{Type: "llm", LLM: n.Parallel.Body.LLM}, nt, &res)
		}
		if n.Parallel.Body.Tool != nil {
			collectNodeTaint(&Node{Type: "tool", Tool: n.Parallel.Body.Tool}, nt, &res)
		}
	}
}

// walkJSONStrings 递归走 JSON 树,对每个字符串叶子回调(tool.inputs 值可为
// 字面量/纯引用/内嵌引用字符串,亦可嵌套数组对象;非字符串标量零关注)。
// UseNumber 解码:越界数字字面量(如 1e999)在 any+float64 路径会让整棵树
// Unmarshal 失败、引用收集静默归零(fail-open 漏报,对抗审计修)——json.Number
// 逐字保留使解码永不因数值域失败(eval.go unmarshalUseNumber 同款口径)。
func walkJSONStrings(raw json.RawMessage, fn func(string)) {
	var v any
	if err := unmarshalUseNumber(raw, &v); err != nil {
		return // 非法 JSON:校验面上游已拒,此处零贡献
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			fn(x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
}

// unmarshalUseNumber 是 UseNumber 解码的本地等价(eval.go 同名私有不可跨包,
// 口径注释见彼处;大整数/越界数逐字往返)。
func unmarshalUseNumber(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}
