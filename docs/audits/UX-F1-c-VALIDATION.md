# UX-F1-c 执行总览视觉整备验证

> 本记录对应产品冻结 f8ebe1c：工程门、官方 23 项、独立本票六轴/真实 UI 审查及官方完整生命周期均已通过。最终纯文档头审查、GitHub 七门、合并与证据归档清树仍由主会话后续核对，不提前计为完成。

任务：#245；[PR #262](https://github.com/HeShen-1/braid/pull/262)。定义真源：ROADMAP M22-2；方向：D-2026-10-15-console-references。基线 `0ad6bd16695533bbdc3db2e4f20f057fa001dc8b`；验收产品头 `f8ebe1c46f17e16c6a6d282bd7e89714704dfd4c`。

## 范围与真实数据口径

- 用户已选择「实时状态卡片＋最近 100 条执行的创建时间分布」。四卡是当前租户、全部工作流的当前保留执行总数，以及 `RUNNING` / `WAITING_APPROVAL` / `FAILED` 的计数，均取既有列表 API 的 `page.total`；不是当前账号待办数，也不受下方列表名称、状态或分页筛选影响。
- 一次刷新共四个 GET：全量 `limit=100, offset=0` 同时提供总数与创建样本，三个状态计数各用 `limit=1, offset=0`。四次分别读取，不承诺原子同刻。每 5 秒、操作事件与手动刷新共用在途去重；逐项失败保留最近成功值/样本，并标明失败与旧读取时间。初载未知不显示 0，缺 `page.total` 不回退到 `items.length`。
- 创建分布仅聚合当前响应中真实 `created_at`；明示样本数、有效/缺失时间、UTC 样本起止、等宽桶宽和「条/桶」，完整桶表可键盘展开。桶按 UTC epoch 对齐、含起点不含终点；空样本、无有效时间或不足两个不同时间点均说明原因而不造图。它不代表固定 24 小时时间窗，不把轮询快照拼成状态历史，也不添加成功率或成本数字。
- 未选择执行时在列表上方展示总览；选择后沿用已有密表与节点/审计详情，1024 窄窗不压缩原有列表宽度。Geist、现有主题/状态令牌与锐利分隔继续复用。没有后端端点、依赖、状态枚举、engine、迁移、CI、AGENTS 或画布变动；不重复实现 T23.9、T23.10、UX-F1-a/d。
- 源码、新回归与完整生成 dist 同提交冻结于 `f8ebe1c46f17e16c6a6d282bd7e89714704dfd4c`。本次只以本文及 ROADMAP 本项 checkbox 收口；逐项核对 667 个非 Markdown 对象与该产品头相同，工程证据因此沿用该准确产品头，不能声称纯文档头重跑了工程门。

## 本机验收

证据根：`/tmp/braid-m23-wave7-20261016/ux-f1-c/`，以及实现树 `.handoff/evidence/UX-F1-c/`。`product-freeze-v4-focus.json` 保存 20 个源码/生成物 SHA256、完整 13 个 dist SHA256、667 个非 MD Git 对象与 059–078 的 20 份命令原件 SHA；v1/v2/v3 冻结及三轮历史官方生命周期均保留。最终独有证据归档校验及清理由主会话持有。

