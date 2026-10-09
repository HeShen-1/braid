---
version: 1
slug: "web-canvas-src-app-tsx"
primary_target: "web/canvas/src/App.tsx"
related_targets: []
---

# Surface brief — braid 控制台（单 SPA，全部路由）

**Scope**：`/workflows/`（草稿列表 + 画布编辑器 + DSL 预览）、`/executions/`（列表 + 详情 + 时间带 + 回滚/重试/取消）、`/approvals/`（待办队列 + 详情 + 批准/驳回）、登录与首启空态。一个 SPA、一套壳、一套设计系统。
**Visitor mode**：Operate。

## Audience / job

author（编排）、approver（审批）、operator（观察与处置），三者共用桌面浏览器（24–27 寸，办公/机房混合光，夜间值班常态）。任务：把一张工作流的"现在到哪一步、卡了多久、下一步谁动"在秒级读出来，并当场处置。移动端不做。

## Chosen direction（用户 2026-10-09 拍板）

**深色仪器盘 / 控制室夜班**：近黑偏蓝绿的无光地面（未点亮的荧光屏），层级靠亮度台阶与发丝分割线，不靠阴影与发光。色相承载状态语义，形状做冗余编码。

## Direction contract

**THESIS**：让「卡在哪一步、卡了多久、下一步谁动」在秒级可读；拒绝通用深色后台那种「一个强调色 + 转圈动画」的等待表达——等待在这里是有长度、有纹理、有归属的东西。

**OWN-WORLD**：近黑偏蓝绿地面（未点亮的荧光屏），四段亮度台阶 + 1px 发丝线承担全部层级，零发光零投影；六态状态色相（idle/run/ok/wait/fail/stop）只做状态，交互一律靠亮度（主操作 = 亮底深字），焦点色是唯一非状态色相且不参与语义；状态色相 + 形状双编码；拉丁/数字自托管 Geist，CJK 走系统面；ID/计数/时长全程等宽数字。

**STORY**：打开就看见系统现在什么状态（脉冲条）；进执行详情一眼看出哪一段在等、等了多久、谁没占资源；在这一页当场回滚 / 续跑 / 取消，不需要回编辑器。

**FIRST VIEWPORT**：顶栏（标识 · 工作流/执行 · 运行脉冲 · 身份）→ 左列已发布工作流与执行列表 → 右主区执行头（状态 chip、定义与版本、TRACE_ID）→ **执行时间带**：每节点一行，横向按真实时间铺开，审批挂起段是整幅 135° 琥珀斜纹，底部一枚共享时间轴。主操作（回滚 / 续跑 / 取消）在时间带正上方。

**FORM**：用户 2026-10-09 从三案中直接拍板「深色仪器盘」（Q3 选 A）——**方向由用户钉死，故未跑 concept-seed 抽签，无 seed key、无 FORM 候选排序**；图像生成能力不在场，构建路径为 code-led，因此没有 comp 轮与 decision page，方向契约的 FIRST VIEWPORT 与签名动作承诺由收尾评审按行为审计。

**FINISH**：unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance.

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
