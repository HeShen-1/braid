// 审批面(T21.2 Lane A;T21.4 ⑥ 改同页两级视图)—— 原 /ui/ 审批台
// (原生三件套)并入统一 SPA 的组合根。两级视图(D-2026-10-09-console-v2
// 决策 5:不改 token 载体、不加端点,故不做新标签页):未选中 = 队列列表
// 占满全页;选中 = 详情占满全页,「返回队列」/ Esc 回列表。零后端新端点:
// 端点、字段沿用既有协议，错误显示复用 ui/errors.ts。
//
// 三条对抗审计不变量(旧实现 app.js 的注释即契约,不得退化):
//   ① 列表请求序号守卫(listSeq):快速连点时后发先至的陈旧响应丢弃,
//      连错误面一起丢(否则新页被旧响应覆盖);
//   ② 进入新详情必须清空上一条未提交的备注(备注会作为下一节点的决定
//      comment 落库进 decision 行与审计);
//   ③ in-flight 防抖:提交期间两个按钮同时禁用 + ref 闸门,双击/连按都
//      不会并发两笔 POST。
//
// 选中态真源在 URL(routes/ApprovalsRoute 受控传入);组件只发 onSelect。
// 401 统一交 onAuthExpired(App 层 cleartoken 回登录)。
import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, api, type ApprovalDetail as ApprovalDetailData, type ExecutionDetail, type PendingApproval } from './api.ts'
import { ApprovalDetail, COMMENT_MAX, approvalCommentCount } from './ApprovalDetail.tsx'
import { ApprovalQueue, PAGE_LIMIT, type ApprovalSelection } from './ApprovalQueue.tsx'
import { decisionLabel } from './ui/status.ts'

import { apiErrorText, ERROR_TEXT } from './ui/errors.ts'
import { CONSOLE_REFRESH, LIVE_REFRESH_MS, refreshConsole } from './refresh.ts'

/** 备注上限提示词(与后端 bad_comment 同文案;上限见 ApprovalDetail.COMMENT_MAX)。 */
const COMMENT_TOO_LONG = ERROR_TEXT.bad_comment as string

export function approvalErrorText(err: unknown): string {
  return apiErrorText(err)
}

/** 焦点在可编辑控件里时,单键快捷键一律让位 —— 打字不是命令。 */
function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  const tag = target.tagName
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true
  return target.isContentEditable
}