| 门 | 实际 revision 与结果 | 证明边界 |
|---|---|---|
| 独立 npm ci | 初轮原生 0；Node v26.10.0、独立 node_modules；003 原件 | 不与其他工作树共用依赖目录 |
| vitest / shadcn 清单 | f8ebe1c：29 文件、347/347，068 原生 0 | 保留原有 335 项，新增 12 项总数、分桶、空/错误态、轮询、迟到响应及焦点/重复触发回归；jsdom/fetch doubles 不是后端验收 |
| tsc/build、build:verify、正式 dist 门 | f8ebe1c：071/072/074 均原生 0，13 文件一致；073 diff check 原生 0 | 完整重建与正式漂移判据，不拼散列资源；既有大 chunk 提示保留 |
| 官方旅程发现 | f8ebe1c：069 发现 23 项、原生 0 | 发现与实际运行分开记录 |
| 真栈 go test ./... | f8ebe1c：fresh `go test -count=1 -json ./...`，2954 test/subtest 通过；唯一 `TestMultiReplicaDrill` 显式 opt-in skip；test/gates 均原生 0 | 主会话持锁，`BRAID_TEST_REQUIRE_STACK=1`、槽位 1 与独立 MySQL/Redis；原件在 `go-f8ebe1c.../`，本代理重新计数并核 SHA |
| go vet ./... / golangci-lint run | f8ebe1c：均原生 0；vet 无输出、lint 为 0 issues | 与产品头一致的 fresh 真栈轮，不复用 a9f Go revision；Go 生命周期 cleanup 原生 0 |
| 正式嵌入 dist 官方 Playwright | f8ebe1c：23/23，`playwright.exit=0` | 真账号/API/worker，工程 stub，零真实供应商模型调用；官方完整生命周期 7418 已退出原生 0；独立 UI 结论另列 |
| 官方总览四矩阵读回 | f8ebe1c：1440/1024 × 深/浅色；原始 DOM/JSON/PNG、柱/焦点实绘、6 桶合计 100 条与 12 个真实刷新焦点阶段读回通过 | 主会话 `PRIMARY-OFFICIAL-BROWSER-READBACK.json` 与原件 SHA；不替代独立 realUI |
| 独立六轴与正常动效 realUI | f8ebe1c：H1–H5 closed，当前 C/H/M/L=0/0/0/0，本票批准范围 ship | 最终报告 `review/073-final-product-review-f8ebe1c.md`；源码/生成物六轴、四矩阵实绘与 Enter/Space/自动/在途移焦去重均有独立原件，未变面的精确复用另述 |
| 官方完整生命周期清理 | f8ebe1c：7418 原生 0，`gates.exit=0`；主仓恢复 clean master `0ad6bd1` | 18 个原容器及 3 个原 PID 保留，own 容器/CWD 进程为空、六测试端口与生命周期锁释放；独立 browser/session/daemon 正常关闭，详见后文原件 |

本代理读回的 `draft-final-audit-v4/evidence-readback-v4.json` 重新计数 f8ebe1c 的 Go 原始 JSONL，并逐 SHA 核验 10 份 Go 原件与 106 份官方浏览器原件；四组图形、桶和焦点结论明确引用主会话正式 PNG/JSON 读回；它是文档事实核读，不是一次新业务或独立 realUI 验收。

## 官方真实样本与实绘门

官方总览用例的早期截面：当前租户四卡 `all/RUNNING/WAITING_APPROVAL/FAILED = 101/0/96/0`，最近样本 100 条；分别与对应服务端 `page.total` 对拍。名称筛选后总览保持租户范围，选中后 URL、密表与详情沿原语义运行。样本用合法隔离名单 `uxf1c-isolated` 维持真实 `WAITING_APPROVAL`，不向 Alice/Bob 的共享待办队列灌入大样本。

官方原始 API 样本的 UTC 创建范围为 `2026-10-16T09:25:56.590Z..2026-10-16T09:26:40.510Z`。主会话与独审分别按 `created_at` 重算 6 个 10 秒等宽半开桶 `[1,1,1,2,2,93]`，合计 100 条；样本起止、桶边界与单位一致。这些是该隔离栈当时的数据，不是产品固定窗口或容量承诺。

独立真实 UI 在完整 23 项之后另取一个截面：四卡为 `112/0/102/3`，最新样本仍为 100 条，UTC 范围 `2026-10-16T09:26:40.272Z..2026-10-16T09:29:00.652Z`；独立原 API 重算 4 个 60 秒半开桶 `[89,6,4,1]`，四矩阵各合计 100。后续官方旅程已创建新的真实执行，因此此截面的总数、范围和桶数与官方早期截面不同；两者分别留证，不混用计数，也不拼成历史。

