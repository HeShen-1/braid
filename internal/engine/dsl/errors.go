package dsl

import (
	"errors"
	"strings"
)

// 语义校验发现码。稳定不变,调用方与测试按码断言。
const (
	IssueCycle          = "cycle"            // 控制流有环
	IssueEdgeEndpoint   = "edge_endpoint"    // 边指向不存在的节点
	IssueDuplicateEdge  = "duplicate_edge"   // 重复边
	IssueDupNodeID      = "dup_node_id"      // 节点 id 重复
	IssueDupInput       = "dup_input"        // 工作流 input 名重复
	IssueDupUserData    = "dup_user_data"    // 同一 prompt 内 user_data 名重复
	IssueNameCollision  = "name_collision"   // input 名与节点 id 重名
	IssueDanglingRef    = "dangling_ref"     // 引用不存在的 input/节点/输出字段
	IssueBadRef         = "bad_ref"          // tool.inputs 值内出现形态不合法的 {{...}}
	IssueNotAncestor    = "not_ancestor"     // 节点内引用了非祖先节点
	IssueBranchEdges    = "branch_edges"     // branch 出边与 cases[].next ∪ {default} 不一致
	IssueTypeMismatch   = "type_mismatch"    // branch.when 左右操作数/操作符类型不匹配
	IssueToolNotAllowed = "tool_not_allowed" // 工具不在发布白名单内
	IssueBadApprover    = "bad_approver"     // approvers[] 含 username 词域外名单(403 死面)
)

// Issue 是一条语义校验发现。
type Issue struct {
	Code   string // 上面的 Issue* 常量
	Node   string // 发现所在节点 id;工作流层级为空
	Ref    string // 涉及的引用或目标原文(可空)
	Detail string // 人读细节
}

func (i Issue) Error() string {
	var b strings.Builder
	b.WriteString("dsl: ")
	b.WriteString(i.Code)
	if i.Node != "" {
		b.WriteString(" [node=")
		b.WriteString(i.Node)
		b.WriteString("]")
	}
	if i.Ref != "" {
		b.WriteString(" [ref=")
		b.WriteString(i.Ref)
		b.WriteString("]")
	}
	b.WriteString(": ")
	b.WriteString(i.Detail)
	return b.String()
}

// ParseError 聚合一次解析的全部语义发现(不 fail-fast,便于作者一次修完)。
type ParseError struct{ Issues []Issue }

func (e *ParseError) Error() string {
	msgs := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		msgs[i] = is.Error()
	}
	return strings.Join(msgs, "; ")
}

// IssuesFrom 从错误链中取出语义发现;非 ParseError 返回 nil。
func IssuesFrom(err error) []Issue {
	var pe *ParseError
	if errors.As(err, &pe) {
		return pe.Issues
	}
	return nil
}

// HasIssue 报告 err 中是否含指定码的发现。
func HasIssue(err error, code string) bool {
	for _, is := range IssuesFrom(err) {
		if is.Code == code {
			return true
		}
	}
	return false
}
