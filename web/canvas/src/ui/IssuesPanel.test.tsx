import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { IssuesPanel } from './IssuesPanel.tsx'

describe('审批人为空的保存提示', () => {
  it('互斥分支汇合明确指出节点、风险和修正方法', () => {
    render(<IssuesPanel issues={[{ code: 'branch_join', node: 'final_review', ref: 'route', detail: 'exclusive join' }]} />)
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('final_review')
    expect(alert).toHaveTextContent('route')
    expect(alert).toHaveTextContent('终审也不会执行')
    expect(alert).toHaveTextContent('请将必经审批放在各分支内')
  })
  it('指明节点与修正方法，技术信息默认折叠且可展开', async () => {
    const user = userEvent.setup()
    render(<IssuesPanel issues={[{ code: 'invalid_document', ref: '/nodes/2/approvers', detail: 'minItems: got 0, want 1' }]} />)
    expect(screen.getByRole('alert')).toHaveTextContent('第 3 个节点的审批人为空')
    expect(screen.getByText(/请在节点设置/)).toBeVisible()
    const raw = screen.getByText(/invalid_document ·/)
    expect(raw).not.toBeVisible()
    await user.click(screen.getByText('技术详情'))
    expect(raw).toBeVisible()
  })

  it.each([
    { code: 'invalid_document', ref: '/nodes/0/approvers', detail: 'uniqueItems: duplicate items' },
    { code: 'invalid_document', ref: '/nodes/0/prompt/template', detail: 'minLength: got 0, want 1' },
    { code: 'invalid_document', ref: '/nodes/0/outputs', detail: 'minItems: got 0, want 1' },
  ])('不把其他校验错误误译为审批人为空：$ref $detail', (issue) => {
    render(<IssuesPanel issues={[issue]} />)
    expect(screen.getByRole('alert')).toHaveTextContent(issue.detail)
    expect(screen.queryByText(/审批人为空/)).not.toBeInTheDocument()
  })
})