本项新增官方证据直接 `writeFile(info.outputPath(...))`：4 张总览图、4 张焦点裁图、4 张图表裁图，以及每组测量、柱像素、轴间隙纵剖面、3 个刷新阶段 JSON 和完整 API 样本证据。12 个焦点阶段含手动 Enter、实际 5 秒自动轮询，以及用户主动移到名称输入框后的下一次自动轮询；每阶段记录到 `false→true→false`、HTTP 200 和全程 active 保持，按钮没有 native disabled。不依赖 list reporter 将成功轮的 `info.attach({body})` 自动落盘。

| 官方实际矩阵，均高 1000 | 正文最低 | 纯轴间隙最强实体像素 | 阈值与其他实绘门 |
|---|---|---|---|
| dark 1440 / 1024 | 5.077930:1 | `[114,118,125]`，4.368063:1 | 正文 ≥4.5；图形/焦点 ≥3 |
| light 1440 / 1024 | 5.469045:1 | `[128,141,149]`，3.229282:1 | 实体轴 RGB 距期望 ≤3，AA 边缘单独保留 |

焦点必须为真实键盘 `focus-visible` 的 solid 2px 内环，PNG 顶部两行逐通道核对；柱形必须在峰值实体内点命中预期色，RGB 误差 ≤3。SVG 几何经实际 `getScreenCTM()` 与屏幕边框映射到 PNG；量化后的采点须仍位于实体内部。轴另在相邻柱之间的纯间隙扫描纵剖面，以同列实际背景计算对比，保留背景/AA 边缘/最强实体区别，防止用柱色冒充轴线通过。主题切换等待实际有限动画自然结束，没有降低任何原门限。

## 五项发现、三轮历史官方生命周期与修复边界

| 发现 | 当时原件与结果 | 修复及边界 |
|---|---|---|
| 首个非零柱采样混入 AA | 6d91 官方首轮在新总览 case 失败：1/93 柱约 0.8 实像素，旧点 `[84,86,91]` 与期望差 94；同图峰值实体点精确命中 `[178,181,188]` | 34f960 采峰值真实内点，用 CTM/屏幕边框定位，保留 RGB ≤3；没有改业务柱高、分布比例或放宽门限。原始 trace/PNG 与 `review/030-round1-png-sampling-analysis.json` 保留 |
| 大样本污染旧审批旅程 | 6d91 首轮新增 Alice 大量等待审批，原始 API 200、待办总数 103/104，20 行首屏容纳了本项 cohort，旧两个新待办不在页内 | 34f960 改本项专用合法隔离 username；DSL 明确不将账号存在作为发布期判据，待办按快照名单精确过滤。未改 Alice/Bob、种子、分页业务或旧 e2e；第二轮旧两旅程均恢复通过 |
| SVGRect 序列化兼容性 | 34f960 第二轮新增 case 原生失败：真实浏览器 `bar.getBBox(...).toJSON is not a function`；TypeScript 检查未覆盖这个运行时差异 | a9f46b4 显式读取 `{x,y,width,height}`，不对 SVGRect 调 `toJSON()`；真实 DOMRect 测量保留。先以隔离 Chromium 执行实际新测量原码验证，再进入正式全套 |
| 轴线无足够实绘实体 | 独审从纯无柱间隙确认：1 SVG 单位在 100→80 高缩成 0.8px，最强 `[61,63,67]` / 背景 `[8,9,10]` 仅 1.889:1；早先落柱内的 x300 采点不构成轴证据 | a9f46b4 局部轴改 `stroke-width:2px; vector-effect:non-scaling-stroke`，令牌/比例/数据/CSP 不变；补纯轴间隙真实 PNG 回归，官方四组实体像素实测另证。保留 `review/033-round1-axis-painted-analysis.json` 与更正记录 |
| **H5 / M/P2：刷新破坏键盘焦点；closed** | a9f46b4 独立真实 UI：09:04:04.808 为 BUTTON/focus-visible/busy=false，Enter 后 09:04:05.003 为 BODY；09:04:05.055 重新聚焦后不操作，09:04:10.020 自动轮询再次使焦点变 BODY。原件为 `visual/a9f46b4.../independent-review/` native 步骤 024–037；该轮官方脚本虽 23/23 原生 0，realUI fail 使整轮生命周期原生 1 | f8ebe1c 产品仅一行 `disabled={refreshing}` 改为 `aria-disabled={refreshing}`，沿用既有 inFlight 去重、忙态文案和 solid 2px 焦点，不写主动恢复焦点，不改 CSS/共享 primitive/旧 refresh。060 修复前新两条单测均失败，061 修复后组件 9/9，068 全量 347/347；官方 12 个真实阶段通过。**独立四矩阵 Enter/Space/真实自动轮询/在途移焦去重及 2px 实绘复验通过，H5 closed。** |

