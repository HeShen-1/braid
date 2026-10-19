# UX-F2 / #228 画布插入与端点保存验证

本记录只覆盖 [#228](https://github.com/HeShen-1/braid/issues/228)。实现分支为 `feat/ux-f2`，独立工作树为 `.worktrees/ux-f2`；基线为 `a328b8180c9457d0a29bd3284eb82d7fe0d99e2f`，候选产品提交为 `ba556ec66552f5cca28dbdbcd488fd612705bb6c`。#244、#227、#242 没有纳入本票。

## 写入面与锁定契约

开工前已核对 `.handoff/WRITE-SURFACE.md`、现有转换器、schema、API 契约测试及前端真实写面。普通边原先只保存 `from/to/source_anchor`，物理 sourceHandle/targetHandle 在导出再导入时丢失；`source_anchor` 仍只表达 branch 的 `case_N/default` 语义，不能复用为物理方向。

用户已于 2026-10-19 明确批准 [D-2026-10-16-ux-f2-edge-sides](../DECISIONS.md#d-2026-10-16-ux-f2-edge-sides)。按该 D-，边元数据增加可选 `source_side` / `target_side`，枚举 `top/right/bottom/left`；只记录非默认物理方向，普通边缺省仍为 source bottom、target top。branch 源继续使用既有 case/default/兜底语义和下沿出口，目标可四向，方向不改变执行路由。旧文档可由新版本读取，旧 schema 会拒绝新方向字段；四入口需使用兼容 schema，回滚保留扩展 schema。本票没有迁移生产数据、已发布快照或旧 schema 的降级转换。

schema 位于 `internal/engine/`，所以即使验证和 CI 全部通过，最终 GitHub 合并仍由用户人工完成；本会话不自动合并。

## 产品结果

- 新节点按真实画布与节点测量尺寸寻找空位，连续添加不堆叠，不重排旧节点；新节点自动选中并联动属性面板。长 branch/far-node 场景只将新增节点带入视口，不按全图缩放旧模型。
- 普通节点 source/target 均展示四向 handle；保存、服务端读回、关闭重开后保留非默认方向。默认 target top 与旧无记录、显式未绑定、陈旧语义锚、branch case/default、删除清理保持原语义。
- 拥挤布局下保持面板折叠偏好和添加按钮焦点；窄窗口的编辑器及两层 flex wrapper 不再把页面横向撑出 viewport。

## 原生验证

所有结果均保留原始日志、退出码和 SHA；统一外部证据根为 `/tmp/braid-m23-wave8-20261019/`，任务树 `.handoff/evidence/` 仍保留实现过程和失败原件。

| 验证层 | 精确结果 | 证据 |
| --- | --- | --- |
| 前端单测 | 最终 `ba556ec`：31 个文件、407/407 通过 | `/tmp/braid-m23-wave8-20261019/final-head-frontend-ba556ec/frontend.log`、`receipt.json` |
| TypeScript/build | 最终 head 的 `build`、`build:verify`、dist drift gate 均退出 0；漂移门报告 13 files clean | 同目录 `build.log`、`build-verify.log`、`dist-drift.log`、`receipt.json`；正式门 `/tmp/braid-m23-wave8-20261019/formal-dist-ba556ec/native.json` |
| Go 真栈三件套 | `BRAID_TEST_REQUIRE_STACK=1`；`go test` 退出 0，2993 个通过、唯一 skip 为显式 opt-in `TestMultiReplicaDrill`；`go vet`、`golangci-lint` 均退出 0，lint 输出 `0 issues.` | `/tmp/braid-m23-wave8-20261019/validation-7-ba556ec66552f5cca28dbdbcd488fd612705bb6c/{go-test.jsonl,go-test.exit,go-vet.exit,go-lint.exit,go-lint.log}` |
| 官方浏览器 | 真栈官方 Playwright 30/30 通过，退出 0 | `/tmp/braid-m23-wave8-20261019/validation-7-ba556ec66552f5cca28dbdbcd488fd612705bb6c/{playwright.log,playwright.exit}` |
| 独立真实 UI | 最终 head 的窄窗长 branch/far-node 硬门通过：1024×900 下 `scrollWidth=1024`，editor/editor-mid/editor-side 右界均为 1024；新增节点选中、属性联动、视口定位、旧坐标保持通过；浏览器和临时 auth 已关闭 | `/tmp/braid-m23-wave8-20261019/independent-ui-ba556ec7/FINAL-UI-REPORT.md`、`visual-complete.json` |
| 生命周期 | 外层 `flock -n -o /tmp/braid-m23-validation.lock` 生命周期退出 0；主仓恢复为 clean `master`，测试端口、锁和独有进程均清理 | `/tmp/braid-m23-wave8-20261019/validation-7-ba556ec66552f5cca28dbdbcd488fd612705bb6c/{lifecycle.exit,restore-master.log,process-cleanup.log}`、`attempt7-primary-readback.json` |

独立 UI 报告明确限定覆盖：最终 head 只复跑修复后的窄窗硬门；普通四向保存/GET/重开、target-top、删除取消/焦点和深浅主题对比度沿用前一精确产品头的不可变原始证据，未把这些旧证据冒充为本 head 的重新操作。官方 30/30 仍覆盖本票自动旅程。

## 失败与修正原件

以下失败原件保留，不改写为通过：

1. 首轮 Go 断言把 null/number/object 类型错误地期待为 enum；仅修正测试预期。
2. attempt1 的 schema suffix 误设及执行中编辑脚本导致 `test_result: unbound variable`；原生 Go 退出码未落盘，不能补称通过。
3. attempt2 官方浏览器 26/29：target selector、可见 stroke 和精确 viewport ratio 假设错误；修正为真实 handle、可见线段及 CSS 像素容差。
4. attempt3 官方 28/29：低缩放 branch 拖线触发真实 auto-pan，测试仍使用平移前坐标；保留诊断和只改测试前置平移的补丁。
5. attempt4 独立 UI 诊断确认长 branch 下新节点因全图 `fitView` 完全离屏；该阻塞问题由新增节点局部 fitView 修复。原诊断、PNG、API/DOM 和组件日志完整保留。
6. attempt5/6 修复 F1 后分别发现长 branch 窄窗横溢；最终在 WorkflowsRoute 的实际第三层 wrapper 补 `min-w-0`，再以最终 head 复跑窄窗硬门通过。

入口：`/tmp/braid-m23-wave8-20261019/attempt{1,2,3,4,5,6,7}-*`、`independent-ui-{414449e,741f372,9b6f4e7,ba556ec7}/` 及任务树 `.handoff/evidence/`。失败日志、旧报告和成功回执均为只追加证据。

## 交付边界与后续闭环

本票使用回环免费 stub、独立测试数据和本地真栈；不代表真实供应商调用、生产数据库、正式 Compose 配置独立环境、自然人用户验收或 T23.13。PR、AI 独审、精确最终 head/base 的 GitHub 七门、人工 merge、远程分支核删、独有证据逐 SHA 归档和 no-force 工作树清理属于交付闭环；完成后只收口 #228 并停止。
