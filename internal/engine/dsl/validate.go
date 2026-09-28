package dsl

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// validate 执行 T1.2 全部语义校验,聚合返回(不 fail-fast,作者可一次修完):
// 环检测、悬空引用、引用类型、引用必须是祖先节点、inputs/user_data/节点 id 重名、
// branch 出边与 cases[].next ∪ {default} 一致、工具白名单。
// 两处显式豁免(与 testdata 语料一致,均已在代码处注明理由):
//   - 工作流级 outputs 不做祖先约束——分支剪枝后静态支配关系不可判定;
//   - user_data 标签名不与 input/节点 id 做跨命名空间查重——标签不进引用语法。
func (wf *Workflow) validate(cfg parseConfig) []Issue {
	v := &validator{
		wf:       wf,
		cfg:      cfg,
		inputs:   map[string]string{},
		nodes:    map[string]*Node{},
		outs:     map[string]map[string]string{},
		adj:      map[string][]string{},
		radj:     map[string][]string{},
		ancCache: map[string]map[string]bool{},
	}
	v.checkInputs()
	v.checkNodes()
	v.checkEdges()
	v.checkCycles()
	v.checkNodeRefs()
	v.checkWorkflowOutputs()
	return v.issues
}

type validator struct {
	wf     *Workflow
	cfg    parseConfig
	issues []Issue

	inputs   map[string]string            // input 名 → 类型
	nodes    map[string]*Node             // 节点 id → 节点(首个)
	outs     map[string]map[string]string // 节点 id → 输出字段 → 类型(branch 无条目)
	adj      map[string][]string          // 正向邻接(端点合法、去重后,保持声明序)
	radj     map[string][]string          // 反向邻接
	ancCache map[string]map[string]bool   // 节点 → 祖先集合(懒算,不含自身)
	inBody   bool                         // 正在校验 parallel body:item 引用语境闸(body 不可嵌套,无需栈)
}

func (v *validator) add(code, node, ref, detail string) {
	v.issues = append(v.issues, Issue{Code: code, Node: node, Ref: ref, Detail: detail})
}

func (v *validator) checkInputs() {
	for _, in := range v.wf.Inputs {
		if _, dup := v.inputs[in.Name]; dup {
			v.add(IssueDupInput, "", in.Name, "duplicate workflow input name")
			continue
		}
		v.inputs[in.Name] = in.Type
	}
}

func (v *validator) checkNodes() {
	for i := range v.wf.Nodes {
		n := &v.wf.Nodes[i]
		if _, dup := v.nodes[n.ID]; dup {
			v.add(IssueDupNodeID, n.ID, "", "duplicate node id")
			continue
		}
		v.nodes[n.ID] = n
		switch {
		case n.Parallel != nil:
			// 输出契约固定(T9.1 D-2026-09-28-t91):results = body 输出对象的数组
			// (gather 输入序)。不可作者声明——下游引用面以此单源校验。
			v.outs[n.ID] = map[string]string{"results": "array"}
		case n.Branch != nil:
			// branch 无输出
		default:
			v.outs[n.ID] = nodeOutputTypes(n)
		}
	}
	// input 名与节点 id 共享顶层命名空间,必须两两不相交。
	for _, in := range v.wf.Inputs {
		if _, clash := v.nodes[in.Name]; clash {
			v.add(IssueNameCollision, in.Name, "", "name used both as workflow input and node id")
		}
	}
}

func nodeOutputTypes(n *Node) map[string]string {
	switch {
	case n.LLM != nil:
		return n.LLM.Outputs
	case n.Tool != nil:
		return n.Tool.Outputs
	case n.Approval != nil:
		return n.Approval.Outputs
	}
	return nil
}

func (v *validator) checkEdges() {
	seen := map[string]bool{}
	for _, e := range v.wf.Edges {
		if _, ok := v.nodes[e.From]; !ok {
			v.add(IssueEdgeEndpoint, e.From, e.To, fmt.Sprintf("edge from unknown node %q", e.From))
			continue
		}
		if _, ok := v.nodes[e.To]; !ok {
			v.add(IssueEdgeEndpoint, e.From, e.To, fmt.Sprintf("edge to unknown node %q", e.To))
			continue
		}
		key := e.From + "\x00" + e.To
		if seen[key] {
			v.add(IssueDuplicateEdge, e.From, e.To, "duplicate edge")
			continue
		}
		seen[key] = true
		v.adj[e.From] = append(v.adj[e.From], e.To)
		v.radj[e.To] = append(v.radj[e.To], e.From)
	}
}

