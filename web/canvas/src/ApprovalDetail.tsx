// 审批详情(T21.2 Lane A)——右列:身份 + 元数据 + 审批说明 + 决定时间线
// + 决定表单。
//
// 逐项对应旧 /ui/ 审批台 renderDetail:
//   执行 ID / 节点状态 / 审批模式 / 审批人 / 待投名单(空 = —)/ 截止时间;
//   审批说明为空 → 「(工作流未提供审批说明)」;
//   决定时间线 = decided_at · decided_by · decisionLabel(decision) + 可选备注。
//
// 安全纪律(与 web.go CSP 同套):instructions / comment / decided_by 都是
// 他人自由文本 —— 一律走 React 文本节点(禁 innerHTML 家族)。
// 决定表单只在 node_status === 'WAITING_APPROVAL' 时在场(收口节点只读,
// 前置免无效提交;后端同样 409)。备注上限在提交前就近拦下。
import { useRef } from 'react'
import { ArrowLeft, Ban, Check, TriangleAlert } from 'lucide-react'
import type { ApprovalDetail as ApprovalDetailData } from './api.ts'
import { TextArea } from './ui/field.tsx'
import { ConfirmLayer, useFocusReturn } from './ui/confirm.tsx'
import { Alert, Button, KV, Panel, PanelHead, StatusChip } from './ui/primitives.tsx'
import { approvalModeLabel, decisionLabel, nodeStatus } from './ui/status.ts'
import { TID } from './ui/testids.ts'
import { fmtDateTime, Timestamp } from './ui/time.tsx'

/** 备注上限(与 internal/api/approval.go 的 maxApprovalComment 同值)。 */
export const COMMENT_MAX = 2000

/** 服务端按 **rune** 计(utf8mb4 VARCHAR(2000));JS 的 .length 是 UTF-16
 *  码元,星平面字符(emoji 等)会数成 2 —— 客户端必须同口径,否则会把
 *  服务端接受的备注拦在门外。 */
export function approvalCommentCount(s: string): number {
  return [...s].length
}

export interface ApprovalDetailProps {  detail: ApprovalDetailData | null
  loading: boolean
  error: string
  /** 列表行带来的定义名(详情响应没有该字段;深链时缺席,标题回退为执行 id)。 */
  definitionName?: string
  comment: string
  commentError: string
  busy: boolean
  confirmReject: boolean
  onCommentChange: (v: string) => void
  /** 决定请求的唯一入口(按钮与快捷键共用;确认层也走它)。 */
  onRequestDecision: (d: 'approve' | 'reject') => void
  onCancelReject: () => void
  onBack: () => void
}

