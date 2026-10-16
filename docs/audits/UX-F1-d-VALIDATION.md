# UX-F1-d 执行列表视觉整备验证

任务：#246；[PR #261](https://github.com/HeShen-1/braid/pull/261)。定义真源：ROADMAP M22-2；方向：D-2026-10-15-console-references。初始基线 `f57cdcf4`，A 合入后的集成基线为 `371a10ac625a8bd4c8d6cb5b0f67a94dc5d0cd8a`。

## 范围与产品候选

- 真实执行 ID、工作流/版本、状态、时长、创建时间组成密表；七个状态页签（全部及六个真实状态）只驱动既有 status 参数，不推算各状态计数。名称继续为完整名称精确匹配，换条件重置分页。
- 页签方向键/Home/End、行 Enter/Space、URL 选中与可见勾形保留。未选择时扩大表区、触发面常显侧栏；选中后保留列表与详情，1024 窄窗上下排列并在区域内滚动。
- 沿用 T23.8/T23.10 的诚实加载/错误出口、角色能力、钉版触发输入与焦点行为；不重复实现前置修复。新增样式限于执行局部作用域，消费 A 已合入的语义 token；没有后端、状态枚举、engine、迁移、CI 或依赖变动。
- 最后产品源、回归与完整 dist 在 `4e70aaa93754c5ffa8b2801d3b79d5d856cd0162`。之后仅本文及 ROADMAP 收口，按 661 个非 Markdown 文件逐项核对产品对象等价；不宣称文档提交重跑了 Go/335 项单测。

## 本机验收

证据根：`/tmp/braid-m23-wave6-20261016/ux-f1-d/`，以及实现树 `.handoff/evidence/UX-F1-d/`；最终归档/校验清理记录由主会话持有。

| 门 | 实际 revision 与结果 | 证明边界 |
|---|---|---|
| 独立 npm ci | 初轮退出 0；Node v26.10.0、独立 node_modules | 不与 A 共用依赖目录 |
| vitest / shadcn 清单 | 4e70：27 文件、335/335，原生 0 | jsdom/fetch doubles，不冒充真实后端/浏览器 |
| tsc/build、build:verify、正式 dist 门 | 4e70：均原生 0，13 文件一致 | 从源码完整构建，不拼散列资源；已有大 chunk 提示保留 |
| 真栈 go test ./... | 4e70：2954 test/subtest pass、唯一 TestMultiReplicaDrill 显式 opt-in skip；test/gates 原生 0 | 主会话持锁、独立栈，原生 JSONL 与 cleanup 在 go-4e70.../ |
| go vet ./... / golangci-lint run | 4e70：均原生 0；vet 无输出、lint 为 0 issues | PRIMARY-GATE-READBACK 与原件 SHA 已核读复用 |
| 正式嵌入 dist 官方 Playwright | 4e70：22/22、2.6m；playwright/gates 原生 0 | 真账号/API/worker、工程 stub；零真实供应商模型调用；不把先前发现 22 例写成运行 |
| 真实正常动效 UI | 4e70：20 份实际测量，双主题 × 1440/1024 的 tab/选中矩阵达标 | 原始 DOM/computed、PNG、关闭/空 session 列表与浏览器 errors 空记录齐备；PRIMARY-UI-READBACK |
| 静态设计检测 | 4e70：两个产品改动目标仅跑一次，原生 0 / `[]` | 机械检测，不替代真实 UI/独审 |
| 独立六视角与收尾报告 | 4e70：开放 C/H/M/L=0/0/0/0；D-R2 真实 SPA/正常动效/像素闭环 | 已核读 review/INDEPENDENT-UI-REVIEW-4e70aaa.md；D-R1 仅按源/静态缺值诊断关闭，不虚构该状态实测 |

官方本项实际保存 12 张 PNG：4 张未选列表、4 张 tab 焦点裁图、4 张已选列表/详情，均为 1440/1024 × 1000。正文 ≥4.5、图形/焦点 ≥3 的阈值没有降低；新增 tab 回归还要求 solid 2px/-2px，并核对实际 PNG 顶部两行像素，避免仅 computed 或底部选中下划线通过。主题切换等待实际有限动画完成。

独立真实 UI 保持正常动效（no-preference=true、reduce=false），实际窗口同为 1440/1024 × 1000。全部 20 份测量的正文最紧为 4.895116:1；焦点与选中图形如下：