// checkCycles 三色 DFS,逐回边报告,错误信息含完整环路径。
// 按节点声明序起扫,保证报告确定性。
func (v *validator) checkCycles() {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(v.nodes))
	var path []string
	var dfs func(id string)
	dfs = func(id string) {
		color[id] = gray
		path = append(path, id)
		for _, next := range v.adj[id] {
			switch color[next] {
			case gray:
				start := 0
				for i, p := range path {
					if p == next {
						start = i
						break
					}
				}
				cycle := append(append([]string{}, path[start:]...), next)
				v.add(IssueCycle, next, "", "control flow cycle: "+strings.Join(cycle, " -> "))
			case white:
				dfs(next)
			}
		}
		color[id] = black
		path = path[:len(path)-1]
	}
	for i := range v.wf.Nodes {
		if color[v.wf.Nodes[i].ID] == white {
			dfs(v.wf.Nodes[i].ID)
		}
	}
}

func (v *validator) checkNodeRefs() {
	for i := range v.wf.Nodes {
		n := &v.wf.Nodes[i]
		switch {
		case n.LLM != nil:
			v.checkLLM(n.ID, n.LLM)
		case n.Tool != nil:
			v.checkTool(n.ID, n.Tool)
		case n.Branch != nil:
			v.checkBranch(n)
		case n.Approval != nil:
			v.checkApproval(n)
		case n.Parallel != nil:
			v.checkParallel(n)
		}
	}
}

// approverRe / reservedApprovers 镜像 username 词域(internal/auth/users.go
// usernameRe + reservedUsernames 同则;DDL chk_user_username 双闸先例)。
// 镜像而非 import auth:DSL 语言核心不耦合账号域;三方(auth Go 侧 / DDL /
// 此处)逐字一致由 TestApproverDomainPinnedToDDL 锚死。
var (
	approverRe        = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)
	reservedApprovers = map[string]bool{"system": true}
)

// checkApproval 校验 approvers[] 逐项对齐 username 词域(T4.4 必带件,
// D-t42 §9-②):名单是执行时审批权真相源,域外项(非 username 形态)
// 发布合法但运行面永无人能命中——审批永 403 死面,GET 详情也无人能读
// (T5.2 收窄后读面同名单)。schema ^\S+$ 放行的形态在此拒收;词域内
// 但尚无账号/角色的主体是运行面事实(账号可后建),不属发布期判据。
func (v *validator) checkApproval(n *Node) {
	for i, a := range n.Approval.Approvers {
		if reservedApprovers[a] || !approverRe.MatchString(a) {
			v.add(IssueBadApprover, n.ID, a,
				fmt.Sprintf("approvers[%d] %q is not a valid username (must match %s, reserved %s; out-of-domain approvers can never read or decide)",
					i, a, approverRe.String(), reservedApproverList()))
		}
	}
}

// reservedApproverList 是保留字集的确定性展示(词表排序;消息文本随
// reservedApprovers 单源,防字面量漂移——对抗审计 LOW)。
func reservedApproverList() string {
	rs := make([]string, 0, len(reservedApprovers))
	for r := range reservedApprovers {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	return strings.Join(rs, ",")
}

// checkParallel 校验 parallel 节点(T9.1,D-2026-09-28-t91):foreach 必须是指向
// 声明 type=array 输入的纯引用(schema 钉 inputRef 形态,此处钉语义);body 限
// llm|tool(schema 已拦,防御兜底),其引用按 parallel 节点位置参与祖先约束,
// item 引用在 body 语境放开(inBody 闸)。签名取 nodeID + 具体指针:body 复用
// checkLLM/checkTool 时以父节点 id 归因。
func (v *validator) checkParallel(n *Node) {
	p := n.Parallel
	if r, ok := parsePureRef(p.Foreach); ok && r.Kind == "input" {
		if t, cont := v.checkRef(n.ID, r, "parallel.foreach"); cont && t != "array" {
			v.add(IssueBadForeach, n.ID, r.Text,
				fmt.Sprintf("parallel.foreach input %q is declared %s, want array", r.Name, t))
		}
	} else {
		v.add(IssueBadForeach, n.ID, p.Foreach,
			"parallel.foreach must be a pure {{inputs.<name>}} reference")
	}
	if p.Body.LLM == nil && p.Body.Tool == nil {
		v.add(IssueBadForeach, n.ID, "", "parallel.body must be an llm or tool node template")
		return
	}
	v.inBody = true
	defer func() { v.inBody = false }()
	if p.Body.LLM != nil {
		v.checkLLM(n.ID, p.Body.LLM)
	}
	if p.Body.Tool != nil {
		v.checkTool(n.ID, p.Body.Tool)
	}
}

func (v *validator) checkLLM(nodeID string, l *LLMNode) {
	seen := map[string]bool{}
	for _, ud := range l.Prompt.UserData {
		if seen[ud.Name] {
			v.add(IssueDupUserData, nodeID, ud.Name, "duplicate user_data name in one prompt")
			continue
		}
		seen[ud.Name] = true
		r, ok := parsePureRef(ud.Ref)
		if !ok {
			// schema 已钉死纯引用形态;此处为防御(跳过 schema 直调 Parse 时兜底)。
			v.add(IssueBadRef, nodeID, ud.Ref, "user_data.ref is not a well-formed pure reference")
			continue
		}
		v.checkRef(nodeID, r, "user_data "+ud.Name)
	}
}

func (v *validator) checkTool(nodeID string, t *ToolNode) {
	if v.cfg.toolAllowlist != nil && !v.cfg.toolAllowlist[t.Tool] {
		v.add(IssueToolNotAllowed, nodeID, t.Tool,
			fmt.Sprintf("tool %q is not in the publish allowlist", t.Tool))
	}
	keys := make([]string, 0, len(t.Inputs))
	for k := range t.Inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v.walkToolValue(nodeID, k, t.Inputs[k])
	}
}