export function ApprovalDetail({
  detail,
  loading,
  error,
  definitionName,
  comment,
  commentError,
  busy,
  confirmReject,
  onCommentChange,
  onRequestDecision,
  onCancelReject,
  onBack,
}: ApprovalDetailProps) {
  // 确认层契约本体在 ui/confirm.tsx(挂载即聚焦);布防状态由组合根
  // (ApprovalsView)持有,这里只补「收起后焦点回入口按钮」(T21.4 增项)。
  const rejectArmRef = useFocusReturn<HTMLButtonElement>(confirmReject)
  const confirmRef = useRef<HTMLDivElement | null>(null)

  if (detail === null) {
    return (
      <main className="appr-detail" data-testid={TID.approvalDetail}>
        {loading ? (
          <p className="text-sm text-ink-2">加载审批详情…</p>
        ) : (
          <>
            <Alert tone="error" icon={TriangleAlert}>
              <strong>审批详情读取失败</strong>
              {error !== '' ? <code className="mono ml-2">{error}</code> : null}
            </Alert>
            <div>
              <Button variant="quiet" size="sm" icon={ArrowLeft} onClick={onBack}>
                返回队列
              </Button>
            </div>
          </>
        )}
      </main>
    )
  }

  const actionable = detail.node_status === 'WAITING_APPROVAL'
  const approvers = detail.approvers ?? []
  const pending = detail.pending_approvers ?? []
  const decisions = detail.decisions ?? []
  const title =
    definitionName !== undefined && definitionName !== ''
      ? `${definitionName} · ${detail.node_id}`
      : `执行 ${detail.execution_id} · ${detail.node_id}`

  return (
    <main className="appr-detail" data-testid={TID.approvalDetail} data-state={detail.node_status}>
      {/* ① 身份 + 元数据 */}
      <header className="panel">
        <div className="panel-body appr-head-body">
          <div className="appr-head-title">
            <StatusChip kind="node" status={detail.node_status} />
            <h2 className="text-md font-semibold text-ink-0">{title}</h2>
            <span className="ml-auto">
              <Button variant="quiet" size="sm" icon={ArrowLeft} onClick={onBack}>
                返回队列
              </Button>
            </span>
          </div>
          <dl className="appr-meta" data-testid={TID.approvalMeta}>
            <KV k="执行 ID">
              <code className="panel-id">{detail.execution_id}</code>
            </KV>
            <KV k="节点">
              <code className="mono text-ink-0">{detail.node_id}</code>
            </KV>
            <KV k="审批模式">{approvalModeLabel(detail.mode)}</KV>
            <KV k="审批人">{approvers.length > 0 ? approvers.join(', ') : '—'}</KV>
            <KV k="待投名单">
              {/* 仅 WAITING 时非空;空 = —(旧实现同口径)。会签中间态的
                  可见进度 = 名单 − 本轮已投。 */}
              <span data-testid={TID.pendingApprovers}>{pending.length > 0 ? pending.join(', ') : '—'}</span>
            </KV>
            <KV k="截止时间">
              <span data-testid={TID.approvalDeadline} data-overdue={isOverdue(detail.deadline_at) ? 'true' : 'false'}>
                <Timestamp iso={detail.deadline_at} />
              </span>
            </KV>
          </dl>
        </div>
      </header>

      {/* ② 审批说明(自由文本,文本节点) */}
      <Panel>
        <PanelHead title="审批说明" />
        <div className="panel-body">
          <p className="appr-instructions measure" data-testid={TID.approvalInstructions}>
            {detail.instructions !== undefined && detail.instructions !== ''
              ? detail.instructions
              : '(工作流未提供审批说明)'}
          </p>
        </div>
      </Panel>

      {/* ③ 决定时间线 */}
      <Panel>
        <PanelHead title="决定时间线" sub={decisions.length > 0 ? `${decisions.length} 票` : '尚无决定'} />
        {decisions.length === 0 ? (
          <div className="panel-body">
            <p className="text-sm text-ink-2">还没有人投出决定 —— 你是第一个。</p>
          </div>
        ) : (
          <ul className="appr-decisions" data-testid={TID.approvalTimeline}>
            {decisions.map((dec) => (
              <li key={dec.id}>
                <span className="num text-ink-2">{fmtDateTime(dec.decided_at)}</span>
                <span className="mono text-ink-0">{dec.decided_by}</span>
                <span className={dec.decision === 'reject' ? 'appr-decision-reject' : 'appr-decision-approve'}>
                  {decisionLabel(dec.decision)}
                </span>
                {dec.comment !== null && dec.comment !== '' ? <span className="text-ink-1">{dec.comment}</span> : null}
              </li>
            ))}
          </ul>
        )}
      </Panel>

      {/* ④ 决定 */}
      {actionable ? (
        <Panel>
          <PanelHead title="决定" sub="提交即续跑挂起的执行" />
          <div className="panel-body appr-decide">
            <TextArea
              label="备注(可选,≤2000 字)"
              rows={3}
              value={comment}
              error={commentError}
              hint={commentError === '' ? `${approvalCommentCount(comment)} / ${COMMENT_MAX} 字` : undefined}
              data-testid={TID.approvalComment}
              disabled={busy}
              onChange={(e) => onCommentChange(e.target.value)}
            />
            {/* 提交期间两个按钮同时禁用(对抗审计:双击不得并发两笔 POST)。
                键盘 a/r 走同一条 requestDecision,同样被拦。 */}
            <div className="appr-actions">
              <Button
                variant="primary"
                icon={Check}
                data-testid={TID.approvalApprove}
                disabled={busy || commentError !== ''}
                onClick={() => onRequestDecision('approve')}
              >
                批准
              </Button>
              <Button
                ref={rejectArmRef}
                variant="danger"
                icon={Ban}
                data-testid={TID.approvalReject}
                disabled={busy || commentError !== ''}
                onClick={() => onRequestDecision('reject')}
              >
                驳回
              </Button>
              <span className="text-xs text-ink-2">{busy ? '提交中…' : 'a 批准 · r 驳回'}</span>
            </div>
            {/* 驳回 = 一次布防 + 一次确认(会签驳回即判死,与服务端不可改判
                同构);批准是主操作,单击直达。Esc / 放弃 收起确认层。
                确认层是一次性浮起层:布防即把焦点移进来,与执行面的「取消
                执行」确认条同契约(收尾评审 fix 3:否则按 r 只产生视觉变化,
                读屏用户不会被通知)。 */}
            {confirmReject ? (
              <ConfirmLayer
                layerRef={confirmRef}
                label="确认驳回"
                hint="驳回落库即定局,会写进审计与决定时间线。"
                confirmText="确认驳回"
                busy={busy}
                onConfirm={() => onRequestDecision('reject')}
                onDismiss={onCancelReject}
              />
            ) : null}
            {error !== '' ? (
              <Alert tone="error" icon={TriangleAlert}>
                {error}
              </Alert>
            ) : null}
          </div>
        </Panel>
      ) : (
        <Alert tone="warn" icon={TriangleAlert}>
          <strong>该审批已收口</strong>
          <span className="ml-2 text-ink-1">
            节点状态 {nodeStatus(detail.node_status).label},不再接受新的决定(后端同样会拒绝)。
          </span>
        </Alert>
      )}
    </main>
  )
}

function isOverdue(iso: string | null): boolean {
  if (iso === null || iso === '') return false
  const t = new Date(iso).getTime()
  return !Number.isNaN(t) && t < Date.now()
}