三轮历史官方生命周期原件完整保留：

- `e2e-6d91a24f29fc6b3fd9a6d2157c12d154100896fc/`：20 通过、3 失败，Playwright/整轮原生 1；不是产品或旧旅程全通过。
- `e2e-34f96035e5d2528a67827c025cc0dbee85d7bdb2/`：22 通过、1 失败，Playwright/整轮原生 1；失败为 SVGRect 测量兼容性，旧两审批旅程恢复。
- `e2e-a9f46b4f162befe001bbff42167ba2eb3103bfd5/`：官方脚本 23 通过、0 失败、Playwright 原生 0；独立 realUI 发现 H5，整轮原生 1，正常 cleanup 已完成。修复前清单逐 SHA 核验 115 份官方原件和 509 份独审原件，不能写成该轮最终通过。
- `product-freeze.json`、`product-freeze-v2-e2e-repair.json`、`product-freeze-v3-axis.json` 与另存的 `product-freeze-v4-focus.json`，编辑前 SHA 清单、独审反例及原始日志/trace/PNG 均保留。原件数量因主会话之后另存 readback 有时间差，不重写旧清单来统一计数。

初始默认网络读取失败、Vitest 错误 cwd、旧单测全局 status 查询歧义和回环静态服务 sandbox EPERM 亦留原件，后续用正确项目入口、控件范围与原生权限修正；不把这些失败轮计为通过。

## Fixture 与独立验收边界

`synthetic-browser-round1-native/` 仅用于初始布局；`fixture-axis-smoke-v3-round1/` 用明确 `[1,1,1,2,2,93]` 稀疏夹具，在真实 Chromium 1440/1024 双主题正常动效下执行从当时 a9f e2e 源提取的实际测量代码。其轴实体深色 4.368、浅色 3.229、柱 RGB 误差 0、自有 browser/server 已关闭；它仍不是正式 API、组件或生产 CSP 验收。045 harness 原码、测量源码 SHA、PNG/JSON 和原生回执保留。

`fixture-focus-smoke-v4-round1/` 仅是明确合成数据、120ms route fulfill 延迟的静态 host harness；真实 Chromium 以正常动效和自然 5 秒间隔，在四组窗口/主题执行从新 e2e 源提取的焦点及图形测量原码。063 原生 0，12 阶段均保持 refresh/name-input、页面 errors 为空、柱/轴旧门仍通过；自有 browser/server 已在 finally 关闭。它不替代本次正式 API/worker、生产 CSP 或独立 UI 验收，失败与成功原件均按轮保留。

impeccable 机械检测在初始实现完成时仅跑一次，011 原生 0 / `[]`；后续修复依据独立真实实绘与运行时证据，没有再跑 detector。静态检测、合成夹具、构建和旅程发现均不替代正式运行或独立审查。未做自然人、屏幕阅读器、移动端、触摸、缩放或多显示器验收。缺 total、初载、无样本/缺失/重复时间、401 与旧代响应属于源码/单测专项证据；503 来自官方 e2e 的 route 故障注入，不冒称真实服务端 503 或独审重复了负向注入。

