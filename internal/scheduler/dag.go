package scheduler

import (
	"fmt"
	"sort"

	"braid/internal/engine/dsl"
)

// dag 是钉住 DSL 文档的拓扑视图(纯函数产物,无 DB 状态)。dsl.Parse 已做环/端点/
// 悬空校验,这里只做防御性构造(重复边去重、未知端点报错),不再重复语义校验。
// 一切遍历按 node_id 升序 = uk_node_exec(execution_id, node_id) 的索引序,
// 与 Complete 的锁序一致(确定性 + 防死锁)。
type dag struct {
	wf    *dsl.Workflow
	nodes map[string]*dsl.Node
	preds map[string][]string // node_id → 前驱集(node_id 升序去重)
	succs map[string][]string // node_id → 后继集(node_id 升序去重)
	entry []string            // 入度 0 入口(node_id 升序;多入口合法)
}

func buildDAG(wf *dsl.Workflow) (*dag, error) {
	d := &dag{
		wf:    wf,
		nodes: make(map[string]*dsl.Node, len(wf.Nodes)),
		preds: make(map[string][]string, len(wf.Nodes)),
		succs: make(map[string][]string, len(wf.Nodes)),
	}
	for i := range wf.Nodes {
		n := &wf.Nodes[i]
		if _, dup := d.nodes[n.ID]; dup {
			return nil, fmt.Errorf("scheduler: duplicate node id %q", n.ID)
		}
		d.nodes[n.ID] = n
	}
	seen := make(map[string]bool, len(wf.Edges))
	for _, e := range wf.Edges {
		if _, ok := d.nodes[e.From]; !ok {
			return nil, fmt.Errorf("scheduler: edge from unknown node %q", e.From)
		}
		if _, ok := d.nodes[e.To]; !ok {
			return nil, fmt.Errorf("scheduler: edge to unknown node %q", e.To)
		}
		if e.From == e.To {
			return nil, fmt.Errorf("scheduler: self edge on node %q", e.From)
		}
		key := e.From + "\x00" + e.To
		if seen[key] {
			continue // T1.2 已拒重复边;去重是防御
		}
		seen[key] = true
		d.succs[e.From] = append(d.succs[e.From], e.To)
		d.preds[e.To] = append(d.preds[e.To], e.From)
	}
	for id, list := range d.succs {
		sort.Strings(list)
		d.succs[id] = list
	}
	for id, list := range d.preds {
		sort.Strings(list)
		d.preds[id] = list
	}
	for _, n := range sortedNodes(d.nodes) {
		if len(d.preds[n]) == 0 {
			d.entry = append(d.entry, n)
		}
	}
	return d, nil
}

// sortedNodes 返回 dag.nodes 的 node_id 升序全列表(确定性遍历的唯一入口)。
func sortedNodes(nodes map[string]*dsl.Node) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
