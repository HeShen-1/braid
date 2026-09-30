package scheduler

// 会签定局判据(T14.2,D-2026-09-30-t142-and-approval):approval mode 的
// 语义单点,api 决定事务(approval.go)/ worker 续跑复核(resume.go)/
// reaper 已决票判定(scan_approval.go)三方共消费——判据分叉 = 三面行为
// 漂移,故以纯函数钉死(D-t42 #9-③ 预告、D-t92 决策 6 钉点的 CountDecisions
// 判据改法在此落定)。
//
// 语义:
//   - any(或签,默认):本轮任一票定局,首票方向即裁决(既有 T4.2 语义)。
//   - all(会签):任一 REJECTED 即定局(reject,任一驳回即败);本轮
//     APPROVED 票数 == 名单长度才定局(approve,全部批准才过)。
//
// REJECTED 谓词先于计数比较:all 下「先批准后驳回」= 驳回定局,拒绝方向
// 恒不依赖名单完整性(超时面在票不齐时仍可达驳回分支)。名单 uniqueItems
// 由 schema 钉死(发布期前移):重复名单在 COUNT==len 判据下 = 永久挂起
// 死锁。前置条件:mode ∈ {"", "any", "all"}(词面校验在发布期 schema enum
// 与各消费点响亮兜底;空词面 = effectiveApprovalSpec 同默认 any)。

// approval mode 词表(schema enum 同源;空词面缺省 = any)。
const (
	ApprovalModeAny = "any"
	ApprovalModeAll = "all"
)

// ApprovalRoundCountSQL 是定局判据的数据面投影(本轮 APPROVED/REJECTED
// 双计数),api 决定事务(approvalstore)/worker 复核(resume 5a)/reaper
// 处置事务(scan_approval [5])三方逐字共用的单源——判据语义在
// ApprovalOutcome,数据投影在此防三处 SQL 漂移(对抗审计 MEDIUM:逐字复制
// 下未来给判据加维度漏改一处 = 三面行为分叉,典型表象 api 判定局落事件、
// worker 5a 判 undecided 响亮拒)。
const ApprovalRoundCountSQL = `SELECT COALESCE(SUM(decision='APPROVED'),0), COALESCE(SUM(decision='REJECTED'),0)
   FROM approval_decision WHERE node_execution_id=? AND round=?`

// ApprovalOutcome 按 mode 裁决定局:final=false = 本轮未定局(节点继续
// WAITING_APPROVAL,不落 resume 事件);final=true 时 direction 为定局方向
// (DecisionApproved | DecisionRejected)。输入 = 本轮(round 锚定)票数与
// 名单长度;调用方在 node 行锁内读数(锁序保证计数稳定)。
//
// 防御语义:approvers<=0 时 all 模式永不判 approve 定局(Q9「永不自动
// 通过」的安全侧——schema minItems=1 使之不可达,结构性双保险)。
func ApprovalOutcome(mode string, approvers, approved, rejected int) (final bool, direction string) {
	if rejected > 0 {
		return true, DecisionRejected
	}
	if mode == ApprovalModeAll {
		if approvers > 0 && approved >= approvers {
			return true, DecisionApproved
		}
		return false, ""
	}
	// any(空词面同默认):首票定局,rejected>0 已在上方先裁。
	if approved > 0 {
		return true, DecisionApproved
	}
	return false, ""
}
