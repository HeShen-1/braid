# UX-F3 节点删除入口验证

任务定义：[ROADMAP M22-2](../ROADMAP.md) / [issue #229](https://github.com/HeShen-1/braid/issues/229)；[PR #258](https://github.com/HeShen-1/braid/pull/258)。独立 `feat/ux-f3` worktree，Redis 槽位 1，基线 `1b6480a833b58dbc4014072567583d9847a47b75`。本机工程门与浏览器实际测试的产品候选为 `229beb8d8965ff85a75c3bb7615486a0206ef775`。

## 范围与回归

当前删除按钮原本已有 `aria-label="删除节点"` 与包含节点 ID 的 `title`；本票不重复补这些属性。唯一产品源码改动是在 `NodePanel.tsx` 去掉 `iconOnly`，为原按钮加入可见“删除节点”文字。保留既有垃圾桶图标、危险按钮样式、焦点 ref、testid 和 `onDelete` 回调，不改 `CanvasEditor`、公共 CSS 或删除实现。

面板入口继续交给既有单一确认漏斗，与 Delete/Backspace 同路。确认前不删除；“放弃”或 Esc 保留节点并回焦入口，Enter 可从入口打开确认层。新增断言覆盖可见文字与原有可及属性；已有画布单测继续覆盖确认、取消、焦点回归与一次性放行。

浏览器专项覆盖两条：文字入口及鼠标/Enter/Delete/Backspace 的确认与取消；通过真实 API 建立已连接分支草稿，先核对取消后完整 DSL 不变，再确认删除，逐项核对节点、入出边、画布节点位置元数据、出口锚点记录和 DOM 手柄无残留，其他节点保持原值。定向自动化选择器证明交互回归，发现性另由下述独立演练证明。

## 本机工程门

主会话针对上述精确产品候选执行完整验证，作者已只读复核原生文件与退出码。统一证据根目录为 `/tmp/braid-m23-wave5-20261016/ux-f3/`，各阶段子目录名称均带完整产品 SHA。

| 门禁 | 实际结果 | 原始材料 |
| --- | --- | --- |
| 前端全量 | 24 文件、290 项 vitest 通过；Node 26.10.0 | `frontend-229beb8d8965ff85a75c3bb7615486a0206ef775/test.log`、`test.exit=0` |
| 构建与正式 dist | `build:verify` 通过；正式漂移判据 clean（14 files）；whitespace 通过 | 同目录 `build-verify.log`、`drift.log`、三项 `.exit` 与 `gates.exit` 均 0 |
| 真栈三件套 | 独立 Compose/MySQL/Redis、worktree schema 与槽位 1；`BRAID_TEST_REQUIRE_STACK=1` 的 Go 测试 JSONL 为 2954 pass、0 fail，仅显式 opt-in `TestMultiReplicaDrill` skip；vet 无输出，lint 为 `0 issues.` | `go-229beb8d8965ff85a75c3bb7615486a0206ef775/test.jsonl`、`test.stderr`、`vet.log`、`lint.log`；test/vet/lint/gates 的原生 `.exit` 均 0 |
| 官方完整浏览器旅程 | 主仓临时检出同一产品候选，官方 `deploy/e2e-up.sh` 运行入库 dist；16/16 通过（2.0 分钟），含上述两条删除专项和既有回归 | `e2e-229beb8d8965ff85a75c3bb7615486a0206ef775/checkout.log`、`playwright.log`、`playwright.exit=0`、`gates.exit=0` |

真栈测试容器已收回；浏览器阶段结束后任务 api/relay/worker/reaper 进程组与 e2e 容器已收回，主仓恢复干净 master `1b6480a`。活应用仅在主仓运行，共享栈生命周期由主会话持公共锁串行。

## 独立代理发现性与保存核验

本轮是独立新代理 `n=1` 的无实现提示演练，没有自然人试用。该代理仅获冻结临时页面 URL、隔离 dummy 账号与“删除 target_step、保留 keep_step 并保存”的普通任务；未读取源码、PR、issue、交接、DOM、可及树或 testid。入口判断来自真实截图，操作使用坐标和键盘。

登录后画布截图到首次看见删除入口为 10.838 秒，需要 1 次选择目标节点。画布到已保存截图为 65.4 秒、5 次任务点击，含 1 次把确认点击落到 DSL 预览的误触；代理按新截图修正后完成删除和保存。首次打开 URL 到已保存为 79.616 秒。这些时间包含工具返回、读图和代理判断，不作为真人可用性指标，也不隐去失败操作。

主会话亲看 `04-after-login.png`、`05-target-selected.png`、`06-after-delete-click.png`、`09-after-save.png`；作者另看 05/09。1440×1000 深色桌面画面中，选中目标后属性面板顶部直接显示“删除节点”，确认栏点名目标；保存后只剩 keep_step 且显示“已保存”。本项没有将这些截图推广为两主题、窄窗或自然人研究结论。

代理观察到的 UI 结果与服务端证据分列：主会话独立 GET 已保存草稿，`persisted-verification.json` 记载相同产品 SHA、`persisted_node_ids=["keep_step"]`，并逐项通过 `target_deleted`、`keep_retained`、`no_dsl_edges`、`target_canvas_metadata_absent`、`no_canvas_edge_anchors`。本次发现演练没有触发执行或调用 LLM。

原始目录 `/tmp/braid-m23-wave5-20261016/discovery/` 保存 `DISCOVERY-REPORT.md`、`metrics.json`、逐步理由与命令时间戳、47 条全部 exit 0 的浏览器原生命令、编号截图、独立 GET 回执，以及 `session-close-verification.json`：browserLaunched=false、pageCount=0，原 daemon PID 已不存在。独立 GET 不归入代理的无提示发现依据。

## 失败轮与准出边界

任务树 `.handoff/evidence/ux-f3/` 保留初次依赖安装的 DNS EAI_AGAIN 日志 `npm-ci-network-stalled.log`、依赖未齐导致 vitest 未启动的退出 127 日志 `nodepanel-red.log`，以及顺序重装成功的 `npm-ci-repair.log`。它们是环境准备结果，不算测试通过。

新增可见文字断言在旧产品上的真正红例见 `nodepanel-red-retry.log`：1 fail、18 skip，原因是按钮文本为空。改动后 `focused-vitest.log` 的 NodePanel/CanvasEditor 39/39 通过，`build.log` 为完整构建成功；`playwright-list.log` 仅为收集两条测试，正式执行结果以主会话上述 16/16 原生记录为准。机械检测 `impeccable-detect.json=[]` 也不替代浏览器验收。

发现演练的种子预备曾因工作流名称含下划线被服务端 `invalid_dsl` 拒绝；改为合法连字符名称后重建，没有执行工作流。工具只读目录/socket 的 EPERM 失败保留在 `preflight-native.jsonl`，本阶段没有把工具预备失败归为产品通过或删除失败。

本次后续提交只改本验证文档与 ROADMAP 的 UX-F3 行；最终产品树与已测候选的一致性由主会话核验。独立六视角审查读取实际 PR diff，并在最终文档头冻结后补审回贴 PR；最终头原生 `gh pr checks` 退出 0 且七项具名 CI 全部 COMPLETED/SUCCESS，才允许合并。这些最终准出门在本文写入时仍待主会话核对，不预先写为已通过。

独有交接、配置、失败日志和发现演练证据由主会话私有归档并逐文件核验；只有任务完成、远程分支证空和归档核验完成后才清理本地 worktree，保留本地分支。真实模型与正式配置完整业务旅程继续归 T23.13，本票不宣称其完成。
