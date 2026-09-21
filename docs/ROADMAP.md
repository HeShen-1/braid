# braid 路线图(P0)

> 任务真源 = 本文件。完成即勾选 `[x]` 并 commit;新任务追加到对应里程碑末尾(标注发现日期);砍掉的任务划线注明原因,不删除。
> 规模:S = 半天内,M = 1-2 天,L = 3+ 天(全职当量)。约 33 任务:全职当量 6-8 周,断续投入约 3-4 个月。
> 里程碑依赖:M1 → M2 → (M3 ∥ M5)→ M4 → M6 → M7 → M8。任务级依赖单独标注,**冲突时以任务级为准**。P1 停车场见文末。

## 会话工作流

1. 会话开始:按 AGENTS.md 交接规则读 `.handoff/HANDOFF.md` → 读本文件定位下一个未完成任务。
2. 一次会话只做一个任务(或其子块);铁律见 CLAUDE.md(@AGENTS.md)。
3. 完成 → 勾选 + commit;ctx > 50% 或到断点 → `/handoff` 写交接。

## M1 契约先行(3-4 晚)

- [x] T1.1 DSL JSON Schema v1 — S — 验收:≥5 合法 + ≥5 非法样例校验测试通过;覆盖 llm/tool/branch/approval 四种节点、`{{nodes.<id>.output.<field>}}` 引用、画布元数据预留;prompt 模板与用户数据**分离标注字段**(未标注 = 非法样例);HTTP 工具 host 由白名单配置提供,**DSL 不允许填 host**(非法样例覆盖)— ✅ 2026-09-21:6 合法 + 16 非法样例,非法样例按拒绝原因锁定;经 3 视角对抗审查修复 15 项发现(引用形态钉死、host 黑名单递归+大小写不敏感+URL/IPv4 值禁令、基数帽、会签枚举收窄)
- [ ] T1.2 Go DSL 解析器 + DAG 校验 — M — 依赖 T1.1 — 验收:环检测、悬空引用、引用类型不匹配、inputs/user_data/节点 id 重名、branch 出边与 cases[].next ∪ {default} 目标一致、**发布时工具白名单校验**(引用未注册/未白名单工具的 DSL 发布被拒)均有单测
- [ ] T1.3 MySQL 迁移 v1(全部表)— M — 依赖 T1.1 — 验收:`migrate up/down` 幂等;表含 outbox、audit_log(分区键 + prev_hash + append-only)、node 输入快照、attempt、version 乐观锁、workflow 定义版本钉住;**parallel/fan-in 预留**(汇聚行确定性排序字段或等价设计说明)
- [ ] T1.4 开发环境 compose — S — 验收:一条命令起 MySQL + Redis×2(队列实例 noeviction + AOF everysec,缓存实例 allkeys-lru)+ Asynqmon(仅内网)

## M2 引擎内核(约 2 周)

- [ ] T2.1 状态机 + MySQL CAS 迁移 — M — 依赖 M1 — 验收:并发单测,两 worker 抢同一节点仅一个成功;**每次状态迁移写入输入快照,任意节点可从快照复现输入**(单测)
- [ ] T2.2 outbox 同事务写入 + relay 投递 — M — 依赖 T2.1 — 验收:**判别性故障测试**——状态写入后、outbox 写入前注入失败(断连/panic),断言两写同事务回滚;relay 标记成功前 kill -9 relay,重启后同事件重投递且下游幂等
- [ ] T2.3 Asynq 执行器(MaxRetry=1、确定性 TaskID、attempt 记录)— M — 依赖 T2.2 — 验收:重复投递被 ErrTaskIDConflict 收敛,attempt 落库
- [ ] T2.4 DAG 调度器(入度推进、**桩谓词路由**、执行完成回写)— M — 依赖 T2.3 — 验收:入度推进单测(多前驱、跳过分支);branch 用显式布尔字段路由,完整求值在 T3.4
- [ ] T2.5 reconciler(超时扫描、幂等重入队或标 FAILED、Redis 锁选主)— M — 依赖 T2.2 — 验收:kill -9 worker 中途 + 断 Redis 30s,**扫描间隔×3 内 execution 无人工干预达 SUCCEEDED/FAILED 终态**
- [ ] T2.6 最小同步审计写入器(直写 audit_log:触发者、attempt、事件类型;trace_id 字段占位)— S — 依赖 T2.1 — 验收:每次节点状态迁移产生对应审计行

里程碑验收:桩 LLM + 桩谓词端到端跑通「LLM → branch → LLM」,全程审计行可查。

## M3 节点实现(约 2 周)

- [ ] T3.1 LLMClient 抽象 + OpenAI 兼容适配器(退避重试、幂等键、usage/cost 台账、预算估算:未知模型 fallback 标 `estimated=true`)— M — 依赖 M2 — 验收:固定 mock usage 下台账金额与 usage 断言一致
- [ ] T3.2 工具 registry + ToolRunner 接口 + 本地进程实现 + per-call scoped token(签发/过期/传递)— M — 依赖 M2、T2.6 — 验收:token 过期后工具调用被拒;审计行与沙箱环境均不含平台主凭证
- [ ] T3.3 HTTP 工具 SSRF 运行时防护(Dialer.Control 拨号复核 IP、CheckRedirect 每跳重校验剥凭证、仅 https)— M — 依赖 T3.2 — 验收:私网段/metadata 段、重定向跳内网、DNS rebinding(TOCTOU)单测;**DSL 层 host 白名单拒绝用例**(host 字段出现即 schema 拒绝)
- [ ] T3.4 条件分支完整求值器(替换 T2.4 桩)— S — 依赖 M2 — 验收:引用比较、多条件组合、类型错误单测
- [ ] T3.5 内置工具:http + file_read(路径白名单)— M — 依赖 T3.2、T3.3 — 验收:白名单外路径/主机被拒;db_query 在 P1 停车场

