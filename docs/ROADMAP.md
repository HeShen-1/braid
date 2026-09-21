# braid 路线图(P0)

> 任务真源 = 本文件。完成即勾选 `[x]` 并 commit;新任务追加到对应里程碑末尾(标注发现日期);砍掉的任务划线注明原因,不删除。
> 规模:S = 半天内,M = 1-2 天,L = 3+ 天(全职当量)。总计约 30 任务:全职当量 6-8 周,断续投入约 3-4 个月。
> 里程碑依赖:M1 → M2 → (M3 ∥ M5)→ M4 → M6 → M7 → M8。任务级依赖单独标注。P1 停车场见文末。

## 会话工作流

1. 会话开始:按 AGENTS.md 交接规则读 `.handoff/HANDOFF.md` → 读本文件定位下一个未完成任务。
2. 一次会话只做一个任务(或其子块);铁律见 CLAUDE.md。
3. 完成 → 勾选 + commit;ctx > 50% 或到断点 → `/handoff` 写交接。

## M1 契约先行(3-4 晚)

- [ ] T1.1 DSL JSON Schema v1 — S — 验收:≥5 合法 + ≥5 非法样例的校验测试通过;覆盖 llm/tool/branch/approval 四种节点、`{{nodes.<id>.output.<field>}}` 引用、画布元数据预留字段
- [ ] T1.2 Go DSL 解析器 + DAG 校验 — M — 依赖 T1.1 — 验收:环检测、悬空引用、引用类型不匹配均有单测
- [ ] T1.3 MySQL 迁移 v1(全部表)— M — 依赖 T1.2 — 验收:`migrate up/down` 幂等;表含 outbox、audit_log(分区键 + prev_hash + append-only)、node 输入快照、attempt、version 乐观锁、workflow 定义版本钉住
- [ ] T1.4 开发环境 compose — S — 验收:一条命令起 MySQL + Redis×2(队列实例 noeviction + AOF everysec,缓存实例 allkeys-lru)+ Asynqmon(仅内网)

## M2 引擎内核(约 2 周)

- [ ] T2.1 状态机 + MySQL CAS 迁移 — M — 依赖 M1 — 验收:并发单测,两 worker 抢同一节点仅一个成功
- [ ] T2.2 outbox 同事务写入 + relay 投递 — M — 依赖 T2.1 — 验收:状态变更与 outbox 同事务提交;relay 幂等、失败重试、成功标记
- [ ] T2.3 Asynq 执行器(MaxRetry=1、确定性 TaskID、attempt 记录)— M — 依赖 T2.2 — 验收:重复投递被 ErrTaskIDConflict 收敛
- [ ] T2.4 DAG 调度器(入度推进、branch 求值、执行完成回写)— M — 依赖 T2.3
- [ ] T2.5 reconciler(超时扫描、幂等重入队或标 FAILED、Redis 锁选主)— M — 依赖 T2.2 — 验收:手动 kill -9 worker 中途 + 断 Redis 30s,流程自动恢复(正式 CI 化在 M8)

里程碑验收:桩 LLM 节点端到端跑通「LLM → branch → LLM」工作流。

## M3 节点实现(约 2 周)

- [ ] T3.1 LLMClient 抽象 + OpenAI 兼容适配器(退避重试、幂等键、usage/cost 台账)— M — 依赖 M2
- [ ] T3.2 工具 registry + ToolRunner 接口 + 本地进程实现 — M — 依赖 M2
- [ ] T3.3 HTTP 工具 SSRF 防护(Dialer.Control 拨号复核 IP、CheckRedirect 每跳重校验剥凭证、仅 https)— M — 依赖 T3.2 — 验收:单测覆盖私网段/metadata 段、重定向跳内网、DNS rebinding(TOCTOU)
- [ ] T3.4 条件分支求值器 — S — 依赖 M2
- [ ] T3.5 内置工具:db_query / file_read / http — M — 依赖 T3.2、T3.3

里程碑验收:真实 LLM 跑通「文档输入 → 摘要 → 按条件分支」示例。

## M4 审批(约 1.5 周)

- [ ] T4.1 审批节点执行路径(挂起 WAITING_APPROVAL、输入快照)— S — 依赖 M2
- [ ] T4.2 审批 API(或签、超时配置、经 outbox 续跑,resume:<exec>:<node> 确定性 TaskID)— M — 依赖 T4.1 — 验收:enqueue 失败注入下流程经对账恢复,不永久卡死
- [ ] T4.3 超时扫描器(默认 72h 可配、升级通知钩子、永不自动通过)— S — 依赖 T4.2
- [ ] T4.4 Web 审批页(登录、待办列表、详情、批准/驳回)— M — **依赖 M5** — 验收:审批动作真实驱动流程续跑
- [ ] T4.5 SMTP 通知 — S — 依赖 T4.2

## M5 用户与 RBAC(约 1 周)

- [ ] T5.1 用户/角色模型 + JWT + bcrypt — M — 依赖 M1
- [ ] T5.2 执行时双重校验(定义白名单 × 用户 RBAC)+ Gin 中间件 — M — 依赖 T5.1、T3.2 — 验收:越权调用被拒并落审计
- [ ] T5.3 IdentityProvider 接口桩 — S — 依赖 T5.1

## M6 审计与观测(约 1.5 周)

- [ ] T6.1 审计写入器(append-only、异步批量、PII 脱敏钩子、prev_hash 字段占位)— M — 依赖 M2
- [ ] T6.2 OTel 贯穿(API → worker → LLM/工具),trace_id 写入审计行 — M — 依赖 T6.1
- [ ] T6.3 审计查询 API(按 execution/trace/人过滤)— S — 依赖 T6.1
- [ ] T6.4 Prometheus 指标 + 告警规则(死信、审批积压、卡死节点、429 率)— M — 依赖 M2

## M7 交付硬化(约 1 周)

- [ ] T7.1 配置体系(连接池上限、审计保留期、各类超时全配置化)+ 部署文档配额表 — M — 依赖 M6
- [ ] T7.2 tiktoken 离线 loader + 词表随安装包 + 启动预构建 tokenizer — M — 依赖 T3.1
- [ ] T7.3 安装自检(Redis 策略/AOF、词表、迁移版本;不符合即拒绝启动)— M — 依赖 T7.2
- [ ] T7.4 交付 compose 定稿 + 升级手册(golang-migrate + N-1 兼容窗口)— S — 依赖 T7.1

## M8 验收(约 1 周)

- [ ] T8.1 CI 故障注入套件:worker 中途 kill / Redis 断连 / 审批 enqueue 失败 / 双投递 — L — 依赖 M2-M6
- [ ] T8.2 端到端示例:文档上传 → 解析 → 合规审查 → 摘要 → 人工审批 → 报告 — M — 依赖全部
- [ ] T8.3 README(架构图、快速上手、故障注入演示录屏)— M — 依赖 T8.1

## P1 停车场(不排期,防遗忘)

画布(React Flow 候选,DSL 已留元数据)、并行节点、回滚/断点续跑 UI、容器沙箱 + egress 默认拒绝代理、MCP 双向、审计哈希链、会签、OIDC/LDAP 插件、IM 审批卡片、工具包签名、Helm、SaaS 多租户、完整 taint 追踪。
