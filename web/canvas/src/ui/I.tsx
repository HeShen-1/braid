import type { LucideIcon, LucideProps } from 'lucide-react'

/**
 * 图标统一入口：一个尺寸（14px）、一个笔画（1.75）、默认装饰性。
 * 图标不单独承载语义——旁边的文字或 aria-label 才是语义来源。
 */
export function I({ glyph: Glyph, size = 14, ...rest }: { glyph: LucideIcon; size?: number } & Omit<LucideProps, 'size' | 'ref'>) {
  return <Glyph size={size} strokeWidth={1.75} aria-hidden focusable={false} {...rest} />
}