里程碑验收:真实 LLM 跑通「文件路径输入 → file_read → 摘要 → 完整条件分支」示例(测试代码触发)。

## M4 审批(约 1.5 周)

- [ ] T4.1 审批节点执行路径(挂起 WAITING_APPROVAL、输入快照)— S — 依赖 M2 — 验收:挂起后无阻塞中的 worker 任务;审计行记录挂起事件
- [ ] T4.2 审批 API(或签、超时配置、经 outbox 续跑,`resume:<exec>:<node>` 确定性 TaskID)— M — 依赖 T4.1、T5.1 — 验收:enqueue 失败注入下经对账恢复,扫描间隔×3 内达终态;未认证或非审批人调用被拒
- [ ] T4.3 超时扫描器(策略可配:升级等待(默认)/ fail、升级通知钩子、永不自动通过)— S — 依赖 T4.2 — 验收:fail 策略下超时节点显式标 FAILED
- [ ] T4.4 Web 审批页(登录、待办列表、详情、批准/驳回)— M — 依赖 T4.2 — 验收:页面动作真实驱动流程续跑
- [ ] T4.5 SMTP 通知 — S — 依赖 T4.2 — 验收:fake SMTP 集成测试断言邮件含审批链接与工作流上下文

## M5 用户、RBAC 与 API 层(约 1.5 周)

- [ ] T5.1 用户/角色模型 + JWT + bcrypt — M — 依赖 M1 — 验收:登录、过期 token、角色越权单测
- [ ] T5.2 执行时 RBAC × 运行时双重校验 + Gin 中间件 — M — 依赖 T5.1、T3.2 — 验收:越权工具调用被拒并落审计
- [ ] T5.3 IdentityProvider 接口桩 — S — 依赖 T5.1 — 验收:桩实现可替换内置认证(编译 + 替换单测)
- [ ] T5.4 definition CRUD API(草稿/发布/版本;发布时工具白名单校验)— M — 依赖 T5.1、T1.2 — 验收:运行实例钉住创建时版本;引用非法工具的发布返回 4xx
- [ ] T5.5 execution 触发/查询/列表 API — M — 依赖 T5.4 — 验收:触发后 execution 创建并推进;查询返回节点状态与审计指针

## M6 审计与观测(约 1.5 周)

- [ ] T6.1 审计写入器升级:异步批量、PII 脱敏钩子、保留期配置、prev_hash 占位 — M — 依赖 T2.6 — 验收:脱敏钩子对标记字段生效;批量写无丢失(对账单测)
- [ ] T6.2 OTel 贯穿(API → worker → LLM/工具)— M — 依赖 T6.1、T3.1、T3.2 — 验收:API 触发一次执行,audit_log 行 trace_id 与本次 trace 一致且可按其查询;审批挂起 → 续跑经 **trace link** 关联
- [ ] T6.3 审计查询 API(按 execution/trace/人过滤)— S — 依赖 T6.1 — 验收:三种过滤维度集成测试
- [ ] T6.4 Prometheus 指标 + 告警规则(死信、审批积压、卡死节点、429 率、队列 Redis 内存水线)— M — 依赖 T4.1、T3.1 — 验收:promtool test rules 通过;注入死信/审批挂起后指标计数断言

## M7 交付硬化(约 1 周)

- [ ] T7.1 配置体系(连接池上限、审计保留期、各类超时全配置化)+ 部署文档配额表 — M — 依赖 M6 — 验收:改配置 → 行为变化单测(池上限生效)
- [ ] T7.2 tiktoken 离线 loader + 词表随安装包 + 启动预构建 tokenizer — M — 依赖 T3.1 — 验收:断网环境冷启动成功,首个 LLM 节点不触发任何外网请求
- [ ] T7.3 安装自检(Redis 策略/AOF、词表、迁移版本)— M — 依赖 T7.2 — 验收:错误配置下拒绝启动并输出修复提示
- [ ] T7.4 交付 compose 定稿 + 升级手册(golang-migrate + N-1 兼容窗口)— S — 依赖 T7.1 — 验收:对上一版本数据卷完成一次升级演练

## M8 验收(约 1 周)

- [ ] T8.1 CI 故障注入套件:worker 中途 kill / Redis 断连 / 审批 enqueue 失败 / 双投递 — L — 依赖 M2-M6 — 验收:全部注入下扫描间隔×3 内达终态,CI 绿
- [ ] T8.2 端到端示例:经 API 触发,文件路径输入 → file_read → LLM 解析 → 合规审查 → 摘要 → 人工审批(Web 页)→ 报告 — M — 依赖 T5.5、T3.5、T4.4 — 验收:全程审计链完整(trace 串联)
- [ ] T8.3 README(架构图、快速上手、故障注入演示)— M — 依赖 T8.1 — 验收:新环境按 README 30 分钟内跑通示例

## P1 停车场(不排期,防遗忘)

画布(React Flow 候选,DSL 已留元数据)、并行节点、回滚/断点续跑 UI、容器沙箱 + egress 默认拒绝代理、MCP 双向、审计哈希链、会签、OIDC/LDAP 插件、IM 审批卡片、工具包签名、Helm、SaaS 多租户、完整 taint 追踪、db_query 内置工具(只读账号/参数化/凭证作用域)。
