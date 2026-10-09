# Surface brief — braid 控制台（单 SPA，全部路由）

**Scope**：`/workflows/`（草稿列表 + 画布编辑器 + DSL 预览）、`/executions/`（列表 + 详情 + 时间带 + 回滚/重试/取消）、`/approvals/`（待办队列 + 详情 + 批准/驳回）、登录与首启空态。一个 SPA、一套壳、一套设计系统。
**Visitor mode**：Operate。

## Audience / job

author（编排）、approver（审批）、operator（观察与处置），三者共用桌面浏览器（24–27 寸，办公/机房混合光，夜间值班常态）。任务：把一张工作流的"现在到哪一步、卡了多久、下一步谁动"在秒级读出来，并当场处置。移动端不做。

## Chosen direction（用户 2026-10-09 拍板）

**深色仪器盘 / 控制室夜班**：近黑偏蓝绿的无光地面（未点亮的荧光屏），层级靠亮度台阶与发丝分割线，不靠阴影与发光。色相承载状态语义，形状做冗余编码。

## Memorable moment（signature move）

**执行时间带（Execution Ribbon）**：每个节点一行，横向按真实时间铺开，状态是实心色块，**审批挂起段用斜纹填充**——等待在图上是有长度的东西，一眼看出"资源没被占用，但时间在走"。这是 braid"挂起不占 worker"机制的可见形态，同类产品抄不走。

## Constraints

- CSP：`default-src 'none'`，脚本/样式/图片/连接全部同源；只新增 `font-src 'self'`。禁内联脚本与内联样式属性。
- 零后端新端点；状态与字段全部来自既有 API 面。
- 认证：Bearer + sessionStorage；401 统一登出。
- 桌面优先；窄窗不横向溢出即可。
- 全中文；API 英文资源名保留原样。
- 表格数据一律 tabular numerals。

## Unresolved

- 执行时间带的横向时间轴刻度密度（随执行时长自适应，实现时定）。
- 是否在 T21.1 内做键盘快捷键（j/k 审批）——**T21.2 做**，T21.1 只落地焦点管理与 Esc。
