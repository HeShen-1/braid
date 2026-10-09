// 主题控件:三态按钮组(auto / light / dark)。
//
// 这里刻意**不是**开关:三颗按钮各自是一个偏好设定,当前值用 aria-pressed
// 表达,视觉走 .seg 的选中态(亮度台阶,不引入新色相)。深色是默认世界,
// 但「默认」不等于「高亮那一颗」——高亮永远跟着真实偏好走。
import type { LucideIcon } from 'lucide-react'
import { Monitor, Moon, Sun } from 'lucide-react'
import { useTheme, type Theme } from '../theme.ts'
import { cx } from '../ui/cx.ts'
import { I } from '../ui/I.tsx'
import { TID, tid } from '../ui/testids.ts'

const OPTIONS: readonly { value: Theme; label: string; icon: LucideIcon }[] = [
  { value: 'auto', label: '跟随系统', icon: Monitor },
  { value: 'light', label: '浅色', icon: Sun },
  { value: 'dark', label: '深色', icon: Moon },
]

export function ThemeControl({ className }: { className?: string }) {
  const { theme, setTheme } = useTheme()
  return (
    <div className={cx('seg', className)} role="group" aria-label="主题" data-testid={TID.themeControl}>
      {OPTIONS.map(({ value, label, icon }) => {
        const active = theme === value
        return (
          <button
            key={value}
            type="button"
            className="seg-item"
            aria-pressed={active}
            title={label}
            data-testid={tid.themeOption(value)}
            onClick={() => setTheme(value)}
          >
            <I glyph={icon} size={13} />
            <span className="sr-only">{label}</span>
          </button>
        )
      })}
    </div>
  )
}