// walkToolValue 递归扫描 tool.inputs 值:字符串里的引用(纯/内嵌)全部解析校验,
// 非法 {{...}} 形态记 bad_ref。键名黑名单与 URL/IPv4 禁令归 schema,此处不重复。
func (v *validator) walkToolValue(nodeID, key string, raw json.RawMessage) {
	var val any
	if err := json.Unmarshal(raw, &val); err != nil {
		v.add(IssueBadRef, nodeID, key, "undecodable tool input value")
		return
	}
	v.walkToolAny(nodeID, key, val)
}

func (v *validator) walkToolAny(nodeID, path string, val any) {
	switch x := val.(type) {
	case string:
		refs, bad := scanStringRefs(x)
		for _, b := range bad {
			v.add(IssueBadRef, nodeID, b, fmt.Sprintf("malformed reference in tool.inputs %q", path))
		}
		for _, r := range refs {
			v.checkRef(nodeID, r, "tool.inputs "+path)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v.walkToolAny(nodeID, path+"."+k, x[k])
		}
	case []any:
		for i, el := range x {
			v.walkToolAny(nodeID, fmt.Sprintf("%s[%d]", path, i), el)
		}
	}
}

func (v *validator) checkBranch(n *Node) {
	b := n.Branch
	// 路由目标存在性
	targets := map[string]bool{}
	for i, c := range b.Cases {
		if _, ok := v.nodes[c.Next]; !ok {
			v.add(IssueDanglingRef, n.ID, c.Next, fmt.Sprintf("cases[%d].next references unknown node", i))
			continue
		}
		targets[c.Next] = true
	}
	if _, ok := v.nodes[b.Default]; !ok {
		v.add(IssueDanglingRef, n.ID, b.Default, "default references unknown node")
	} else {
		targets[b.Default] = true
	}
	// 出边集合与 cases[].next ∪ {default} 目标集合一致(双向)
	edges := map[string]bool{}
	for _, to := range v.adj[n.ID] {
		edges[to] = true
		if !targets[to] {
			v.add(IssueBranchEdges, n.ID, to,
				fmt.Sprintf("edge to %q is not routable by any cases[].next or default", to))
		}
	}
	for i, c := range b.Cases {
		if _, ok := v.nodes[c.Next]; ok && !edges[c.Next] {
			v.add(IssueBranchEdges, n.ID, c.Next,
				fmt.Sprintf("cases[%d].next target has no edge from branch node", i))
		}
	}
	if _, ok := v.nodes[b.Default]; ok && !edges[b.Default] {
		v.add(IssueBranchEdges, n.ID, b.Default, "default target has no edge from branch node")
	}
	// 谓词类型检查
	for i, c := range b.Cases {
		v.checkCondition(n.ID, i, c.When)
	}
}

