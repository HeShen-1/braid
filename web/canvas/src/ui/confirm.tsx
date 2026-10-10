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
 */
export function useFocusReturn<T extends HTMLElement>(armed: boolean): RefObject<T | null> {
  const ref = useRef<T | null>(null)
  const wasArmed = useRef(armed)
  useEffect(() => {
    if (wasArmed.current && !armed) ref.current?.focus()
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
 * 挂载即聚焦(tabIndex=-1),Esc 收起且不外泄(避免与画布/队列的快捷键打架)。
 */
export function ConfirmLayer({
  layerRef,
  label,
  hint,
  confirmText,
  confirmTestId,
  testId,
  busy = false,
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
  className?: string
  onConfirm: () => void
  onDismiss: () => void
}) {
  useEffect(() => {
    layerRef.current?.focus()
  }, [layerRef])
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
          e.stopPropagation()
          onDismiss()
        }
      }}
    >
      <span className="text-xs text-ink-1">{hint}</span>
      <Button variant="danger" size="sm" disabled={busy} data-testid={confirmTestId} onClick={onConfirm}>
        {confirmText}
      </Button>
      <Button variant="quiet" size="sm" disabled={busy} onClick={onDismiss}>
        放弃
      </Button>
    </div>
  )
}
