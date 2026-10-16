# UX-F1-a 壳层与审批视觉整备验证

任务：#243；定义真源：ROADMAP M22-2，方向：D-2026-10-15-console-references。基线 `f57cdcf4a0190c921cf860c3e4b71dad76335c84`。

## 改动范围

- 壳层：48px 顶部域导航与身份、46px 路由条；每个认证路由只有一个可见 `h1`。暗色改为中性近黑，语义 token 名、浅色档与三态主题机制保留。
- 审批：紧凑行式待办队列、顶部可见 `j/k/Enter` 提示；详情以分节细线组织，按可决定状态外显 `a/r`，保留 `Esc` 返回提示。
- 行为不重复实现：沿用 T23.10 的 URL、焦点、轮询、决定防重、备注校验、角色/名单与只读边界。无后端、迁移、engine、CI、依赖变更。
- 前端入库 dist 由本 worktree 完整构建；不拼接另一任务的产物。

## 本地源码验证

2026-10-16，Node `v26.10.0`，本 worktree 独立 `node_modules`。

| 门 | 结果 | 边界 |
|---|---|---|
| `npm --prefix web/canvas ci` | 退出 0 | 独立依赖安装 |
| `npm --prefix web/canvas test` | 退出 0；27 文件、333 测试 | jsdom/fetch doubles；不是后端或浏览器验收 |
| `npm --prefix web/canvas run build` | 退出 0 | TypeScript + 正式 dist；已有 >500kB chunk 提示仍在 |
| `npm --prefix web/canvas run build:verify` | 退出 0 | 独立重建；正式 dist 门须在提交后核验 |
| `node .impeccable/contrast.mjs` | 退出 0；双主题各 15 对全过，另 1 对禁用态登记 | 静态 token；暗色最紧正文 5.34/图形 3.85，浅色 4.90/3.01 |

证据目录：`/tmp/braid-m23-wave6-20261016/ux-f1-a/agent/`。首次漂移门误把 verify 目录作为 dist 参数，退出 1；保留 `dist-gate-initial.log`，不把该次当门通过。之后使用正式默认参数，在候选提交后重验。

## 待准出门

以下在首个产品冻结提交时仍待执行，不能据上表宣称任务完成：

- 主会话持锁串行的真栈 `go test ./...`、`go vet ./...`、`golangci-lint run`。
- 正式 Go 嵌入 dist 与真实 API/worker 的全量 Playwright；本项独立 `console-shell-approval.e2e.ts` 覆盖两主题 × 1440/1024 的队列/详情、实际渲染正文/控件/焦点对比度、路由标题、键盘决定与 8 张截图。该 spec 使用自动化验收数据和真实内置账号，不调用模型。
- 独立六视角审查与设计/截图核对、最终候选七项 GitHub CI、合并后的远程分支及本地证据归档/清理核验。

验证结果与失败历史将在对应候选完成后补记；正式配置完整业务旅程仍归 T23.13。
