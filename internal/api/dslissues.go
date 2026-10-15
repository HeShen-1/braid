// JSON Schema 结构违例的结构化拆解(F05):校验器对 oneOf/anyOf 联合的
// 失败携带全臂 cause 树(Error() 整树缩进转储 25+ 行),对画布作者只有
// 噪音。schemaIssues 沿 cause 树下钻:联合节点只保留与文档判别式一致的
// 臂(节点族按 type const/enum 判别,type 被拒的臂只证明判别式不匹配,
// 不描述本文档),叶子按 InstanceLocation 折成一条 Issue——ref 即 JSON
// 指针,落前端 IssuesPanel 既有「字段」槽位,DTO 零新增词汇。
package api

import (
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"braid/internal/engine/dsl"
)

// schemaPrinter 固定英文:issue detail 是契约面,不随进程 locale 漂移
// (jsonschema 自身同款包级 printer 模式)。
var schemaPrinter = message.NewPrinter(language.English)

// schemaIssues 把一次 jsonschema 校验失败的 cause 树拆成逐位置的 issue
// 序列:结构化事实(哪个字段、违反哪条)保留,联合全臂转储剪掉。
func schemaIssues(ve *jsonschema.ValidationError) []dsl.Issue {
	var issues []dsl.Issue
	var walk func(*jsonschema.ValidationError)
	walk = func(ve *jsonschema.ValidationError) {
		if isUnion(ve.ErrorKind) && len(ve.Causes) > 0 {
			if arm := bestUnionArm(ve); arm != nil {
				walk(arm)
				return
			}
		}
		if len(ve.Causes) == 0 {
			issues = append(issues, dsl.Issue{
				Code:   "invalid_document",
				Ref:    instanceRef(ve),
				Detail: ve.ErrorKind.LocalizedString(schemaPrinter),
			})
			return
		}
		for _, cause := range ve.Causes {
			walk(cause)
		}
	}
	walk(ve)
	return issues
}

func isUnion(k jsonschema.ErrorKind) bool {
	switch k.(type) {
	case *kind.OneOf, *kind.AnyOf:
		return true
	}
	return false
}

// bestUnionArm 从失败臂中挑一条继续下钻:优先「type 未被拒」的臂(其
// 判别式已被文档接受,剩余叶子才是本文档的问题);全臂 type 均被拒
// (判别式本身非法,如未知 type)时退化取叶子最少的臂,保底有界输出。
func bestUnionArm(ve *jsonschema.ValidationError) *jsonschema.ValidationError {
	var fallback *jsonschema.ValidationError
	fallbackLeaves := 0
	for _, arm := range ve.Causes {
		leaves := validationLeaves(arm)
		if !rejectsType(ve.InstanceLocation, leaves) {
			return arm
		}
		if fallback == nil || len(leaves) < fallbackLeaves {
			fallback, fallbackLeaves = arm, len(leaves)
		}
	}
	return fallback
}

// validationLeaves 收集子树叶子(无 cause 的校验错误)。
func validationLeaves(ve *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(ve.Causes) == 0 {
		return []*jsonschema.ValidationError{ve}
	}
	var leaves []*jsonschema.ValidationError
	for _, cause := range ve.Causes {
		leaves = append(leaves, validationLeaves(cause)...)
	}
	return leaves
}

// rejectsType 报告叶子集里是否含联合判别式拒绝:位置恰为「<联合>/type」
// 的 const/enum 违例(节点族按 type 判别,见 dsl schema.json)。
func rejectsType(unionLoc []string, leaves []*jsonschema.ValidationError) bool {
	disc := append(slices.Clip(unionLoc), "type")
	for _, leaf := range leaves {
		if !slices.Equal(leaf.InstanceLocation, disc) {
			continue
		}
		switch leaf.ErrorKind.(type) {
		case *kind.Const, *kind.Enum:
			return true
		}
	}
	return false
}

// instanceRef 把实例位置折成 JSON 指针形态(Issue.Ref 契约);根层级为空
// (前端对空 ref/node 槽位自动省略)。
func instanceRef(ve *jsonschema.ValidationError) string {
	if len(ve.InstanceLocation) == 0 {
		return ""
	}
	return "/" + strings.Join(ve.InstanceLocation, "/")
}
