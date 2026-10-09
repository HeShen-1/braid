// 校验/错误清单:后端 issues 数组的统一呈现(code · 节点 · 引用 — detail)。
import type { Issue } from '../api.ts'
import { TriangleAlert } from 'lucide-react'
import { I } from './I.tsx'
import { TID } from './testids.ts'

export function IssuesPanel({ issues }: { issues: Issue[] }) {
  if (issues.length === 0) return null
  return (
    <div
      className="alert m-3 shrink-0 items-start"
      data-tone="error"
      role="alert"
      data-testid={TID.issues}
    >
      <I glyph={TriangleAlert} size={14} className="mt-0.5 shrink-0" />
      <div className="min-w-0 flex-1">
        <p className="mb-1 font-semibold text-ink-0">操作未完成（{issues.length} 项）</p>
        <ul className="flex flex-col gap-0.5">
          {issues.map((iss, i) => (
            <li key={i} className="flex flex-wrap items-baseline gap-x-2 text-xs text-ink-1">
              <code className="mono text-ink-0">{iss.code}</code>
              {iss.node !== undefined && iss.node !== '' ? (
                <span>
                  节点 <code className="mono">{iss.node}</code>
                </span>
              ) : null}
              {iss.ref !== undefined && iss.ref !== '' ? (
                <span>
                  字段 <code className="mono">{iss.ref}</code>
                </span>
              ) : null}
              <span className="text-ink-2">{iss.detail}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  )
}