export function ApprovalsView({
  selected,
  onSelect,
  onAuthExpired,
  canDecide = false,
  username = null,
}: {
  selected: ApprovalSelection | null
  onSelect: (sel: ApprovalSelection | null) => void
  onAuthExpired: () => void
  canDecide?: boolean
  username?: string | null
}) {
  const selExec = selected?.executionId ?? null
  const selNode = selected?.nodeId ?? null

  // 401 出口走 ref:详情加载 effect 的依赖里不能有这个函数(父级换身份会
  // 重跑 effect,把正在编辑的备注清掉)。
  const expired = useRef(onAuthExpired)
  useEffect(() => {
    expired.current = onAuthExpired
  })

  // ---- 队列 ----
  const [items, setItems] = useState<PendingApproval[]>([])
  const [total, setTotal] = useState(0)
  const [offset, setOffset] = useState(0)
  const [listLoading, setListLoading] = useState(true)
  const [listError, setListError] = useState('')
  const [flash, setFlash] = useState('')
  const [cursorKey, setCursorKey] = useState<string | null>(null)
  const cursor = Math.max(0, items.findIndex((item) => `${item.execution_id}/${item.node_id}` === cursorKey))
  const [focusRequest, setFocusRequest] = useState(0)
  const [lastDecision, setLastDecision] = useState<ApprovalSelection | null>(null)
  // ① listSeq:列表请求序号。快速连点时后发先至的陈旧响应必须丢弃。
  const listSeq = useRef(0)

  const loadPage = useCallback(async (off: number, background = false) => {
    const seq = ++listSeq.current
    setListError('')
    if (!background) setListLoading(true)
    try {
      const data = await api.listPendingApprovals({ limit: PAGE_LIMIT, offset: off })
      if (seq !== listSeq.current) return // 陈旧响应:已被更新的请求取代
      const its = Array.isArray(data.items) ? data.items : []
      // 只在被聚焦的待办移除时回退焦点；刷新不能抢走工具栏或输入框。
      const focusedKey = document.activeElement instanceof HTMLElement
        ? document.activeElement.closest<HTMLElement>('[data-approval-key]')?.dataset.approvalKey
        : undefined
      if (focusedKey !== undefined && !its.some((item) => `${item.execution_id}/${item.node_id}` === focusedKey)) setFocusRequest((n) => n + 1)
      setItems(its)
      setTotal(typeof data.page?.total === 'number' ? data.page.total : its.length)
    } catch (err) {
      if (seq !== listSeq.current) return // 陈旧失败同样丢弃(别拿旧错误顶新页)
      if (err instanceof ApiError && err.status === 401) {
        expired.current()
        return
      }
      setListError(approvalErrorText(err))
    } finally {
      if (seq === listSeq.current) setListLoading(false)
    }
  }, [])

  useEffect(() => {
    void loadPage(offset)
    const refresh = () => { void loadPage(offset, true) }
    const timer = window.setInterval(refresh, LIVE_REFRESH_MS)
    window.addEventListener(CONSOLE_REFRESH, refresh)
    window.addEventListener('focus', refresh)
    return () => {
      listSeq.current++
      window.clearInterval(timer)
      window.removeEventListener(CONSOLE_REFRESH, refresh)
      window.removeEventListener('focus', refresh)
    }
  }, [loadPage, offset])

  /** 旧实现语义:刷新按钮与决定成功后都 offset 归零重拉。 */
  const reloadFromFirst = useCallback(() => {
    if (offset === 0) void loadPage(0)
    else setOffset(0)
  }, [offset, loadPage])

  // ---- 详情 ----
  const [detail, setDetail] = useState<ApprovalDetailData | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [execution, setExecution] = useState<ExecutionDetail | null>(null)
  const [contextError, setContextError] = useState('')
  // 错误带归属键:提交/读取失败可能落在"已经切走"的旧节点上 —— 归属当前
  // 选中才就近贴在表单,归属别处则退回队列错误条(不误挂到新节点头上)。
  const [decisionError, setDecisionError] = useState<{ key: string; text: string } | null>(null)
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirmReject, setConfirmReject] = useState(false)
  const detailSeq = useRef(0)
  // ③ in-flight 闸门:两笔事件落在同一 tick 时 state 还没刷,只看 busy 会
  // 留窗口(旧实现靠同步读 $("approve").disabled)。
  const inFlight = useRef(false)

  const selKey = selExec !== null && selNode !== null ? `${selExec}/${selNode}` : null
  // 提交在飞时用户可能已经切走:await 之后要靠这个 ref 判断"还是同一条吗",
  // 免得把旧节点的备注清空/把用户从新节点弹回队列(备注串号面)。
  const selKeyRef = useRef<string | null>(selKey)
  useEffect(() => {
    selKeyRef.current = selKey
  }, [selKey])

  useEffect(() => {
    // ② 换详情即清空备注(旧实现的对抗审计项):备注与节点一一对应,残留
    // 会作为下一节点的 comment 落库。归属错误只在**打开新详情**时重置 ——
    // 回列表级(T21.4 ⑥)不清:迟到失败的错误条还等着在队列里被看见
    // (守卫注释的本意 = 不静默吞)。
    setComment('')
    setConfirmReject(false)
    setDetail(null)
    setExecution(null)
    setContextError('')
    const seq = ++detailSeq.current
    if (selExec === null || selNode === null) return
    setDecisionError(null)
    const key = `${selExec}/${selNode}`
    setDetailLoading(true)
    api
      .getApproval(selExec, selNode)
      .then((d) => {
        if (seq === detailSeq.current) setDetail(d)
      })
      .catch((err: unknown) => {
        if (seq !== detailSeq.current) return
        if (err instanceof ApiError && err.status === 401) {
          expired.current()
          return
        }
        setDecisionError({ key, text: approvalErrorText(err) })
      })
      .finally(() => {
        if (seq === detailSeq.current) setDetailLoading(false)
      })
    void api.getExecution(selExec).then((data) => {
      if (seq === detailSeq.current) setExecution(data)
    }).catch((err: unknown) => {
      if (seq !== detailSeq.current) return
      if (err instanceof ApiError && err.status === 401) expired.current()
      else setContextError(approvalErrorText(err))
    })
    return () => { detailSeq.current++ }
  }, [selExec, selNode])

  // 详情响应没有 definition_name(旧实现从列表行补):从上一条队列命中取,
  // 深链直达时缺席 → 标题回退为执行 id。
  const definitionName =
    selExec === null || selNode === null
      ? undefined
      : items.find((it) => it.execution_id === selExec && it.node_id === selNode)?.definition_name

  const actionable = canDecide && username !== null && detail !== null && detail.node_status === 'WAITING_APPROVAL' && detail.pending_approvers.includes(username)
  const commentCount = approvalCommentCount(comment)
  const commentError = commentCount > COMMENT_MAX ? COMMENT_TOO_LONG : ''
  // 归属当前选中的错误 → 就近贴在表单;归属别处 → 队列错误条兜住(不静默吞)。
  const detailErrorText = decisionError !== null && decisionError.key === selKey ? decisionError.text : ''
  const queueError =
    listError !== ''
      ? listError
      : decisionError !== null && decisionError.key !== selKey
        ? decisionError.text
        : ''

  const submit = useCallback(
    async (decision: 'approve' | 'reject') => {
      if (inFlight.current || !actionable) return
      if (selExec === null || selNode === null) return
      const submittedKey = `${selExec}/${selNode}`
      if (approvalCommentCount(comment) > COMMENT_MAX) {
        // 超出上限:提交前就近提示,不发无效 POST(服务端 bad_comment 兜底)。
        setDecisionError({ key: submittedKey, text: COMMENT_TOO_LONG })
        return
      }
      inFlight.current = true
      setBusy(true)
      setDecisionError(null)
      try {
        const out = await api.decideApproval(selExec, selNode, decision, comment)
        setLastDecision({ executionId: selExec, nodeId: selNode })
        refreshConsole()
        // 还在同一条详情上才清备注/回队列 —— 用户若已切走,别动他的新输入。
        if (selKeyRef.current === submittedKey) {
          setComment('')
          setConfirmReject(false)
          onSelect(null)
          setFocusRequest((n) => n + 1)
        }
        // 反馈挂在队列面板(列表级):在本条上提交成功会自动回列表,主路径
        // 即见;提交在飞时切走再回来的,回列表级补见(T21.4 ⑥ 两级视图)。
        setFlash(
          `已记录:${decisionLabel(out.decision)}${out.comment !== null && out.comment !== '' ? ` · ${out.comment}` : ''}`,
        )
        // 成功即回队列、offset 归零重拉(旧实现 showView("list-view") 同义)。
        reloadFromFirst()
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          expired.current()
          return
        }
        setDecisionError({ key: submittedKey, text: approvalErrorText(err) })
      } finally {
        inFlight.current = false
        setBusy(false)
      }
    },
    [selExec, selNode, comment, onSelect, reloadFromFirst, actionable],
  )

  /** 决定唯一入口:按钮、确认层、快捷键都走这里(不绕过防抖与确认)。 */
  const requestDecision = useCallback(
    (decision: 'approve' | 'reject') => {
      if (busy || inFlight.current) return
      if (selExec === null || selNode === null || !actionable) return
      if (decision === 'reject' && !confirmReject) {
        // 驳回 = 布防一次 + 确认一次(会签驳回即判死;批准是主操作,单击直达)。
        setConfirmReject(true)
        return
      }
      void submit(decision)
    },
    [busy, selExec, selNode, actionable, confirmReject, submit],
  )

  // 光标按稳定审批项身份保留，索引只在本次结果里推导；移除时回首项。
  useEffect(() => {
    setCursorKey((key) => {
      if (items.length === 0) return null
      if (selExec !== null && selNode !== null) {
        const i = items.findIndex((it) => it.execution_id === selExec && it.node_id === selNode)
        if (i >= 0) return `${selExec}/${selNode}`
      }
      if (items.some((item) => `${item.execution_id}/${item.node_id}` === key)) return key
      return `${items[0].execution_id}/${items[0].node_id}`
    })
  }, [items, selExec, selNode])

  // 键盘:j/k 移动光标、Enter 打开、a 批准、r 驳回(布防/确认)、Esc 收确认
  // 或回队列。输入框聚焦时整段让位(见 isTypingTarget)。
  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      // 带修饰键的组合留给浏览器/系统;已被行级/确认层处理的事件不再抢。
      // ev.repeat 必须挡:按住 r 时第二次 keydown 落在「确认层已布防」的
      // 新闭包上,等于**长按即定局**——驳回落库不可改判,这是安全面
      // (T21.2 收尾评审 fix 1)。
      if (ev.defaultPrevented || ev.metaKey || ev.ctrlKey || ev.altKey || ev.repeat) return
      if (isTypingTarget(ev.target)) return

      if (ev.key === 'j' || ev.key === 'k') {
        if (items.length === 0 || selExec !== null) return
        ev.preventDefault()
        setCursorKey((key) => {
          const current = Math.max(0, items.findIndex((item) => `${item.execution_id}/${item.node_id}` === key))
          const next = items[ev.key === 'j' ? Math.min(current + 1, items.length - 1) : Math.max(current - 1, 0)]
          return `${next.execution_id}/${next.node_id}`
        })
        setFocusRequest((n) => n + 1)
        return
      }
      if (ev.key === 'Enter') {
        if (selExec !== null) return // 详情内 Enter 交给浏览器与确认层
        if (ev.target instanceof HTMLElement && ev.target.closest('button, a') !== null) return
        const it = cursor >= 0 && cursor < items.length ? items[cursor] : undefined
        if (it === undefined) return
        ev.preventDefault()
        setFlash('')
        onSelect({ executionId: it.execution_id, nodeId: it.node_id })
        return
      }
      if (ev.key === 'a' || ev.key === 'r') {
        if (selExec === null || !actionable) return
        ev.preventDefault()
        requestDecision(ev.key === 'a' ? 'approve' : 'reject')
        return
      }
      if (ev.key === 'Escape') {
        if (selExec === null) return
        ev.preventDefault()
        if (confirmReject) {
          setConfirmReject(false)
          return
        }
        onSelect(null)
        setFocusRequest((n) => n + 1)
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [items, cursor, selExec, actionable, confirmReject, requestDecision, onSelect])

  const open = useCallback(
    (sel: ApprovalSelection) => {
      setFlash('') // 反馈属于上一次决定:打开下一条即收起
      onSelect(sel)
    },
    [onSelect],
  )

  const prev = useCallback(() => setOffset((o) => Math.max(0, o - PAGE_LIMIT)), [])
  const next = useCallback(() => setOffset((o) => (o + PAGE_LIMIT < total ? o + PAGE_LIMIT : o)), [total])

  // 两级视图:列表级(未选中)= 队列占满全页;详情级 = 详情占满全页。
  // 在飞提交切走时的旧节点错误归队列错误条 —— 列表级回来即见,不误挂。
  return (
    <div className="appr-view">
      {selected === null ? (
        <ApprovalQueue
          items={items}
          total={total}
          offset={offset}
          loading={listLoading}
          error={queueError}
          flash={flash}
          selected={selected}
          cursor={cursor}
          onCursorChange={(sel) => setCursorKey(`${sel.executionId}/${sel.nodeId}`)}
          focusRequest={focusRequest}
          lastDecision={lastDecision}
          onOpen={open}
          onRefresh={reloadFromFirst}
          onPrev={prev}
          onNext={next}
        />
      ) : (
        <ApprovalDetail
          detail={detail}
          execution={execution}
          contextError={contextError}
          canDecide={actionable}
          decisionHint={!canDecide ? '当前角色不能提交决定；需要 approver/admin 角色，且账号必须在审批名单内。' : '当前账号已投票或不在本轮待投名单内。'}
          loading={detailLoading}
          error={detailErrorText}
          definitionName={definitionName}
          comment={comment}
          commentError={commentError}
          busy={busy}
          confirmReject={confirmReject}
          onCommentChange={setComment}
          onRequestDecision={requestDecision}
          onCancelReject={() => setConfirmReject(false)}
          onBack={() => { onSelect(null); setFocusRequest((n) => n + 1) }}
        />
      )}
    </div>
  )
}
