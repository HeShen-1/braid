# UX-F1-b / #244 画布 n8n 化验证

本记录只覆盖 UX-F1-b/#244。实现分支为 `feat/ux-f1-b`，工作树为 `/tmp/braid-ux-f1-b`，基线为当前 `master` 的 22b1026。#228 的连线与排版行为已在前置票交付，本票没有重写其 DSL、schema、converter、API、迁移或边端点语义。

## 写入面与契约

开工前由独立代理核对 `docs/ARCHITECTURE.md`、`docs/ROADMAP.md`、`docs/DECISIONS.md:1004-1009`、`docs/plans/M23-NEXT-WAVES-2026-10-15.md:34-57` 及真实源码。写入面限定为：

- `web/canvas/src/CanvasEditor.tsx`：n8n 点阵地面；
- `web/canvas/src/Palette.tsx`、`web/canvas/src/styles/canvas-editor.css`：类型识别与节点彩型；彩型 token 只在 `.editor` 作用域生效，执行详情的只读 `DefinitionGraph` 保持中性 fallback；
- `web/canvas/src/theme.ts`、`main.tsx`、`AppShell.tsx`、`ThemeControl.tsx`、`theme-boot.js`：无显式偏好时工作流路由浅色、其他运营路由深色；用户明确选择仍通过 `localStorage` 全局持久化；
- 画布专项静态/浏览器测试、正式 `internal/api/web/` dist 与本记录。

已锁定的 DSL/metadata 兼容契约未变，因此无需新增 D- 确认。彩型是既有 `D-2026-10-15-console-references` 对画布分面的视觉落地，不表达执行状态。

## 实现结果

- 画布背景切为 n8n 风格点阵；工具条与五类节点使用独立的深/浅主题底色、前景色、描边色，卡片正文、占位、ID、摘要文字均使用卡片前景保证可读性。
- 选中、悬停、未配置、分支折角、四向 handle、连线、删除确认、DSL 预览和节点实际 DSL 数据流保持原行为。
- React 入口与阻塞式主题启动脚本共享路由缺省，避免深链首绘先落到运营面深色；彩型规则不污染执行详情定义图。
- 源码变更后已重建入库 dist；构建产物与源码验证目录逐文件一致。

## 验证证据

| 层级 | 命令/旅程 | 结果 |
|---|---|---|
| 前端单测 | `PATH=/tmp/braid-m23-wave3-20261016/bin:/tmp/braid-m23-next:$PATH npm --prefix web/canvas test` | 32 files / 414 tests 通过 |
| 前端构建 | `npm --prefix web/canvas run build` | TypeScript 与 Vite build 通过；仅保留既有 chunk size warning |
| dist 漂移 | `npm --prefix web/canvas run build:verify` + `diff -r --no-dereference internal/api/web web/canvas/.dist-verify` | 无差异 |
| Go 契约 | `GOCACHE=/tmp/braid-ux-f1-b-gocache go test ./internal/api -run 'TestCanvasExportFixtures\|TestWeb' -count=1` | 通过 |
| Go 全量 | `GOCACHE=/tmp/braid-ux-f1-b-gocache go test ./...` | 通过（受控 loopback 环境） |
| Go 静态 | `GOCACHE=/tmp/braid-ux-f1-b-gocache go vet ./...` | 通过 |
| Lint | `golangci-lint run` | 0 issues |
| 浏览器真实栈 | 外层 `flock -n -o /tmp/braid-m23-validation.lock`；`deploy/e2e-up.sh`；`BRAID_E2E_BASE_URL=http://127.0.0.1:18081 npm --prefix web/canvas run test:e2e`；`deploy/e2e-down.sh` | 31/31 通过；包含新视觉旅程、保存/重开/连线/删除、双主题、1024/1440 窄/宽窗口；栈已收干净 |
| 独立审查 | 独立代理只读审查实际 diff、契约边界、dist 与测试；首轮 HIGH 修复后复核 | CRITICAL/HIGH = 0；修复入口浅色首绘与 DefinitionGraph 彩型作用域 |

## 失败与边界

- 未提升沙箱中的首次 `go test ./...` 因 loopback 临时监听被拒绝；同一命令在允许 loopback 的受控环境重跑通过，失败输出保留在会话证据中。
- 首次全量浏览器尝试为 30/31：新视觉断言采到了节点 120ms 色彩过渡的中间帧；等待实际对比度稳定后，针对性与最终全量均为 31/31。该失败没有改变产品数据或栈状态。
- e2e 使用仓库钉定的工程桩与独立 MySQL/Redis 栈，不调用真实模型、不改 dev/prod 数据；本票没有付费调用授权需求。
- 对比度门按正文/占位 ≥4.5:1、图形 ≥3:1执行；真实浏览器断言覆盖五类节点两主题及其卡片文字。
