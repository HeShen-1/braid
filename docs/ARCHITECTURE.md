# braid 架构定稿 v1

> 2026-09-21 定稿。经 4 轮决策评审(18 项)+ 6 视角对抗审计(33 条发现,8 条 blocking/high 经独立反驳者逐条验证,零翻案)。
> 目标:个人开发的 Go 后端深度项目,远期商业化(不急),近期作为可对外展示的深度作品打磨。

## 0. 定位与差异化

- 企业级 Agent 工作流编排平台:可视化 DAG(P1)、人在回路、全链路审计、工具权限隔离。
- **修正后的差异化认知**(审计结论):HITL + 审计在 2026 年已是 n8n/Dify/Temporal 标配,不是卖点;真正的护城河是工具沙箱的安全审查级实现、审计防篡改、合规交付深度。
- 交付形态:私有化,一镜像四入口(api/worker/relay/reaper 四二进制,同一代码库交付)+ docker-compose。

## 1. 已锁定的 18 项决策

| # | 决策点 | 结论 |
|---|--------|------|
| Q1 | 产品定位 | 商业化目标,节奏不急,按商业标准设计 |
| Q2 | MVP 范围 | P0:JSON DSL + LLM/工具/条件分支/人工审批节点 + 审计 + 节点级重试;P1:画布、并行、断点续跑、回滚 |
| Q3 | 状态真相源 | MySQL(每次节点状态变更事务落库);Redis 只做锁/队列/缓存 |
| Q4 | 人在回路机制 | 状态机挂起 `WAITING_APPROVAL` + 事件驱动唤醒,worker 不阻塞 |
| Q5 | 权限模型 | 声明式白名单 + RBAC(执行时强制) + 工具沙箱(目标态) |
| Q6 | 部署形态 | 私有化优先,docker-compose 交付 |
| Q7 | 开源策略 | MVP 闭源,目录按 engine/enterprise 分层,公测前再定 |
| Q8 | 沙箱 | P1 容器级;P0 用 ToolRunner 抽象隔离设计 |
| Q9 | 审批策略 | 超时可配默认 72h;超时=升级通知+保持等待,默认可配 fail;永不自动通过;或签 P0、会签 P1 |
| Q10 | 审计等级 | append-only + PII 脱敏钩子 + 保留期可配;`prev_hash` 字段建表即留,哈希链 P1 |
| Q11 | 数据流 | 显式数据流:`{{nodes.<id>.output.<field>}}` 引用,无共享黑板 |
| Q12 | 审批触达 | 内置 Web 登录 + 极简待办/审批页 + SMTP;IM 卡片 P1 |
| Q13 | 用户体系 | P0 内置用户/角色 + JWT + bcrypt;`IdentityProvider` 接口桩,OIDC/LDAP P1 |
| Q14 | 交付打包 | 一镜像四入口 + compose(编排 MySQL/Redis;Asynqmon 仅 dev 观测栈),golang-migrate;Helm P1 |
| Q15 | 人力 | 个人开发 |
| Q16 | 引擎底座 | **自研**(不用 Temporal/Hatchet):一镜像四入口交付红利 + 全链路自主可控、行为可验证;Temporal 作对照读物 |
| Q17 | 画布 | 维持 P1,DSL 预留画布元数据(节点 id/位置);启动售卖前提为售前 P0 |
| Q18 | 工具协议 | P0 内部 registry 薄接口;P1 双向 MCP(暴露为 MCP server + 消费外部 MCP 工具) |

底层假设:回滚 = 回到某节点重跑(非补偿事务),依赖每节点输入快照;工作流定义带版本,运行实例钉住创建时版本;LLM 走 OpenAI 兼容端点,密钥进配置不进代码/审计。

## 2. 技术栈(含审计修正)