| 实际矩阵 | 深色 | 浅色 | 门 |
|---|---|---|---|
| 状态 tab 焦点环 | 5.251843:1 | 4.147860:1 | ≥3；solid 2px/-2px，四边实绘 |
| 已选行勾形 / 行焦点 | 5.248933:1 | 4.431368:1 | ≥3；真实 URL/aria-selected 对应 |

同栈只读核对实际 All=40，第二页 21–40；WAITING_APPROVAL=30 且状态切换回第一页；alice 夹具工作流完整名称精确命中 20 条、少一字符为 0，bob 完整名称为 1。真实键盘可到第 20 行并选择，窄窗列表/详情可读。独审实际为 19 个原生 CLI 进程/120 个浏览器动作全部成功，20 份测量/14 张 PNG；上下各两行焦点实际像素均核读。以上为该隔离栈当时的数据，不是产品统计承诺。隔离浏览器关闭、稳定 session list=[]、errors=[] 已核读；官方同栈整体 cleanup 与 gates 原生 0。

通过轮 reporter=list 没有将末尾 info.attach 的 body matrix JSON 独立落盘，不声称官方各组合 raw JSON 已归档。精确数值引用另存的正常动效真实 UI 原始 JSON；12 张官方 PNG 实际存在并已逐图/联系表核验。未做自然人、屏幕阅读器或物理设备验收。

## 诊断与开发历史

| 记录 | 当时证据 | 处理与边界 |
|---|---|---|
| 初始 focused 单测 | 27 pass / 1 fail，原生 1；新增断言误用 jsdom 的 gridcell 角色 | 改核对真实 ID 文本；不是产品行为失败 |
| 初始 tsc | 原生 1；把 Playwright exact 选项用于 Testing Library | 移除不支持选项；f877 本地 329 项及构建门通过，A 合入后为 335 项 |
| 初始 f877 push 客户端 | 有界轮询未返回，远端 API/PR 已精确核实 f877 后结束本次独立进程组，原生 143；组复核无残留 | 不称 push 原生 0，不盲重发；后续精确 lease 推送原生 0 |
| 19 个 dist 冲突 | 精确 rebase 371a10ac 原生 1，冲突仅生成物 | 四个任务源码与 A 共享文件逐项保留；从合并源码整构后继续 rebase 原生 0，形成 7876907；f877 备份 ref 保留 |
| D-R1 缺值文字反差 | 787 静态 token 反例：ink-3 / accent-soft 为 4.363381 < 4.5；不是失败用例退出 1 | 4e70 只在选中行把 ink-3 提升为 ink-2，静态候选 5.152178。真实 API 样本没有缺值，不宣称 SPA 已覆盖缺值状态 |
| D-R2 tab 环裁切 | 精确 787 编译 CSS 的最小 DOM 观察脚本原生 0，图/几何证明上下环被滚动容器裁切；不等于 SPA 失败用例 | 4e70 只设局部 focus-visible offset=-2px；真实官方顶部像素门和正常动效独审另证实际环 |
| 新增 PNG 读取的 tsc | 原生 1：钉版 Playwright 随包导出没有声明文件 | 在 Node createRequire 边界加窄接口类型，零忽略、零新依赖；后续 335/构建门全过 |

初始 local-freeze/、rebased-371a10ac/、repair-r1-r2/ 的命令原件/原生退出、source manifest、原始 f877 与 787、R1 静态反例、R2 最小 DOM 图及修复补审均保留。修复后采样明确包含表格实际 .num/.text-ink-3，避免漏掉时长/时间缺值文字；静态候选不冒充真实缺值验收。

## 准出边界

最终纯 Markdown 提交须核 661 个非 Markdown 文件与 4e70 产品对象一致，并复审文档；最终 PR head 的 GitHub 七门全部 COMPLETED/SUCCESS 且 gh pr checks 原生 0 后，由主会话执行合并、删除并核验远程分支、独有证据归档校验及本地 worktree 清理。本文记录已完成的本机验收，不提前宣称这些准出动作完成。

本项零付费模型调用，未操作既有 dev/prod 数据。正式 Compose 与组件账号、真实模型完整业务旅程仍归 T23.13，不计本项通过。
