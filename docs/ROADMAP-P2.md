# braid 路线图(P2)

> 任务定义与依赖真源 = 本文件;执行状态真源 = GitHub issues(认领/进行中/闭环,`gh issue list`)。勾选仅作历史镜像;勾选提交随任务 PR、merge 即生效,PR 描述带 `Closes #N`(注:远端 Closes 关键字偶发不触发,合并后 `gh issue close` 人工补)。
> 规模:S = 半天内,M = 1-2 天,L = 3+ 天(全职当量)。
> 里程碑依赖:M15(主轴)→(M16 ∥ M17 穿插池)。任务级依赖单独标注,冲突时以任务级为准。
> 方向拍板:2026-09-30 用户选定 Track 1「规模化与多副本 HA」为 P2 主轴(4 track 候选与 127 条前瞻义务全录见 `braid-p2-survey.json`,仓外);T17 为穿插清账池、不占波次,T16 排 T15.5 中后段吃压测数字。
> P0 冻结记录 = docs/ROADMAP-P0.md;P1 冻结记录 = docs/ROADMAP.md + docs/P1-DELIVERY.md;P1 停车场三项继承唤醒触发器(ROADMAP.md 文末),不重复排期。

## M15 规模化与多副本 HA(P2 主轴)

- [x] T15.1 HA 目标形态 D- 立项 — L — 验收:多副本拓扑与单点逐一裁定的 D- 条目**经用户确认**(Redis 队列 HA 替代选型——含 Sentinel 丢写负样本论证、MySQL HA 取舍、雪花节点号分配机制、沙箱/egress 多副本口径、多副本演练口径);不重开 ARCHITECTURE 已锁决策,只裁多副本新面;本任务是 T15.2-T15.4、T15.6 的前置闸
- [x] T15.2 副本身份与雪花节点号自动分配 — M — 依赖 T15.1 — 验收:租约注册或 stable ordinal 自动分配,消除人工外部分配 `BRAID_*_SNOWFLAKE_NODE`;Helm values 默认副本 >1 可起(D-t144 已知边界:Deployment 无稳定 ordinal)——实现 = 迁移 0012 租约表 + 三层解析(env 数字 > 内建默认 > "auto")+ auto 池抢注/复占/接管 + 续期/lost 停发 + SIGTERM 释放 + chart 摘钉转 auto(口径 = D-2026-10-01-t151 决策 2;rebase 时 T15.3/T15.4 已在 master——worker >1 的雪花/egress/清扫三挡全解)
- [x] T15.3 沙箱清扫互杀红线消除 — M — 依赖 T15.1 — 验收:多 worker 同网互杀面消除(t101-sandbox-baseline #11:kill -9 后死实例身份可知、孤儿回收不依赖全局 label 误伤;明文「多副本/k8s 滚动部署前必须先解」)— 已交付:D-t151 决策 5 全量落地(owner label + owner-scoped 清扫 + 部署契约 hostname/停机宽限 ≥35s + runbook 按-owner 原语;租约活性复核选型 = 部署契约承载,followup 位)
- [x] T15.4 egress 端口多副本分配面 — M — 依赖 T15.1 — 验收:同网多 worker 不撞端口拒启(D-t102④ + chart README 登记);rootless/受限 daemon 适用域一并复核
- [x] T15.5 引擎核心压测基线与容量模型 — L — 验收:基准套件落地(吞吐/时延/大 DAG 回滚锁窗量化);MaxRollbackRows 实测定档(D-t92 停车场悬项);容量模型文档;audit_trace_map 旁路表按延迟实测裁定(D-t63#3);锁窗修复若动 DBTX 形状须先 D-(D-t24#6 演进注记);产物为 T16.4 的输入
- [ ] T15.6 mode=all 混跑窗硬闸 + 多副本整体演练 — M — 依赖 T15.2、T15.3、T15.4 — 验收:启动期能力探测硬闸(替 UPGRADE.md 人工纪律,D-t142①);多副本整体演练验收(哈希链混合版本窗 D-t93③ 口径随演练复核)

## M16 开源与交付面(排 T15.5 中后段)

- [x] T16.1 engine/enterprise 分层划线 — L — 验收:归属裁定清单(tenant/OIDC/MCP/IM 卡片/canvas 划线)+ module 拆分评估 + import 边界 lint 门(Q7 落地;现状 0% 物理分层)(D-2026-10-01-t161)
- [ ] T16.2 braid CLI 最小命令面 — M — 验收:建号/定义发布/触发三命令(README:143 明言 P0 无 CLI 建号入口)
- [ ] T16.3 一键 demo — M — 依赖 T16.2 — 验收:seed 脚本(迁移+建号+示例定义发布+触发)接入 dev-up.sh
- [ ] T16.4 性能数字进 README — S — 依赖 T15.5 — 验收:benchmark 数字成章 + agent-council 示例守护测试(README:241 自认无守护)
- [ ] T16.5 镜像构建/发布流水线 — M — 验收:零 Secrets 姿态下 registry push 方案评估与落地(GHCR GITHUB_TOKEN vs 本地导出)
- [ ] T16.6 回滚-重跑 UI + Playwright e2e — M — 验收:画布/执行详情回滚操作面(§5 留白 + rollback.go 零回滚入口);浏览器级 e2e(D-t141#2 登记回收)

## M17 安全与可靠性清账(穿插池:lane 间隙消化,不占波次)

- [x] T17.1 REST 全端点 scp 执法 — M — **红线优先** — 验收:scope 执法中间件覆盖 REST 全端点;scopesWithin/intersectScopes 与 auth 包词表单源收敛;MCP 面零回退;红线解除登记(D-t131 登记②⑤ 悬空移交收编:未执法前 scoped token 不得发不完全受信方)(D-2026-10-01-t171-rest-scp)
- [ ] T17.2 scoped token 审计 + login method 列 — S — 验收:scoped_token_issued 审计事件(主体/作用域/TTL/token 哈希至多)+ login_audit method 列迁移(OIDC 与口令登录可区分);迁移序号按台账排队
- [ ] T17.3 T7.4 授权断言收口 — M — 验收:outbox UPDATE/DELETE、audit_log ALTER 组件级授权断言;CheckAuditAppendOnly 表枚举扩 login_audit;RequeueNode runbook 手工原语文档化;按组件授权方案
- [ ] T17.4 asynq 双执行硬化 + 告警补面 — M — 验收:Inspector 硬化双执行残余窗(D-t25#10);告警四组:5a premature/fabricated、reaper 毒快照、DB 连接池、im_notified 投递对账
- [ ] T17.5 IM 签名短链预取闭合 — M — 验收:确认页第二跳/按钮改 POST(D-t143 决策6);instructions markdown 行内词法中和(D-t45 钓鱼面同族)
- [ ] T17.6 LDAP bind 桩实现 — M — 验收:IdentityProvider 缝面换 Authenticate 本体 + 构造装配(桩已钉死形状);容器化测试目录为验收方案

## P2 停车场(不排期,防遗忘)

- P1 三项(完整 taint 追踪 / PII 脱敏放开评估 / metrics exposition 无认证):继承 ROADMAP.md 文末唤醒触发器,原样引用
- 多租户重契约族(租户命名自治、idempotencyScopeV2、跨租户雇佣/平台跨租户读、IM per-tenant webhook):均须独立 D- 且重开已锁契约(login/uk_exec_idem/OIDC/审计归因/防枚举),无真实多租户运营诉求不立项
- SSRF 扩禁段(NAT64 64:ff9b::/96 等):待用户拍板;拍板后同步 ssrf_test 锚点、t10.1 e2e 网段重选、D-t102 代理口径继承
- 动态 DAG / 子工作流 / LLM 流式输出:仅 D- 评估条目按需插入,实现不排期
- 其余登记不修族维持原状,全录见 `braid-p2-survey.json`(仓外);本文不复制第二真源