- Go + Gin;MySQL(真相源);**Redis 双实例**:队列实例(noeviction + AOF everysec + 水线告警)与热缓存实例(allkeys-lru 可用)物理分离,compose 默认值 + 启动自检;
- Asynq:执行器,**MaxRetry=1**,重试权威收归平台节点层;`v0.26.0(2026-02)` 维持慢节奏,收敛到内部接口后 pin 版本;
- LLM:自研薄 `LLMClient` 封装,OpenAI 兼容适配器手写 net/http、零 SDK(不引 go-openai——其无 extra_body 字段,国产模型必需参数发不出,平台经 `Request.ExtraBody` 顶层合并);平台原语只用 Chat Completions,**不用 Responses API**(与多供应商/私有化/本地审计冲突);
- tiktoken-go:自研内嵌 BpeLoader(go:embed 词表随二进制 + SHA-256 校验,零额外依赖)+ 词表随安装包分发 + 启动预构建 DSL 引用的全部 tokenizer(失败拒绝启动);计费真相源 = API 返回 usage,tiktoken 仅预算估算,未知模型 fallback 标 `estimated=true`;
- 可观测:Prometheus + log/slog(标准库结构化日志)+ **OpenTelemetry**(trace_id 贯穿并写入审计行);golang-migrate。

明确不用:Redis 分布式锁做状态迁移(改 MySQL CAS);Asynq 默认重试语义;Responses API 做平台原语。

## 3. 核心架构

### 3.1 执行引擎(自研状态机)

表:`workflow_definition`(版本化,草稿/发布)→ `workflow_execution` → `node_execution`;`outbox`;`audit_log`。

- **状态迁移 = MySQL 条件更新 CAS**:`UPDATE node_execution SET status=EXECUTING, version=version+1 WHERE id=? AND status=PENDING AND version=?`;Redis 锁仅留给 reconciler 选主。
- **outbox + relay + reconciler**(P0 硬性验收,非优化项):节点状态变更与"待入队事件"同事务写 outbox,独立 relay 投递 Asynq,成功才标记;reconciler 周期扫描 QUEUED/RUNNING 超时且无活动执行者的 execution,幂等重入队或显式标 FAILED。此通道同时是回滚与断点续跑的依赖。
- **审批续跑用确定性 TaskID**:`resume:<execution_id>:<node_id>:<attempt_no>`(四段式定稿见 0003 迁移;原三段式在 (exec,node) 二次续跑时撞 TaskID,是已修豁口),Asynq `ErrTaskIDConflict` 天然幂等去重。
- 节点执行 = Asynq 短任务,超时按节点类型可配;attempt 级记录(幂等键),重复执行可对账。
- 每节点落库时存**输入快照**(回滚/断点续跑的地基)。

### 3.2 DSL(显式数据流)

- JSON + JSON Schema 校验;引用语法 `{{nodes.<id>.output.<field>}}`;无共享可变黑板——每次执行的输入可精确快照、并行天然安全。
- 节点类型:llm / tool / branch / approval(T9.1 起)+ parallel(Scatter-Gather:foreach 数组展开 fan-out + gather 输入序汇聚,单节点多实例行模型,见 D-2026-09-28-t91)。
- 预留画布元数据(节点位置等);prompt 模板与用户数据在 DSL 里强制分离标注(间接注入缓解,完整 taint 追踪 P1)。

### 3.3 人在回路

- 审批节点 → `WAITING_APPROVAL`,任务正常结束释放 worker;审批 API 持久化结果 → 经 outbox 通道触发续跑任务。
- 或签 P0、会签 P1;超时默认 72h 可配,超时升级通知 + 保持等待,策略可配为 fail,永不自动通过。
- 触达:内置 Web 待办/审批页 + SMTP 邮件;钉钉/企微/Slack 卡片走通知插件 P1。

### 3.4 安全

- 双重校验:**定义时白名单 × 执行时 RBAC**,均强制。
- `ToolRunner` 抽象:P0 本地进程实现,P1 换 Docker 容器沙箱不动核心;沙箱运行时基线(非 runc 默认即安全)在 P1 安全评审时定义。
- 凭证:per-call 短时 scoped token;平台主凭证(DB/LLM key)永不进沙箱环境、永不落审计。
- SSRF 三层修(P0):DSL 层 host 静态白名单(LLM 只填 path/query);运行时 `net.Dialer.Control` 在拨号瞬间复核实际 IP(拒私网/环回/链路本地/CGNAT/metadata 段,防 DNS rebinding TOCTOU)+ `CheckRedirect` 每跳重校验并剥 Authorization/Cookie + 仅 https;网络层 egress 默认拒绝代理 P1。
- 插件:P0 内部 registry;P1 工具包签名 + MCP 双向。
- 内置工具 P0:http + file_read(路径白名单);db_query 移 P1(只读账号、参数化、凭证作用域)。

