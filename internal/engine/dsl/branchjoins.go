package dsl

import "fmt"

// checkBranchJoins 找出不同互斥出口可达的汇合点。普通并行分叉不受限，
// 多个 case 指向同一出口也不算互斥。每个节点每个 branch 最多传播两次：
// 第一次记录出口归属，第二个不同归属标记共享，后续归属无需重复传播。
func (v *validator) checkBranchJoins() {
	for _, branch := range v.wf.Nodes {
		if branch.Branch == nil {
			continue
		}
		owners := map[string]string{}
		shared := map[string]bool{}
		for _, arm := range v.adj[branch.ID] {
			stack := []string{arm}
			for len(stack) > 0 {
				id := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if shared[id] {
					continue
				}
				if owner, seen := owners[id]; seen {
					if owner == arm {
						continue
					}
					shared[id] = true
				} else {
					owners[id] = arm
				}
				stack = append(stack, v.adj[id]...)
			}
		}
		// 只报告有多个入边的汇合点，不逐条重复报告其单前驱后继。
		for _, node := range v.wf.Nodes {
			if shared[node.ID] && len(v.radj[node.ID]) > 1 {
				v.add(IssueBranchJoin, node.ID, branch.ID,
					fmt.Sprintf("node %q joins mutually exclusive paths from branch %q; a skipped predecessor skips this node and its descendants; place required approvals inside each branch", node.ID, branch.ID))
			}
		}
	}
}
