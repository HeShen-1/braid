// 常驻在壳层:草稿切换、主导航、浏览器前进/后退都经同一阻断点。
import { useEffect, useRef } from 'react'
import { useBlocker } from 'react-router'
import { ConfirmLayer } from '../ui/confirm.tsx'
import { useWorkflow } from './WorkflowProvider.tsx'

export function WorkflowLeaveGuard() {
  const { shouldConfirmLeave } = useWorkflow()
  const focusReturn = useRef<HTMLElement | null>(null)
  const layerRef = useRef<HTMLDivElement | null>(null)
  const blocker = useBlocker(({ currentLocation, nextLocation }) => {
    const block = currentLocation.pathname !== nextLocation.pathname && shouldConfirmLeave(nextLocation.pathname)
    if (block) focusReturn.current = document.activeElement as HTMLElement | null
    return block
  })
  const blocked = blocker.state === 'blocked'
  const wasBlocked = useRef(false)
  useEffect(() => {
    if (wasBlocked.current && !blocked && focusReturn.current?.isConnected) focusReturn.current.focus()
    wasBlocked.current = blocked
  }, [blocked])

  if (blocker.state !== 'blocked') return null
  return (
    <ConfirmLayer
      layerRef={layerRef}
      label="未保存的画布"
      hint="当前画布尚未保存。请返回保存，或确认继续离开。"
      confirmText="继续离开"
      onConfirm={blocker.proceed}
      onDismiss={blocker.reset}
    />
  )
}
