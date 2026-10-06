package scheduler

// T19.5(D-2026-10-06-t195-parallel-incremental-recovery)parallel 增量恢复:
// 回滚闭包重置对 parallel 节点按「单实例粒度」保活——SUCCEEDED 实例行(及其
// output/finished_at/attempts)经门控后跳过 ResetNodeToPending,重派只 claim
// 非 SUCCEEDED 行,gather 收敛面原样收割旧输出(results 合并新旧)。
//
// 门控三件(任一不过 = 整行集重跑,即 T19.5 前的现行为):
//  1. 意图门:显式 target 落在 SUCCEEDED 控制器上 = 再生意图(「重跑可能产出
//     不同结果」,D-t193 决策 3 同 rationale)——保活会使 SUCCEEDED 节点的
//     显式重跑塌缩成 no-op。自动定位恢复点恒不命中(退化链只走 FAILED →
//     CANCELLED → 前驱就绪 PENDING)。
//  2. 依赖门:节点快照数据依赖(foreach 来源 + body 引用,钉版 DSL 静态提取,
//     dsl.NodeDataDeps 单源)∩ 闭包 == ∅ ⇒ 输入不变,旧输出仍真;有交集 ⇒
//     前驱重跑可能改输出,旧快照对不上新输入,保守整行集重跑。结构性推论:
//     动态扇出的 shrink/grow 必经 foreach 来源重跑 ⇒ 来源必在闭包 ⇒ 依赖门
//     已关 ⇒ 保活行永不进 reconcile 裁剪面(reconcileFanout 的盈余断言口径
//     对保活行免检的根据)。
//  3. 行集原子性:控制行 SUCCEEDED ⇒ 整行集保活(部分容忍的 FAILED/CANCELLED
//     证据行零重置——results 已物化,重置 = 无人驱动的悬死 PENDING);控制行
//     重置(FAILED/CANCELLED,必经重派)⇒ 仅 SUCCEEDED 实例行保活。「保活
//     控制行 + 重置实例」结构性禁止:实例 claim 只发生在 dispatchParallel 内。
//     whole-keep 追加控制依赖门(对抗审计 HIGH 修):闭包内含 branch 型祖先
//     ⇒ 路由可能改变,整行集重跑(见 noBranchAncestorInClosure);
//
// 与实例收口/ReapInstanceExhausted 的交叉:被 reap 耗尽的实例行 = FAILED ⇒
// 重跑面;ResetNodeToPending 的 attempts_base=attempts 语义 ⇒ 预算满额;
// 保活行非 EXECUTING ⇒ reap no-op、worker 活态闸幂等 ack(D-t193)。

import (
	"braid/internal/engine"
	"braid/internal/engine/dsl"
)

// recoveryKeep 描述单个 parallel 节点在闭包重置面的保活裁定:map 存在性
// 即「保活 SUCCEEDED 实例行」(控制行重置的增量恢复形状);whole 进一步
// 升格为控制行 SUCCEEDED 下的整行集保活(零重置,行集自证)。
type recoveryKeep struct {
	whole bool
}

// planIncrementalRecovery 对闭包内 parallel 节点逐个裁定保活面。纯函数:
// 输入 = 全行锁当前读的行集 + 钉版定义 + 闭包/target,零 DB 触达。
func planIncrementalRecovery(rows *lockedRows, g *dag, wf *dsl.Workflow,
	closure map[string]bool, target string) map[string]recoveryKeep {
	plan := map[string]recoveryKeep{}
	for _, id := range sortedNodes(g.nodes) {
		if !closure[id] {
			continue
		}
		ctrl := rows.byNodeID[id]
		if ctrl == nil || ctrl.nodeType != "parallel" {
			continue
		}
		if target == id && ctrl.status == engine.NodeSucceeded {
			continue // 意图门:SUCCEEDED 节点的显式重跑 = 再生,不保活
		}
		if !depsOutsideClosure(wf, id, closure) {
			continue // 依赖门:快照输入可能已变,保守整行集重跑
		}
		if ctrl.status == engine.NodeSucceeded {
			if !noBranchAncestorInClosure(g, id, closure) {
				continue // 控制依赖门(对抗审计 HIGH 修):路由可能改变,整行集重跑
			}
			plan[id] = recoveryKeep{whole: true}
			continue
		}
		plan[id] = recoveryKeep{}
	}
	return plan
}

// noBranchAncestorInClosure 报告节点 id 的祖先锥内是否有 branch 型节点落在
// 闭包中(对抗审计 HIGH 修:whole-keep 的控制依赖门)。闭包内 branch 必被
// 重置重评估(PENDING/SUCCEEDED 之外的全被收编)⇒ 路由可能改变;而
// dispatchBranch 的剪枝面只吃 PENDING 后继——whole-keep 的 SUCCEEDED 控制器
// 会逃逸重剪:废弃臂被步 13 advance 急派(旧 results 上执行)+ 翻转后残留
// SUCCEEDED 入终态,与 branch.evaluated 审计矛盾。succ-keep 形状不受影响:
// 控制器重置回 PENDING,dispatchBranch/skipOne 正常重剪(实例面保活行留作
// 剪枝臂上的终态证据,results 从未物化)。
func noBranchAncestorInClosure(g *dag, id string, closure map[string]bool) bool {
	seen := map[string]bool{}
	stack := append([]string(nil), g.preds[id]...)
	for len(stack) > 0 {
		a := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[a] {
			continue
		}
		seen[a] = true
		if g.nodes[a].Branch != nil && closure[a] {
			return false
		}
		stack = append(stack, g.preds[a]...)
	}
	return true
}

// depsOutsideClosure 报告节点 id 的快照数据依赖是否全部在闭包外
// (闭包外 ⇒ 未被重置 ⇒ 输出不可变 ⇒ 旧快照仍真)。
func depsOutsideClosure(wf *dsl.Workflow, id string, closure map[string]bool) bool {
	for _, dep := range dsl.NodeDataDeps(wf, id) {
		if closure[dep] {
			return false
		}
	}
	return true
}

// keepAliveRow 报告闭包重置循环是否应跳过该行(保活 = 零写零审计零预算)。
func keepAliveRow(plan map[string]recoveryKeep, r *nodeRow) bool {
	k, ok := plan[r.nodeID]
	if !ok {
		return false
	}
	if k.whole {
		return true // 整行集保活(控制行 SUCCEEDED ⇒ 实例重置必悬死)
	}
	return r.instanceNo > 0 && r.status == engine.NodeSucceeded
}
