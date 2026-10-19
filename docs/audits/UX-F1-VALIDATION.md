# UX-F1 / #227 父票收口验证

本记录只覆盖 UX-F1 父票 #227 的汇总收口。它不是第五条实现线：四个视觉子项及其画布前置已经分别交付，本票只核对父票要求的跨页一致性、更新 ROADMAP 汇总行并关闭父 issue。基线为 `bfe0cf57cfe90598551824c2ae779659d7fb80af`。

## 写入面与契约

开工前核对了 `docs/ARCHITECTURE.md`、`docs/ROADMAP.md`、`docs/plans/M23-NEXT-WAVES-2026-10-15.md`、`docs/DECISIONS.md` 中的 `D-2026-10-15-console-references` 与 `D-2026-10-16-ux-f2-edge-sides`，并读取 live issues #227/#243/#244/#245/#246/#228/#229。

本票允许的写入面只有：

- `docs/ROADMAP.md` 的 UX-F1 父行：四子项已完成、跨页核对已完成，并链接本记录；
- 本记录；
- GitHub issue #227 的子票清单与关闭状态。

没有修改源码、DSL/schema、API、engine、迁移、CSS/token、dist、依赖或锁定决策。现有 D- 决策继续有效；父票收口不需要新的 D-确认。

## 子项与前置闭环

| 范围 | 合并/状态 | 证据 |
| --- | --- | --- |
| #243 / UX-F1-a 壳层与审批 | PR #260，merge `371a10ac`，issue CLOSED | [UX-F1-a-VALIDATION](UX-F1-a-VALIDATION.md) |
| #245 / UX-F1-c 执行总览 | PR #262，merge `a328b818`，issue CLOSED | [UX-F1-c-VALIDATION](UX-F1-c-VALIDATION.md) |
| #246 / UX-F1-d 执行列表 | PR #261，merge `0ad6bd16`，issue CLOSED | [UX-F1-d-VALIDATION](UX-F1-d-VALIDATION.md) |
| #228 / UX-F2 画布行为前置 | PR #263，merge `22b1026`，issue CLOSED | [UX-F2-VALIDATION](UX-F2-VALIDATION.md) |
| #229 / UX-F3 删除发现性前置 | PR #258，merge `57eef401`，issue CLOSED | [UX-F3-VALIDATION](UX-F3-VALIDATION.md) |
| #244 / UX-F1-b 画布视觉 | PR #264，merge `bfe0cf57`，issue CLOSED | [UX-F1-b-VALIDATION](UX-F1-b-VALIDATION.md) |

Live issue 状态显示四个父票子项均 CLOSED，且 #228/#229 两个画布前置也 CLOSED；没有开放的同范围 PR 或待认领分支。

## 跨页一致性核对

| 面 | 核对内容 | 结果 |
| --- | --- | --- |
| 主题 | 运营面无偏好深色、工作流画布无偏好浅色；用户明确选择仍由同一 `localStorage` 偏好全局持久化；子项双主题正文/图形对比度门均已留证 | 通过；没有第二套 token 语言或路由覆盖偏好 |
| 导航 | 壳层/审批、总览、执行列表、画布复用同一 AppShell 路由条、身份区和返回/选择语义；各子项分别验证路由标题、URL 选择、加载/错误出口 | 通过；无新增导航或路由契约 |
| 焦点 | 壳层/审批的 j/k/Enter/Esc、执行列表 tab/行焦点、画布节点/面板/删除确认的鼠标与键盘路径均由各专项证据覆盖 | 通过；没有因视觉子项重写焦点行为 |
| 画布交互 | #228 的四向端点、保存/重开、节点防堆叠和 #229 的删除确认与 DSL 无残留，与 #244 的点阵/彩型视觉叠加；DSL/metadata 语义保持兼容 | 通过；没有新增或重载 `source_anchor` 语义 |
| 视觉边界 | Linear/Vercel/Temporal/n8n 只按已批准分面取长；状态色、类型色、焦点蓝的责任边界在 `DESIGN.md` 与子项记录中保持一致 | 通过；父票没有扩大设计方向 |

## 验证边界

本票没有重新执行四个子项的业务旅程；它消费各子项最终提交、验证记录和已合并 PR 的原始证据，并以新的父票文档/ROADMAP 改动触发本票自己的七门。这里的“通过”表示父票汇总契约与证据链闭合，不扩张为 T23.13/#242 正式配置独立环境、真实供应商模型调用或自然人用户验收。

父 issue 在本 PR 合并后更新为四项全勾并关闭。独有父票证据、最终头/base、七门 receipt、issue/远支核验和 worktree 清理记录归档在 `.handoff/archive/2026-10-19-ux-f1-parent/`。
