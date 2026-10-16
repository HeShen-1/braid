# UX-F1-a 壳层与审批视觉整备验证

任务：#243；[PR #260](https://github.com/HeShen-1/braid/pull/260)。定义真源：ROADMAP M22-2；方向：D-2026-10-15-console-references。基线 `f57cdcf4a0190c921cf860c3e4b71dad76335c84`。

## 范围与候选

- 壳层：48px 顶部域导航与身份、46px 路由条；认证路由唯一可见 `h1`。暗色采用中性近黑，语义 token 名、浅色/深色/跟随系统三态和 Bearer/sessionStorage 纪律保留。
- 审批：44px 紧凑队列行、上方可见 j/k/Enter；详情细线分节、按可决定状态显示 a/r，保留 Esc 返回。沿用 T23.10 的 URL、稳定焦点、轮询、决定防重、备注与角色/名单逻辑。
- 实测门暴露并修复三项样式责任层问题：shadcn 别名覆盖焦点蓝、浅色主操作实体过弱、SelectItem 的 outline-hidden 压过键盘焦点边界。没有后端、迁移、engine、CI、依赖、账号或状态机变动。
- 最后产品源与完整 dist 在 `267a7959fe96dbc05826791f76c7d6a033eecd9b`。之后 `e250d3e` 只修工作流名夹具；`9603fcb0367e4c6e767474b59d2f45aa08e29e9e` 只修新增回归事件起点。两段逐对象等价证明分别为 `fixture-only-equivalence.json`、`event-only-equivalence.json`（各确认其余 705 个树对象相同）。不宣称这些后续头重跑了 Go/334 项单测。

## 本机验收

证据根：`/tmp/braid-m23-wave6-20261016/ux-f1-a/`；最终归档/校验清理记录由主会话持有。

| 门 | 实际执行 revision 与结果 | 证明边界 |
|---|---|---|
| 独立 `npm ci` | 首轮退出 0；Node v26.10.0、独立 node_modules | 安装日志在 agent/ |
| vitest / shadcn 清单 | 267a：27 文件、334 测试，退出 0 | jsdom/fetch doubles，不冒充后端或浏览器 |
| build / build:verify / 正式 dist 门 | 267a：均退出 0，13 文件一致；后续产品对象等价 | 完整构建，不手拼另一树产物；既有 >500kB chunk 提示保留 |
| 静态 token 对比度 | 双主题各 15 对全过；另 1 对禁用态登记 | 当前暗色正文最紧 5.34/图形 3.85，浅色 4.90/3.01；不替代浏览器像素 |
| 真栈 `go test ./...` | 267a：2954 test/subtest pass、唯一 TestMultiReplicaDrill 显式 opt-in skip；test/gates 退出 0 | 主会话持锁、独立栈；原生 JSONL 与 cleanup 在 go-267a.../ |
| `go vet ./...` / `golangci-lint run` | 267a：退出 0；vet.log 空、lint 为 0 issues | 后续 fixture/event-only revision 按逐对象证明复用 |
| 正式嵌入 dist 官方 Playwright | 9603：21/21、playwright.exit 0 | 真账号、API、worker；零模型调用。含实际 author TriggerDefinition Select 回归 |
| 独立六视角与真实 UI | 9603：开放 C/H/M/L=0；R4 真实 Select 四组合关闭，visual-complete=pass | 独审逐份核原始 DOM/computed、图及实际像素；review/R4-CLOSURE-9603fcb.md |

官方本项截图为 8 张审批队列/详情图，加 4 张真实 TriggerDefinition Select 键盘图（两主题 × 1440/1024，窗口高度均为 1000）。原 4.5 正文、3 图形/焦点门未降低；新增 Select 还要求真实 ArrowDown 到第二个可用项、与鼠标 hover 项不同、solid/至少 2px/负 offset 与 Esc 回触发器。

独立 Select 补测保持正常动效（`no-preference=true`、`reduce=false`），实际窗口与结果如下；未把 1000 高的证据改写成 800：

| 真实组合 | 独立取证与核读 | 环 / 正文最低对比度 |
|---|---|---|
| dark 1440×800 | 独立 reviewer 直接操作 | 4.488742:1 / 5.077930:1 |
| dark 1024×1000 | D 独立会话；reviewer 核原件/图/像素 | 4.488742:1 / 5.077930:1 |
| light 1440×1000 | D 独立会话；reviewer 核原件/图/像素 | 3.538408:1 / 5.494559:1 |
| light 1024×1000 | D 稳定补批次；reviewer 核原件/图/像素 | 3.538408:1 / 5.494559:1 |

