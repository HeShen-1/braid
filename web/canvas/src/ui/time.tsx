// 时间面：控制台里时间必须是一等公民——相对时长（"卡了多久"）与
// 绝对时刻（"什么时候发生的"）同时在场，数字一律等宽对齐。
import { useEffect, useState } from 'react'

/** 每 interval 毫秒触发一次重渲染的"现在"（默认 1s）。 */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), intervalMs)
    return () => window.clearInterval(id)
  }, [intervalMs])
  return now
}

function pad(n: number, w = 2): string {
  return String(n).padStart(w, '0')
}

/** 本地绝对时刻：YYYY-MM-DD HH:MM:SS。 */
export function fmtDateTime(iso: string | null | undefined): string {
  if (iso === null || iso === undefined || iso === '') return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

/** 只到分钟：列表行里够用，且更短。 */
export function fmtMinute(iso: string | null | undefined): string {
  if (iso === null || iso === undefined || iso === '') return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const today = new Date()
  const sameDay =
    d.getFullYear() === today.getFullYear() && d.getMonth() === today.getMonth() && d.getDate() === today.getDate()
  return sameDay ? `${pad(d.getHours())}:${pad(d.getMinutes())}` : `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** 时长：按量级换单位，保留足以判断的精度。 */
export function fmtDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  const s = ms / 1000
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)}s`
  const m = Math.floor(s / 60)
  const rs = Math.round(s % 60)
  if (m < 60) return `${m}m${pad(rs)}s`
  const h = Math.floor(m / 60)
  const rm = m % 60
  if (h < 24) return `${h}h${pad(rm)}m`
  return `${Math.floor(h / 24)}d${pad(h % 24)}h`
}

/** 相对时长："3m12s 前"。 */
export function fmtAgo(iso: string | null | undefined, now: number): string {
  if (iso === null || iso === undefined || iso === '') return '—'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return iso
  const delta = now - t
  if (delta < 0) return `${fmtDuration(-delta)} 后`
  return `${fmtDuration(delta)} 前`
}

/** 绝对时刻 + 悬停相对：一个组件同时给两面。 */
export function Timestamp({ iso, relative = true }: { iso: string | null | undefined; relative?: boolean }) {
  const now = useNow(1000)
  if (iso === null || iso === undefined || iso === '') return <span className="num text-ink-3">—</span>
  const abs = fmtDateTime(iso)
  return (
    <time dateTime={iso} title={relative ? abs : undefined} className="num text-ink-2">
      {relative ? fmtAgo(iso, now) : abs}
    </time>
  )
}

/** 起止区间时长（未结束则算到"现在"）。 */
export function Duration({ from, to }: { from: string | null | undefined; to: string | null | undefined }) {
  const now = useNow(1000)
  if (from === null || from === undefined || from === '') return <span className="num text-ink-3">—</span>
  const a = new Date(from).getTime()
  if (Number.isNaN(a)) return <span className="num text-ink-3">—</span>
  const b = to === null || to === undefined || to === '' ? now : new Date(to).getTime()
  return (
    <span className="num" title={`${fmtDateTime(from)} → ${to ? fmtDateTime(to) : '进行中'}`}>
      {fmtDuration(b - a)}
    </span>
  )
}