独立最终报告为 `review/073-final-product-review-f8ebe1c.md`，SHA256 `f0cbadb0e2f6654b4ba827e40b4a5069e686491ab556dc43b1c1fc7463d4206f`；`review/074-final-product-review-facts.json` 记录当前开放 C/H/M/L=0/0/0/0、H1–H5 closed。结论为本票批准产品范围 ship，不扩为 T23.13 或整个产品准出。根会话逐原件复核的 `e2e-f8ebe1c.../PRIMARY-INDEPENDENT-FOCUS-READBACK.json` SHA256 为 `ef57a73a17c0f0e3d12432e43b2e42cba849bf505d943a96de99e5b894a35dbb`。

独立浏览器原件在 `visual/f8ebe1c.../independent-focus/`，含 99 份 native 回执、4 张亲看 PNG、只读原 API 与独立像素分析。1440/1024 × 深/浅色、DPR 1、正常 motion；各矩阵开始时实际有 4/4/5/4 个有限动画，等待其自然完成后均为 0，未以关闭动画替代正常动效。正文最低深色 5.077930、浅色 5.469045；焦点实际深色 7.278591、浅色 5.323722，每矩阵 3 个横向采点均有连续 2 行实体色；纯轴深色 4.368063、浅色 3.229282，峰柱及轴 RGB 误差 ≤3，AA 剖面保留，无横向溢出。

独立每矩阵实际 Tab 到桶 summary、Shift+Tab 返回、Enter、Space 刷新和自然 5 秒自动读取。手动各增 1 次 all `limit=100` 请求，焦点保持 BUTTON/focus-visible；自动 busy 前后亦保持。另在自动请求在途时使用明确的 DOM 双 click QA 控制，再将焦点移到名称输入框，all 请求仅增 1 次，结束后保持 INPUT；不将该控制伪称原生键盘重复激活，重复 Enter/Space 由单测另证。a9→f8 除本组件忙态属性外源码/样式/算法/接入逐 blob 相同，故 a9 独审的名称/状态筛选独立、桶明细键盘操作、选中详情让位与导航/back 可精确复用；a9 失败的焦点证据仍作失败保留，f8 焦点均重新亲测。

独立 052 errors=[]、053 console=[]；054 closed=true，058 session active=false/pid/runtime=null，059 own processes=[]、原 daemon 自然退出，未发终止信号。CLI list 仅保留 inactive 命名记录，未删除全局 metadata 或干预用户浏览器。主会话随后释放正式生命周期 7418，`gates.exit=0`。资源原件 `../after-c-v4-official.json` SHA256 `1503dbbeb6419b3513d8863ff35ef3e5b08e0a05a1567270262ea160762d150b`，确认 18 个原容器保留、own 容器及树 CWD 进程为空、六测试端口与锁释放；另名原件 `../after-c-v4-official-original-processes.json` SHA256 `60a6c1a75b4f2d481eb4fa0099428719aa99d4179655459e57ff5d6c17013138` 确认 3 个原 PID exact/live。回执同名覆盖诊断保留于 `../primary-receipt-name-collision-resolution.json`，未重跑或改写产品门。

## 准出边界

f8ebe1c 的 347 项单测、完整 dist/TS 构建、fresh 真栈 Go、官方 23、独立六轴及本票真实 UI、完整生命周期清理均已有准确产品头原件。最终文档只新增本文并切换 ROADMAP 本项 checkbox；提交前后须核对全部 667 个非 MD Git 对象与产品冻结相同，因此沿用产品门，不把新文档头说成重跑工程门。

最终纯 MD 内容与准确 PR 身份复审、最终文档头 GitHub 七个 checks 全部 COMPLETED/SUCCESS 且 `gh pr checks` 原生 0、PR 合并、issue 闭环、远端分支删除及核验、独有证据归档校验、本地工作树删除，均由主会话在本文之后执行与核对；本文不提前声称完成。ROADMAP 勾选随本 PR 合并生效。

本项零付费/真实供应商模型调用，未操作既有 dev/prod 数据。正式配置、组件账号与真实模型完整业务旅程仍归 T23.13，不计本项通过。
