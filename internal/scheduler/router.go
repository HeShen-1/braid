package scheduler

import (
	"fmt"

	"braid/internal/engine/dsl"
)

// Router 是分支路由缝(T3.4 的唯一替换点):对 branch 节点求值路由目标。
// resolve 把纯引用文本解析为当前值(未命中 ok=false)。调度逻辑(跳过传播、
// 取路推进)不在缝内——T3.4 只换实现,不换调度。
type Router func(b *dsl.BranchNode, resolve func(ref string) (any, bool)) (string, error)

// StubRouter 是 T2.4 桩谓词路由:按 cases 声明序解析 when.left,要求 JSON 布尔值
// (显式布尔字段),首个 true → 该 case.next,无命中 → default。右值/操作符的
// 完整求值(eq/ne/gt/...)是 T3.4;左值非布尔或不可解析在此即为错误(类型错误
// 单测的地基),由调度器收口为执行级 branch_eval 失败。
func StubRouter(b *dsl.BranchNode, resolve func(ref string) (any, bool)) (string, error) {
	for i := range b.Cases {
		c := &b.Cases[i]
		v, ok := resolve(c.When.Left)
		if !ok {
			return "", fmt.Errorf("stub router: cases[%d].left %s unresolved", i, c.When.Left)
		}
		take, isBool := v.(bool)
		if !isBool {
			return "", fmt.Errorf("stub router: cases[%d].left %s = %T, want boolean (full evaluation is T3.4)", i, c.When.Left, v)
		}
		if take {
			return c.Next, nil
		}
	}
	return b.Default, nil
}
