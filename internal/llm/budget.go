package llm

// T18.2 token 预算闸门:纯语义半边(D-2026-10-05-t182-token-budget-gate)。
// 口径 = 每执行(BRAID_LLM_TOKEN_BUDGET,0 = 闸门关闭缺省关):判据 =
// Σ 该执行已计费 usage(node_attempt.total_tokens,计费真相源 §2)+
// 当次调用估算(EstimatePromptTokens prompt + MaxTokens 完成预留)>
// 限额 → pre-call 拒绝。本文件只做判定,不做状态查询与收口——消费点在
// internal/nodes/llm.go(唯一有 ExecutionID/DB 全上下文的缝),llm 包保持
// DB-free。计费真相源 = API 返回 usage、闸门口径 = 估算,两者永不互换
// (§2 口径纪律);跨执行成本分析归 T8.x 报表面,闸门只做护栏。
//
// 已知边界(登记,D-t182 决策 7):并行节点 TOCTOU(各自过闸合计可超,
// 护栏非硬不变量);失败/DEAD/discarded attempt 的 token 不进 Σ(系统性
// 低估);估算恒近似。

// MaxTokenBudget 是 BRAID_LLM_TOKEN_BUDGET 的误配防御上界(Validate 拒
// 超):预算在此量级以上不可能被单执行触达,大数字通常是把别的旋钮值
// 粘错了位——启动即败好于静默永闸门关。
const MaxTokenBudget TokenBudget = 1_000_000_000

// TokenBudget 是单次 workflow_execution 的总 token 上限(已计费 Σ +
// 当次估算的合并口径)。零值 = 闸门关闭(缺省,存量部署零行为变化);
// 拒绝语义(响亮 fail-closed 收口、审计分面)见消费点。
type TokenBudget uint64

// Enabled 报告闸门是否配置(0 = 关)。
func (b TokenBudget) Enabled() bool { return b > 0 }

// Exceeded 报告本次调用是否超预算:spent = Σ 该执行已计费 usage,
// estPrompt = 当次 prompt 估算,completionReserve = 当次完成预留
// (DSL max_tokens 声明时计入、未声明 0——预算估算宁可高估)。
// 加法饱和(uint64 溢出不会回卷成假阴);边界含(== 限额放行)。
func (b TokenBudget) Exceeded(spent, estPrompt, completionReserve uint64) bool {
	if !b.Enabled() {
		return false
	}
	return saturatingAdd(spent, estPrompt, completionReserve) > uint64(b)
}

// saturatingAdd 求和,任一步溢出即饱和 math.MaxUint64。
func saturatingAdd(vs ...uint64) uint64 {
	var total uint64
	for _, v := range vs {
		if total > ^uint64(0)-v {
			return ^uint64(0)
		}
		total += v
	}
	return total
}