// checkCondition 校验 cases[i].when 的左引用/操作符/右值类型相容性。
// 左侧为纯引用;右侧为字面量或纯引用字符串(引用按声明类型比较)。
func (v *validator) checkCondition(nodeID string, idx int, cond Condition) {
	where := fmt.Sprintf("cases[%d].when", idx)
	lr, ok := parsePureRef(cond.Left)
	if !ok {
		v.add(IssueBadRef, nodeID, cond.Left, where+".left is not a well-formed pure reference")
		return
	}
	leftType, ok := v.checkRef(nodeID, lr, where+".left")
	if !ok {
		return
	}

	rightType := ""
	hasRight := len(cond.Right) > 0
	if hasRight {
		var rv any
		if err := json.Unmarshal(cond.Right, &rv); err != nil {
			v.add(IssueBadRef, nodeID, string(cond.Right), where+".right is undecodable")
			return
		}
		if s, isStr := rv.(string); isStr {
			if rr, isRef := parsePureRef(s); isRef {
				rightType, ok = v.checkRef(nodeID, rr, where+".right")
				if !ok {
					return
				}
			} else {
				rightType = "string"
			}
		} else {
			rightType = jsonTypeName(rv)
		}
	}

	mismatch := func() {
		v.add(IssueTypeMismatch, nodeID, cond.Left,
			fmt.Sprintf("%s: %s %s is not type-compatible", where, leftType, opDisplay(cond.Op, hasRight, rightType)))
	}
	needRight := func() {
		v.add(IssueTypeMismatch, nodeID, cond.Left, where+": op "+cond.Op+" requires a right operand")
	}

	switch cond.Op {
	case "exists":
		// 右值无语义,忽略
	case "eq", "ne":
		if !hasRight {
			needRight()
			return
		}
		if rightType == "null" || rightType != leftType {
			mismatch()
		}
	case "gt", "gte", "lt", "lte":
		if !hasRight {
			needRight()
			return
		}
		bothNumbers := leftType == "number" && rightType == "number"
		bothStrings := leftType == "string" && rightType == "string"
		if !bothNumbers && !bothStrings {
			mismatch()
		}
	case "contains":
		if !hasRight {
			needRight()
			return
		}
		switch leftType {
		case "string":
			if rightType != "string" {
				mismatch()
			}
		case "array":
			if rightType == "null" {
				mismatch()
			}
		default:
			mismatch()
		}
	}
}

func opDisplay(op string, hasRight bool, rightType string) string {
	if !hasRight {
		return op + " <missing>"
	}
	return op + " " + rightType
}

// checkRef 解析并校验一个引用:存在性、输出字段声明、(节点内发起的)祖先约束。
// nodeID 为空表示工作流层级发起(如 outputs),豁免祖先约束。
// 返回引用的声明类型与是否可继续参与类型检查。
func (v *validator) checkRef(nodeID string, r Ref, where string) (string, bool) {
	switch r.Kind {
	case "input":
		t, ok := v.inputs[r.Name]
		if !ok {
			v.add(IssueDanglingRef, nodeID, r.Text,
				fmt.Sprintf("%s references undeclared workflow input %q", where, r.Name))
			return "", false
		}
		return t, true
	case "item":
		// item 引用只在 parallel body 语境合法(元素类型是运行时数据,静态不可判;
		// body 限 llm|tool,无谓词类型面,不需要类型返回)。
		if !v.inBody {
			v.add(IssueBadRef, nodeID, r.Text,
				fmt.Sprintf("%s uses an item reference outside a parallel body", where))
			return "", false
		}
		return "unknown", true
	case "node":
		target, ok := v.nodes[r.Name]
		if !ok {
			v.add(IssueDanglingRef, nodeID, r.Text,
				fmt.Sprintf("%s references unknown node %q", where, r.Name))
			return "", false
		}
		if target.Branch != nil {
			v.add(IssueDanglingRef, nodeID, r.Text,
				fmt.Sprintf("%s references output of branch node %q, which declares no outputs", where, r.Name))
			return "", false
		}
		t, ok := v.outs[r.Name][r.Field]
		if !ok {
			v.add(IssueDanglingRef, nodeID, r.Text,
				fmt.Sprintf("%s references output field %q not declared by node %q", where, r.Field, r.Name))
			return "", false
		}
		if nodeID != "" && !v.isAncestor(r.Name, nodeID) {
			v.add(IssueNotAncestor, nodeID, r.Text,
				fmt.Sprintf("%s references node %q which is not an ancestor (no control-flow path %s -> %s)",
					where, r.Name, r.Name, nodeID))
		}
		return t, true
	}
	return "", false
}

func (v *validator) checkWorkflowOutputs() {
	names := make([]string, 0, len(v.wf.Outputs))
	for name := range v.wf.Outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r, ok := parsePureRef(v.wf.Outputs[name])
		if !ok {
			v.add(IssueBadRef, "", v.wf.Outputs[name],
				fmt.Sprintf("workflow output %q is not a well-formed pure reference", name))
			continue
		}
		v.checkRef("", r, "workflow output "+name)
	}
}

// isAncestor 报告是否存在控制流路径 x -> ... -> of(不含 x == of)。
func (v *validator) isAncestor(x, of string) bool {
	anc, ok := v.ancCache[of]
	if !ok {
		stack := []string{of}
		visited := map[string]bool{of: true}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range v.radj[cur] {
				if !visited[p] {
					visited[p] = true
					stack = append(stack, p)
				}
			}
		}
		delete(visited, of)
		v.ancCache[of] = visited
		anc = visited
	}
	return anc[x]
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}
