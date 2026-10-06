package dsl

import "sort"

// NodeDataDeps 返回 nodeID 输入面的直接节点依赖集(升序确定性序;T19.5,
// D-2026-10-06-t195-parallel-incremental-recovery 的快照依赖门消费面)。
//
// 语义 = 「该节点的 input_snapshot 是谁的函数」:foreach 来源节点引用(parallel)
// + 模板/入参里的 {{nodes.<id>.output.*}} 引用;parallel body 的 {{item}} 引用
// 归并到 foreach 来源(元素与其字段同源)。inputs 引用零贡献(执行输入不可变,
// 回滚重跑不改变量)。事实收集单源复用 collectNodeTaint 的 preds(taint 的
// 直接前驱集与本口径同构——第二份实现 = 漂移面,拒绝)。
//
// 防御口径同 ComputeTaintSets:发布面 schema/语义校验上游已拒的形态(未知
// 节点引用、body 类型失配)此处零贡献或零抽取,调用方按保守方向消费
// (依赖集不可信时宁可不保活)。
func NodeDataDeps(wf *Workflow, nodeID string) []string {
	for i := range wf.Nodes {
		if wf.Nodes[i].ID != nodeID {
			continue
		}
		nt := &nodeTaint{paths: map[string]struct{}{}, preds: map[string]struct{}{}}
		collectNodeTaint(&wf.Nodes[i], nt, nil)
		if len(nt.preds) == 0 {
			return nil
		}
		out := make([]string, 0, len(nt.preds))
		for id := range nt.preds {
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	return nil
}