### 3.5 审计

- append-only(不提供 update 接口);分区键(execution 日期)建表即定;异步批量写,与状态变更分表;`prev_hash` 建表即留,哈希链 P1。
- 记录:触发者身份、attempt 级 prompt/模型返回/usage+cost、工具入参出参、每行 otel trace_id;审批等待用 trace link 串联。
- PII 脱敏钩子 P0;保留期可配。

### 3.6 用户与 API

- 内置用户/角色(admin / workflow 作者 / 审批人),JWT + bcrypt;`IdentityProvider` 接口桩;OIDC/LDAP 插件 P1。
- Gin REST API:definition CRUD(草稿/发布/版本)、execution 触发与查询、审批操作、审计查询、待办列表。

## 4. P0 里程碑(建议顺序)

1. **契约先行**:DSL JSON Schema + 全部表结构(outbox、审计分区键、prev_hash、attempt、输入快照)。
2. **引擎内核**:状态机 + CAS 迁移 + outbox relay + reconciler + Asynq 执行器(MaxRetry=1)+ 最小同步审计写入器(触发者/attempt;M6 再升级为异步批量)。
3. **节点**:LLM(重试/幂等/usage 台账)+ 工具(registry + HTTP SSRF 防护)+ 条件分支。
4. **审批**:挂起/续跑(确定性 TaskID)/超时升级 + Web 审批页 + SMTP。
5. **用户/RBAC/JWT + API 层**:definition CRUD(草稿/发布/版本,发布时工具白名单校验)+ execution 触发/查询。
6. **审计异步批量/脱敏/保留期 + 查询 API + OTel 贯穿 + Prometheus 告警集**(死信、审批积压、卡死节点、429 率、队列 Redis 内存水线)。
7. **compose 交付** + golang-migrate + 安装自检(Redis 策略、词表、tokenizer 预构建、连接池上限全配置化)。
8. **故障注入测试**(CI 化):worker 中途 kill、Redis 断连、审批 enqueue 失败、双投递——耐久性承诺的核心证明。端到端验收场景:经 definition/execution API 触发,文件路径输入 → file_read → LLM 解析 → 合规审查 → 摘要 → 人工审批 → 报告。

## 5. P1

画布(React Flow 候选)、回滚/断点续跑 UI、容器沙箱 + egress 代理、MCP 双向、审计哈希链、会签、OIDC/LDAP、IM 审批卡片、工具包签名、Helm、SaaS 多租户、完整 taint 追踪、db_query 内置工具(只读账号/参数化/凭证作用域)。

## 附:审计事实链(设计依据)

- Redis Sentinel 官方文档:整体最终一致,"there is always a window for losing acknowledged writes";Jepsen 实测单次 partition 丢 1126 条已确认写入(aphyr.com/posts/283)。
- 队列 Redis 必须 noeviction 是生态共识:Sidekiq #5712、BullMQ #2737(非 noeviction 直接拒启)。
- Asynq at-least-once 只覆盖入队之后;`EnqueueContext` 丢底层错误类型链(hibiken/asynq #1166,%v 非 %w)。
- Asynq 任务状态全在 Redis、无外部持久化 → 必须应用层对账。
- SSRF/DNS rebinding:Go 无开箱防护,需 `Dialer.Control`(Doyensec safeurl, 2022-12);同类漏洞 2026 年仍在批量出现(CVE-2026-55524);间接注入链 EchoLeak(CVE-2025-32711)实证"文档→LLM→网络调用"。
- AOF everysec 官方承认约 1 秒丢失窗;`WAIT` 不构成 CP。
- tiktoken-go 默认运行时从公网下载 BPE 词表 → 内网私有化首个 run 即失败。
