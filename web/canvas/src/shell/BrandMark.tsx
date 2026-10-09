/**
 * braid 标识:三股交织的线——产品名的字面形状,不是通用图标块。
 * 几何在模块加载时算一次(确定性),不依赖任何素材文件。
 */
const X0 = 9
const AMP = 4.6
const TOP = 1.6
const HEIGHT = 14.8

function strand(phase: number): string {
  const steps = 28
  let d = ''
  for (let i = 0; i <= steps; i++) {
    const t = i / steps
    const y = TOP + t * HEIGHT
    const x = X0 + AMP * Math.sin(t * Math.PI * 2 + phase)
    d += `${i === 0 ? 'M' : 'L'}${x.toFixed(2)} ${y.toFixed(2)}`
  }
  return d
}

const S0 = strand(0)
const S1 = strand((Math.PI * 2) / 3)
const S2 = strand((Math.PI * 4) / 3)

export function BrandMark({ size = 18 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 18 18" aria-hidden focusable="false">
      <g fill="none" strokeLinecap="round" strokeWidth={1.5}>
        <path d={S1} stroke="var(--color-ink-2)" />
        <path d={S2} stroke="var(--color-ink-3)" />
        <path d={S0} stroke="var(--color-accent)" />
      </g>
    </svg>
  )
}

export function Wordmark({ compact = false }: { compact?: boolean }) {
  return (
    <span className="flex items-baseline gap-2">
      <BrandMark size={17} />
      <span className="text-md font-semibold tracking-[-0.02em] text-ink-0">braid</span>
      {compact ? null : (
        <span className="hidden text-xs text-ink-2 lg:inline">Agent 工作流编排平台</span>
      )}
    </span>
  )
}