四组均为鼠标 hover 首 enabled 且真实 focused，单次 ArrowDown 到第二 enabled；首仍 hover、次 focus-visible 且非 hover，solid 2px/-2px 内环实绘且未裁切，Esc 回到触发器。早先 light1024 停首项的中间尝试保留，不计通过。壳层、审批、20 行队列与主操作沿用 1457 已过且相关源未变的正常动效 800 高矩阵；这次只补 Select 差异，不冒充重跑全矩阵。两隔离会话关闭、稳定 list=[] 原生 0；官方同栈随后完整 cleanup、gates 退出 0。未覆盖 screen reader、物理设备或无实际业务调用的 dropdown 变体。

通过轮 reporter=list 没有把 info.attach 的 body contrast JSON 单独落盘；不声称 8/12 份原始 JSON 已归档。精确真实运行时数值引用独立 UI 的 DOM/computed JSON；失败 trace 含原测量并保留。`agent/controls-final.json` 的 46 状态正常动效稳态矩阵与 Select 人工 DOM 红绿，仅是诊断样本，不替代正式 SPA/API/CSP/组件验收。样本中两条 disabled 按原口径登记，可用主操作从未豁免。

## 失败与修复历史

| 候选 / 发现 | 当时证据与结果 | 修复及边界 |
|---|---|---|
| a19d 首轮节点夹具 | 官方 19/20；新节点 id 含连字符，创建 API 400 invalid_dsl | 3a 改为 compliance_review_gate；工作流名与 node id 是两套 schema 契约 |
| R1，a19d 独审 LOW | DESIGN 当前表仍有旧 line-0/ink-0 与 4.00:1 | 3a 统一新值与 4.71:1，历史 T21.3 测量保留归属 |
| R2，3a | 官方 19/20；暗色队列焦点实测 1.223:1 | 19be 删除 shadcn 同名 accent 覆盖；输入/主题等环统一 85%；保留菜单中性底 |
| R3，19be | 官方 19/20；浅色批准实体与相邻地面 1.273:1 | 1457 浅色主操作改深实体/浅字；人工 DOM 的 46 状态诊断覆盖 normal/hover/active，真实 SPA 独审证明 normal/hover/focus（未把 active 提交截图作为门）；无可用按钮豁免 |
| R4，1457 | 官方原 20/20、playwright 0；独立真实 ArrowDown UI fail，整轮 gates 1 | SelectItem :focus-visible=true 但 outlineStyle=none；hover 与 keyboard 项同 ground-3。267a 仅为 SelectItem 增加 solid 2px/85%accent/-2px 内侧环，不扩 dropdown |
| R5，267a | 独审在起新浏览器前发现 Select workflow name 含下划线 | e250 单模板改合法连字符；读真实 schema 校验，未故意向栈发送该非法夹具 |
| R6，e250 | 官方 20/21；新 case 在 hover!=focus 处失败，尚未测到环 | 三帧 DOM 证实 hover 首 enabled→Home disabled placeholder→Down 回首 enabled。9603 用首 enabled 真实 focus 作前置，单 ArrowDown 后明确第二 enabled focused；所有原阈值/不等断言保留，没有 DOMfocus 或按键重试，也未改底层库 Home 行为 |

旧失败目录 e2e-a19d.../、e2e-3a.../、e2e-19be.../、e2e-1457.../、e2e-e250.../，以及 review/ 中各 revision 的原 diff/native receipt/补审报告均保留。R5 规则检查、R6 三帧 DOM 与库代码诊断分别保留；初次 dist 门参数倒置退出 1、本地探针 sandbox 启动失败也保留，均不计作产品通过。

## 准出边界

最终纯 Markdown 收口后，由主会话核产品对象等价、复审文档、核最终头的 GitHub 七门全部 COMPLETED/SUCCESS 且 gh pr checks 原生退出 0，再执行合并、删除并核验远程分支、独有证据归档校验与本地 worktree 清理。本文记录本机验收，不提前宣称这些尚未执行动作完成。

本项没有付费/真实模型调用，也没有改既有 dev/prod 数据。正式 Compose 与组件账号的完整业务/真实模型旅程仍归 T23.13，本项不将其写为通过。
