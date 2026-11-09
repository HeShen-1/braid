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
import { useId, useRef, useState } from 'react'
import { ArrowLeft, Ban, Check, TriangleAlert } from 'lucide-react'
import type { ApprovalDetail as ApprovalDetailData, ExecutionDetail } from './api.ts'
import { DECISION_KEY, useCollapsed } from './prefs.ts'
import { SectionTabs } from './ui/SectionTabs.tsx'
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
  canDecide?: boolean
  decisionHint?: string
  execution?: ExecutionDetail | null
  contextError?: string
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
  canDecide = false,
  decisionHint = '当前账号不能提交新的决定。',
  execution = null,
  contextError = '',
}: ApprovalDetailProps) {
  // 确认层契约本体在 ui/confirm.tsx(挂载即聚焦);布防状态由组合根
  // (ApprovalsView)持有,这里只补「收起后焦点回入口按钮」(T21.4 增项)。
  const tabsId = useId()
  const [tab, setTab] = useState('content')
  const [decisionCollapsed, setDecisionCollapsed] = useCollapsed(DECISION_KEY)
  const decisionVisible = !decisionCollapsed || confirmReject
  const rejectArmRef = useFocusReturn<HTMLButtonElement>(confirmReject)
  const confirmRef = useRef<HTMLDivElement | null>(null)

  if (detail === null) {
    return (
      <article className="appr-detail" data-testid={TID.approvalDetail}>
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
      </article>
    )
  }

  const actionable = detail.node_status === 'WAITING_APPROVAL' && canDecide
  const approvers = detail.approvers ?? []
  const pending = detail.pending_approvers ?? []
  const decisions = detail.decisions ?? []
  const title =
    definitionName !== undefined && definitionName !== ''
      ? `${definitionName} · ${detail.node_id}`
      : `执行 ${detail.execution_id} · ${detail.node_id}`

  return (
    <article className="appr-detail" data-testid={TID.approvalDetail} data-state={detail.node_status}>
      {/* ① 身份 + 元数据 */}
      <header className="panel">
        <div className="panel-body appr-head-body">
          <div className="appr-head-title">
            <h2 className="text-md font-semibold text-ink-0">{title}</h2>
            <StatusChip kind="node" status={detail.node_status} />
            <span className="ml-auto flex flex-wrap items-center gap-2">
              <a className="btn btn-quiet btn-sm" href={`/executions/${detail.execution_id}`}>查看执行详情与审计</a>
              <Button variant="quiet" size="sm" icon={ArrowLeft} onClick={onBack}>
                返回队列
              </Button>
            </span>
          </div>
          <p className="appr-keys appr-detail-keys" aria-label="详情快捷键">
            <span><kbd>Esc</kbd> 返回队列</span>
            {actionable ? <><span><kbd>a</kbd> 批准</span><span><kbd>r</kbd> 驳回</span></> : null}
          </p>
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

      <SectionTabs id={tabsId} label="审批详情视图" items={[{ value: 'content', label: '审批内容' }, { value: 'history', label: '审批记录' }]} value={tab} onChange={setTab}
        actions={actionable ? <Button ref={decisionCollapsed ? rejectArmRef : undefined} size="sm" variant="quiet" aria-expanded={decisionVisible} aria-controls={`${tabsId}-decision`} onClick={() => setDecisionCollapsed(decisionVisible)} disabled={confirmReject}>{decisionVisible ? '收起处理栏' : '展开处理栏'}</Button> : undefined} />
      <div className="appr-workarea" data-decision-visible={actionable && decisionVisible}>
      <div className="appr-reader">
      <section role="tabpanel" id={`${tabsId}-panel-content`} aria-labelledby={`${tabsId}-tab-content`} hidden={tab !== 'content'} tabIndex={0}>
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

      <Panel><PanelHead title="执行上下文" sub={execution == null ? undefined : `${execution.definition_name} v${execution.definition_version}`} />
        <div className="panel-body appr-context">
          {execution != null ? <><p className="text-sm text-ink-1">触发人：<span className="mono">{execution.triggered_by}</span></p><details className="mt-3"><summary>原始输入</summary><p className="text-xs text-ink-2">解析后展示；精确值可在执行详情的原始响应核对。</p><pre className="mt-2 overflow-x-auto text-xs text-ink-1">{JSON.stringify(execution.input, null, 2)}</pre></details></>
            : <p className="text-sm text-ink-2">{contextError !== '' ? `执行上下文未能读取：${contextError}` : '正在读取执行上下文…'}</p>}
        </div>
      </Panel>

      </section>
      <section role="tabpanel" id={`${tabsId}-panel-history`} aria-labelledby={`${tabsId}-tab-history`} hidden={tab !== 'history'} tabIndex={0}>
      {/* ③ 审批记录 */}
      <Panel>
        <PanelHead title="审批记录" sub={decisions.length > 0 ? `${decisions.length} 票` : '尚无决定'} />
        <div className="appr-history-scroll">
          <table className="table appr-history-table" data-testid={TID.approvalTimeline}>
            <caption className="sr-only">审批决定记录</caption>
            <thead><tr><th scope="col">处理人</th><th scope="col">决定</th><th scope="col">处理意见</th><th scope="col">时间</th><th scope="col">轮次</th></tr></thead>
            <tbody>{decisions.length === 0
              ? <tr><td colSpan={5} className="appr-history-empty">尚无审批决定，等待审批人处理。</td></tr>
              : decisions.map((dec) => <tr key={dec.id}>
                <td className="mono" data-label="处理人">{dec.decided_by}</td>
                <td data-label="决定" className={dec.decision === 'reject' ? 'appr-decision-reject' : 'appr-decision-approve'}>{decisionLabel(dec.decision)}</td>
                <td className="appr-history-comment" data-label="处理意见">{dec.comment || '—'}</td>
                <td className="num" data-label="时间">{fmtDateTime(dec.decided_at)}</td>
                <td className="num" data-label="轮次">第 {dec.round} 轮</td>
              </tr>)}</tbody>
          </table>
        </div>
      </Panel>

      </section>
      </div>
      <aside className="appr-decision-aside" id={`${tabsId}-decision`} aria-label="审批处理" hidden={actionable && !decisionVisible}>
      {/* ④ 决定 */}
      {actionable ? (
        <Panel className="appr-decision-panel">
          <PanelHead title="审批决定" sub="备注选填；驳回前将再次确认" />
          <div className="panel-body appr-decide">
            <TextArea
              label="备注(可选,≤2000 字)"
              rows={7}
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
                ref={decisionCollapsed ? undefined : rejectArmRef}
                variant="danger"
                icon={Ban}
                data-testid={TID.approvalReject}
                disabled={busy || commentError !== ''}
                onClick={() => onRequestDecision('reject')}
              >
                驳回
              </Button>
              {busy ? <span className="text-xs text-ink-2" role="status">提交中…</span> : null}
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
          <strong>{detail.node_status === 'WAITING_APPROVAL' ? '当前审批只读' : '该审批已收口'}</strong>
          <span className="ml-2 text-ink-1">
            {detail.node_status === 'WAITING_APPROVAL' ? decisionHint : `节点状态 ${nodeStatus(detail.node_status).label},不再接受新的决定(后端同样会拒绝)。`}
          </span>
        </Alert>
      )}
      </aside>
      </div>
    </article>
  )
}

function isOverdue(iso: string | null): boolean {
  if (iso === null || iso === '') return false
  const t = new Date(iso).getTime()
  return !Number.isNaN(t) && t < Date.now()
}
