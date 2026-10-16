// 危险动作确认层的单一实现(一次性浮起层):执行面「取消执行」、审批面
// 「驳回」、画布「删除节点」三处共用同一契约 —— 布防即把焦点移进浮起层
// (否则按快捷键只产生视觉变化,读屏用户不会被通知),Esc / 放弃 收起,
// 收起后焦点回到入口按钮(T21.4 增项:原先焦点掉回 body,键盘用户要从头
// Tab 一遍才能回到刚才那个危险按钮上)。
import { useCallback, useEffect, useRef, useState, type ReactNode, type RefObject } from 'react'
import { Button } from './primitives.tsx'
import { cx } from './cx.ts'

/**
 * 焦点回归:armed 由 true 翻回 false 的那一帧把焦点还给入口按钮。
 * 入口按钮与确认层是互斥渲染(布防时入口按钮不在场),所以必须等这一帧
 * 提交后再聚焦 —— 这也是它必须做成 effect 而不是写在 dismiss 回调里的原因。
 *
 * 两处已知边界(收尾审计 M1,有意不做静默兜底):
 * - 入口**禁用**时(toggle 类入口:取消执行成功后 canCancel 已翻 false)focus()
 *   在浏览器里是 no-op —— 显式判掉,别假装回了焦点;
 * - 入口**已卸载**时(画布删除成功 = 节点连同面板一起消失)没有可回之处,
 *   焦点留在 body 是「操作对象消失」的正常结果,不硬塞给别的元素。
 */
export function useFocusReturn<T extends HTMLElement>(armed: boolean): RefObject<T | null> {
  const ref = useRef<T | null>(null)
  const wasArmed = useRef(armed)
  useEffect(() => {
    if (wasArmed.current && !armed) {
      const el = ref.current
      if (el !== null && !(el instanceof HTMLButtonElement && el.disabled)) el.focus()
    }
    wasArmed.current = armed
  }, [armed])
  return ref
}

/** 组件自持确认态时的成套件;状态在父层时父层用 useState + useFocusReturn。 */
export function useConfirmLayer() {
  const [armed, setArmed] = useState(false)
  const layerRef = useRef<HTMLDivElement | null>(null)
  const armRef = useFocusReturn<HTMLButtonElement>(armed)
  return {
    armed,
    arm: useCallback(() => setArmed(true), []),
    disarm: useCallback(() => setArmed(false), []),
    layerRef,
    armRef,
  }
}

/**
 * 确认条本体:role=group + aria-label 是布防的对外信号(测试与读屏都靠它),
 * 挂载即聚焦(tabIndex=-1)。Esc 走两条路:
 * - 焦点在层内 → 层自己的 keydown(stopPropagation,不与画布/队列的快捷键打架);
 * - 焦点被用户点到层外(点画布、点右栏开关、再点一次入口)→ 层上挂的
 *   document 监听兜底(Esc 是这一层的契约,不该因为焦点离开就失效 ——
 *   收尾审计 M2 的真机面)。父层若也听 Escape(审批台),其处理器仍会跑,
 *   语义幂等:两边都只是 setArmed(false)/(confirmReject=false)。
 */
export function ConfirmLayer({
  layerRef,
  label,
  hint,
  confirmText,
  confirmTestId,
  testId,
  busy = false,
  confirmDisabled = false,
  className,
  onConfirm,
  onDismiss,
}: {
  layerRef: RefObject<HTMLDivElement | null>
  /** 浮起层的无障碍名(= 布防信号),同时用于测试查询。 */
  label: string
  hint: ReactNode
  confirmText: string
  confirmTestId?: string
  /** 层本体的 testid(需要按 id 查的调用点才传;其余靠 role+name)。 */
  testId?: string
  busy?: boolean
  /** 读取失败/刷新期间只禁确认，仍可放弃或 Esc 退出。 */
  confirmDisabled?: boolean
  className?: string
  onConfirm: () => void
  onDismiss: () => void
}) {
  useEffect(() => {
    layerRef.current?.focus()
  }, [layerRef])
  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === 'Escape') onDismiss()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onDismiss])
  return (
    <div
      ref={layerRef}
      tabIndex={-1}
      className={cx('confirm-bar', className)}
      role="group"
      aria-label={label}
      data-testid={testId}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          // 层内这条路自己收(幂等);document 那条只在焦点已离开层时兜底。
          e.stopPropagation()
          onDismiss()
        }
      }}
    >
      <span className="text-xs text-ink-1">{hint}</span>
      <Button variant="danger" size="sm" disabled={busy || confirmDisabled} data-testid={confirmTestId} onClick={onConfirm}>
        {confirmText}
      </Button>
      <Button variant="quiet" size="sm" disabled={busy} onClick={onDismiss}>
        放弃
      </Button>
    </div>
  )
}
