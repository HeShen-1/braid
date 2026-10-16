import { useCallback, useEffect, useRef, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { ApiError, api, type ExecutionSummary } from './api.ts'
import { bucketWidthText, creationDistribution, OVERVIEW_SAMPLE_LIMIT, overviewTime } from './executionOverview.ts'
import { CONSOLE_REFRESH, LIVE_REFRESH_MS } from './refresh.ts'
import { apiErrorText } from './ui/errors.ts'
import { Button, StatusChip } from './ui/primitives.tsx'
import './styles/execution-overview.css'

const METRICS = [
  { key: 'all', status: undefined, label: '保留执行总数' },
  { key: 'running', status: 'RUNNING', label: '执行中' },
  { key: 'waiting', status: 'WAITING_APPROVAL', label: '等待审批' },
  { key: 'failed', status: 'FAILED', label: '已失败' },
] as const
type MetricKey = typeof METRICS[number]['key']
type Reading<T> = { value: T | null; at: number | null; error: string }
const unread = <T,>(): Reading<T> => ({ value: null, at: null, error: '' })

export function ExecutionOverview({ onAuthExpired }: { onAuthExpired: () => void }) {
  const [counts, setCounts] = useState<Record<MetricKey, Reading<number>>>(() => ({ all: unread(), running: unread(), waiting: unread(), failed: unread() }))
  const [sample, setSample] = useState<Reading<ExecutionSummary[]>>(unread)
  const [refreshing, setRefreshing] = useState(true)
  const inFlight = useRef(false)
  const generation = useRef(0)

  const refresh = useCallback(async () => {
    if (inFlight.current) return
    inFlight.current = true
    const seq = ++generation.current
    setRefreshing(true)
    const results = await Promise.allSettled(METRICS.map(({ status }) => api.listExecutions({ status, limit: status === undefined ? OVERVIEW_SAMPLE_LIMIT : 1, offset: 0 })))
    if (seq !== generation.current) return
    const at = Date.now()
    setCounts((previous) => {
      const next = { ...previous }
      results.forEach((result, index) => {
        const key = METRICS[index].key
        if (result.status === 'fulfilled') {
          const total = result.value.page?.total
          next[key] = Number.isSafeInteger(total) && total >= 0
            ? { value: total, at, error: '' }
            : { ...previous[key], error: '响应缺少有效总数，请重试。' }
        } else next[key] = { ...previous[key], error: apiErrorText(result.reason) }
      })
      return next
    })
    const all = results[0]
    setSample((previous) => all.status === 'fulfilled'
      ? Array.isArray(all.value.items)
        ? { value: all.value.items.slice(0, OVERVIEW_SAMPLE_LIMIT), at, error: '' }
        : { ...previous, error: '响应缺少执行样本，请重试。' }
      : { ...previous, error: apiErrorText(all.reason) })
    const unauthorized = results.some((result) => result.status === 'rejected' && result.reason instanceof ApiError && result.reason.status === 401)
    inFlight.current = false
    setRefreshing(false)
    if (unauthorized) onAuthExpired()
  }, [onAuthExpired])

  useEffect(() => {
    void refresh()
    const interval = window.setInterval(() => void refresh(), LIVE_REFRESH_MS)
    const changed = () => void refresh()
    window.addEventListener(CONSOLE_REFRESH, changed)
    return () => {
      generation.current++
      inFlight.current = false
      window.clearInterval(interval)
      window.removeEventListener(CONSOLE_REFRESH, changed)
    }
  }, [refresh])

  const distribution = sample.value === null ? null : creationDistribution(sample.value)
  const failed = Object.values(counts).some((count) => count.error !== '') || sample.error !== ''
  const peak = Math.max(0, ...distribution?.buckets.map((bucket) => bucket.count) ?? [])

  return (
    <section className="exec-overview" aria-label="执行总览" aria-busy={refreshing}>
      <header className="exec-overview-head">
        <div><h2>执行总览</h2><p>当前租户 · 全部工作流 · 不受列表筛选影响</p></div>
        <div className="exec-overview-refresh"><span>每 5 秒自动刷新</span><Button size="sm" variant="quiet" icon={RefreshCw} disabled={refreshing} onClick={() => void refresh()} aria-label="刷新执行总览">{refreshing ? '正在刷新…' : '刷新'}</Button></div>
      </header>
      <div className="exec-overview-body">
        <dl className="exec-overview-metrics" aria-label="当前租户执行状态计数">
          {METRICS.map(({ key, status, label }) => {
            const reading = counts[key]
            return <div className="exec-overview-metric" key={key}>
              <dt>{status === undefined ? label : <StatusChip status={status} />}</dt>
              <dd aria-label={`${label}：${reading.value === null ? '未知' : `${reading.value}条`}`}><span className="exec-overview-number">{reading.value === null ? '—' : reading.value.toLocaleString('zh-CN')}</span>{reading.value !== null ? <span className="exec-overview-unit">条</span> : null}</dd>
              {reading.error !== '' ? <p className="exec-overview-error">读取失败：{reading.error}{reading.at !== null ? ` 保留 ${overviewTime(reading.at)} UTC 的值。` : ''}</p>
                : <p>{reading.at === null ? '等待读取' : `读取于 ${new Date(reading.at).toISOString().slice(11, 19)} UTC`}</p>}
            </div>
          })}
        </dl>
        <div className="exec-overview-distribution">
          <h3>最近 {OVERVIEW_SAMPLE_LIMIT} 条的创建分布</h3>
          <p>按创建时间倒序取样 · 条/桶 · 不代表固定时间窗</p>
          {sample.error !== '' ? <p className="exec-overview-error">创建分布读取失败：{sample.error}{sample.at !== null ? ` 保留 ${overviewTime(sample.at)} UTC 的样本。` : ''}</p> : null}
          {distribution === null ? <p className="exec-overview-empty">{sample.error !== '' ? '样本未知，请刷新重试。' : '正在读取执行样本…'}</p>
            : distribution.sampleCount === 0 ? <p className="exec-overview-empty">暂无执行样本。触发执行后将按真实创建时间显示分布。</p>
            : <>
              <p className="exec-overview-sample">已取 {distribution.sampleCount} 条 · 有效时间 {distribution.sampleCount - distribution.invalidCount} 条{distribution.invalidCount > 0 ? ` · 缺失或无效时间 ${distribution.invalidCount} 条，未计入分布` : ''}</p>
              {distribution.buckets.length === 0 ? <p className="exec-overview-empty">时间点不足，暂不绘制分布。至少需要两个不同的有效创建时间。</p> : <>
                <div className="exec-overview-chart-label"><span>每桶 {bucketWidthText(distribution.bucketMs)}</span><span>最高 {peak} 条/桶</span></div>
                <svg className="exec-overview-chart" viewBox="0 0 600 100" preserveAspectRatio="none" role="img" aria-label={`创建时间分布，${distribution.buckets.length}个等宽时间桶，每桶${bucketWidthText(distribution.bucketMs)}，最高${peak}条`}>
                  <line x1="0" y1="99" x2="600" y2="99" className="exec-overview-axis" />
                  {distribution.buckets.map((bucket, index) => <rect key={bucket.start} x={index * 600 / distribution.buckets.length + 2} y={99 - bucket.count / peak * 94} width={600 / distribution.buckets.length - 4} height={bucket.count / peak * 94} className="exec-overview-bar"><title>{overviewTime(bucket.start)} ≤ 创建时间 &lt; {overviewTime(bucket.end)} UTC：{bucket.count} 条</title></rect>)}
                </svg>
                <div className="exec-overview-range"><time>{overviewTime(distribution.buckets[0].start)}</time><time>{overviewTime(distribution.buckets.at(-1)!.end)}</time></div>
                <p>横轴：桶边界（UTC）；纵轴：样本条数，从 0 起。</p>
                <details className="exec-overview-buckets"><summary>查看各时间桶</summary><table><caption>创建时间桶（UTC，含起点、不含终点）</caption><thead><tr><th scope="col">开始</th><th scope="col">结束</th><th scope="col">条数</th></tr></thead><tbody>{distribution.buckets.map((bucket) => <tr key={bucket.start}><td>{overviewTime(bucket.start)}</td><td>{overviewTime(bucket.end)}</td><td>{bucket.count}</td></tr>)}</tbody></table></details>
              </>}
              {distribution.earliest !== null ? <p className="exec-overview-sample-range">样本创建范围（UTC）：{overviewTime(distribution.earliest)} 至 {overviewTime(distribution.latest!)}</p> : null}
            </>}
        </div>
      </div>
      <p className="exec-overview-note" role="status">{failed ? '部分总览数据读取失败；请用“刷新执行总览”重试，旧值已标明读取时间。' : '状态卡为分别读取的当前保留执行数，数值可能短暂不同步；创建分布仅描述本次样本。'}</p>
    </section>
  )
}
