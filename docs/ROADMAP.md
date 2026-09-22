# braid 路线图(P0)

> 任务定义与依赖真源 = 本文件;执行状态真源 = GitHub issues(认领/进行中/闭环,`gh issue list`)。**2026-09-22 起本文件不再维护勾选**(存量 `[x]` 保留为历史);勾选提交随任务 PR,merge 即生效,PR 描述带 `Closes #N` 自动关 issue(D-2026-09-22-git-native-collab)。新任务追加到对应里程碑末尾(标注发现日期);砍掉的任务划线注明原因,不删除。
> 规模:S = 半天内,M = 1-2 天,L = 3+ 天(全职当量)。约 37 任务:全职当量 6-8 周,断续投入约 3-4 个月。
> 里程碑依赖:M1 → M2 → (M3 ∥ M5)→ M4 → M6 → M7 → M8。任务级依赖单独标注,**冲突时以任务级为准**。P1 停车场见文末。

## 会话工作流

1. 会话开始:按 AGENTS.md 交接规则读 `.handoff/HANDOFF.md` → 读本文件定位下一任务(执行状态看 `gh issue list`,未建 issue 的先建/认领)。
2. 一次会话只做一个任务(或其子块);铁律见 CLAUDE.md(@AGENTS.md)。
3. 完成 → 勾选随任务 PR 提交(merge 即生效 + `Closes #N`);ctx > 50% 或到断点 → `/handoff` 写交接。

## M1 契约先行(3-4 晚)

- [x] T1.1 DSL JSON Schema v1 — S — 验收:≥5 合法 + ≥5 非法样例校验测试通过;覆盖 llm/tool/branch/approval 四种节点、`{{nodes.<id>.output.<field>}}` 引用、画布元数据预留;prompt 模板与用户数据**分离标注字段**(未标注 = 非法样例);HTTP 工具 host 由白名单配置提供,**DSL 不允许填 host**(非法样例覆盖)— ✅ 2026-09-21:6 合法 + 16 非法样例,非法样例按拒绝原因锁定;经 3 视角对抗审查修复 15 项发现(引用形态钉死、host 黑名单递归+大小写不敏感+URL/IPv4 值禁令、基数帽、会签枚举收窄)
- [x] T1.2 Go DSL 解析器 + DAG 校验 — M — 依赖 T1.1 — 验收:环检测、悬空引用、引用类型不匹配、inputs/user_data/节点 id 重名、branch 出边与 cases[].next ∪ {default} 目标一致、**发布时工具白名单校验**(引用未注册/未白名单工具的 DSL 发布被拒)均有单测 — ✅ 2026-09-21:29 负例(按发现码集合精确断言,防级联误报)+ 7 正例(防过度拒绝)+ 白名单 4 例 + T1.1 合法样例全量过双层校验;另落实 schema 契约中的祖先约束(工作流级 outputs 豁免)与 tool.inputs 引用形态钉死(每个 {{ 必须是合法完整引用)
- [x] T1.3 MySQL 迁移 v1(全部表)— M — 依赖 T1.1 — 验收:`migrate up/down` 幂等;表含 outbox、audit_log(分区键 + prev_hash + append-only)、node 输入快照、attempt、version 乐观锁、workflow 定义版本钉住;**parallel/fan-in 预留**(汇聚行确定性排序字段或等价设计说明)— ✅ 2026-09-21:3 视角独立设计 + 评审合成 + 6 视角对抗审计(51 发现,blocking/high 经双反驳验证:8 confirmed/4 disputed/0 refuted),全部修复;真 MySQL 8.0.46 容器验证 up/up/down/down/up 幂等 + 结构契约断言(15 分区、NOT NULL 修复锚、6 CHECK、对账索引);雪花 ID/DATETIME(3) UTC/ENUM 末尾追加/日分区 no-MAXVALUE;fan-in 预留 = uk_node_exec(execution_id,node_id) 汇聚按 node_id 字典序读前驱输出;51 条发现全文归档 docs/audits/t1.3-mysql-ddl-audit.json(39 条 medium/low 未采纳,后续任务素材)
- [x] T1.4 开发环境 compose — S — 验收:一条命令起 MySQL + Redis×2(队列实例 noeviction + AOF everysec,缓存实例 allkeys-lru)+ Asynqmon(仅内网)— ✅ 2026-09-21:deploy/dev.docker-compose.yml(name: braid-dev)四容器一条命令全起且 healthy;redis-cli 实测队列 noeviction/appendonly yes/appendfsync everysec、缓存 allkeys-lru/256mb/无持久化;四端口(3306/6379/6380/8080)全部仅 127.0.0.1 绑定;Asynqmon /api/queues 实读队列实例;缺 deploy/.env 起栈响亮报错;与 braid-t13(13306)并行无冲突;asynqmon 钉 digest(内容 = 0.7.2,镜像源版本 tag 403)

## M2 引擎内核(约 2 周)

- [x] T2.1 状态机 + MySQL CAS 迁移 — M — 依赖 M1 — 验收:并发单测,两 worker 抢同一节点仅一个成功;**每次状态迁移写入输入快照,任意节点可从快照复现输入**(单测)— ✅ 2026-09-21:internal/engine(states/errors/dbtx/nodecas/wfcas/attempt/snapshot);链路 = 3 视角独立设计 → 评审合成 → 6 视角对抗审计(去重 17 发现,blocking/high 双反驳:4 confirmed(其中 3 条同根因)/0 disputed/0 refuted,修复全落地);并发证据链 = 确定性双连接(A 提交/回滚双裁决)+ 阻塞证明(败方 UPDATE 在胜方持锁期不返回)+ N 路 barrier(2/4/8)恰一胜 + 同 status 同 version 双提交恰一胜(单独证明 version 列仲裁);快照 = 全状态矩阵后逐行全称断言(语义深比较 + 读回确定性);ENUM 词表含序钉死(末尾追加契约);审计修复 = 败方复查 SELECT 改 FOR UPDATE 当前读(RR read view 会污染 Observed)、CanonicalDSN 补 time_zone='+00:00' 会话半边契约 + 钉死测试(此前靠容器系统时区碰巧 UTC)、TouchExecution 同毫秒幂等 bump(changed-rows=0 ≠ 缺行)、变异验证加固清列/version 谓词断言
- [x] T2.2 outbox 同事务写入 + relay 投递 — M — 依赖 T2.1 — 验收:**判别性故障测试**——状态写入后、outbox 写入前注入失败(断连/panic),断言两写同事务回滚;relay 标记成功前 kill -9 relay,重启后同事件重投递且下游幂等 — ✅ 2026-09-21:internal/{outbox,relay,snowflake,redistest} + cmd/relay;链路 = 3 视角独立设计 → 评审合成(单事件单事务裁决)→ 6 视角对抗审计(43 发现:1 high/11 medium/31 low,0 blocking,全部修复或有记录移交)+ 4 变异验证(其一揪出 SKIP LOCKED 测试真空洞断言,已改 payload 级投递归属断言);验收① = 正例对照 + panic 变体 + KILL 断连变体(CONNECTION_ID 经事务自身连接,fn 体内串行零竞态);验收② = TestMain 子进程 READY 哨兵行(携行身份)+ stdin 阻塞构造性证明 → SIGKILL → A1-A4 → 重启冲突收敛 B1-B4 + 冲突检测门(真实双入队 errors.Is);另 SKIP LOCKED 阻塞证明反演、双 relay 恰 M 次投递、退避/DEAD、available_at 门控(带活性对照)、停机中断不动行、雪花全套(布局/单调/有界回拨/序列耗尽/-race 唯一);errTaskIDConflict 冲突含 archived(滞留无保证上界)→ DISPATCHED 安全前提 = TaskID 词表不变量(resume: 豁口记档,T4.2 承接);DECISIONS 追加 D-2026-09-21-snowflake-rollback-bounded(DDL 头注释同步修订);t13 栈加 16379 Redis(noeviction、无持久化、tmpfs)
- [ ] T2.3 Asynq 执行器(MaxRetry=1、确定性 TaskID、attempt 记录)— M — 依赖 T2.2 — 验收:重复投递被 ErrTaskIDConflict 收敛,attempt 落库;T2.2 审计移交:①执行级幂等锚 = worker attempt 唯一键(ErrTaskIDConflict 仅任务记录存活期内收敛,无 Retention 下完成即删,重投会二次执行);②MaxRetry=1 实际语义 = asynq 自管一次内部重试(server 默认 15-45s 退避)后 archived,需显式决定 RetryDelayFunc 与平台重试的叠加;③实证 relay 锁窗(FOR UPDATE SKIP LOCKED 跨 Redis RTT)与引擎同事务 outbox.Append INSERT 的 gap 锁交互;④测试包接入 redistest 时必须选独立 BRAID_TEST_REDIS_DB(go test 跨包并行 FlushDB 互踩)
- [ ] T2.4 DAG 调度器(入度推进、**桩谓词路由**、执行完成回写)— M — 依赖 T2.3 — 验收:入度推进单测(多前驱、跳过分支);branch 用显式布尔字段路由,完整求值在 T3.4
- [ ] T2.5 reconciler(超时扫描、幂等重入队或标 FAILED、Redis 锁选主)— M — 依赖 T2.2 — 验收:kill -9 worker 中途 + 断 Redis 30s,**扫描间隔×3 内 execution 无人工干预达 SUCCEEDED/FAILED 终态**;reaper 兼任 audit_log 分区水位作业(≥7 天未来分区预建 ADD PARTITION、超保留期整分区 DROP——T1.3 审计确认承载)+ outbox DISPATCHED 行按 dispatched_at 7 天批量清理(T2.2 审计移交);前置约束(D-2026-09-22):T2.3 落地的确定性 TaskID 契约必须先钉死(写进 issue body)
- [ ] T2.6 最小同步审计写入器(直写 audit_log:触发者、attempt、事件类型;trace_id 字段占位)— S — 依赖 T2.1 — 验收:每次节点状态迁移产生对应审计行
- [x] T2.7 测试隔离 env 化(worktree 并行地基,2026-09-22 D-2026-09-22-git-native-collab)— S — 依赖 T2.2 — 验收:schema 名 = base + "_" + sha256(worktree 根绝对路径)[:8](base ∈ {braid_engine_test, braid_relay_test, braid}),wtKey 由 os.Getwd() 向上找 go.mod 自动推导(kill_test 子进程 cwd 继承天然成立),`BRAID_TEST_SCHEMA_SUFFIX` 可选覆盖(CI/调试);新增 `BRAID_TEST_MYSQL_DSN`(sqltest 与 migrate_test 共用此口,默认值不变),db 包测试脱离 dev 库 `braid` 改用 `braid_<wtKey>` 独立 schema;`BRAID_TEST_REDIS_DB` 保留不换名、语义扩为 worktree 槽位(DB = 槽位×4 + 包基号,分配表见 AGENTS.md;redistest 零代码改动,组合发生在消费包 TestMain);**单 worktree 行为零变化(所有默认值=现状)**;约 4 文件、净增 40-60 行、零 compose 改动;TDD。c 级(每 worktree 独立栈)按需升级:b 删 t13 compose 硬编码 container_name(纯减法)+ 端口改 `${BRAID_T13_*_PORT:-默认}`(host = base + 槽位×10)+ `-p braid-t13-<slot>`,触发条件 = 活进程联调/整栈重置

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

- [x] T5.1 用户/角色模型 + JWT + bcrypt — M — 依赖 M1 — 验收:登录、过期 token、角色越权单测 — ✅ 2026-09-22:internal/auth(存储 JSON_ARRAYAGG 角度聚合 + bcrypt + JWT HS256 golang-jwt/v5 + Authenticate/RequireRole)+ 迁移 0002(users/user_role);链路 = 3 视角设计判定面板(最小正确/安全硬化/仓库同构,双评委独立收敛 minimal + 6 项嫁接)→ TDD → 6 视角×security 对抗审计(17 发现逐条双反驳,10 修复:防枚举三件套统一 sentinel+dummy 时序均衡+密码先验后查状态、SignToken TTL 上界自洽、CredentialsError %q 防日志伪造、保留字 system 双闸、注释准确性);变异锚 6 处实证(alg 白名单/exp 必存/词表 fail-closed/DISABLED 判定序/WithTx 连接归还/时序均衡存在性,全 部「改防线即红」);migrate_test 断言域分化(表 9/CHECK 8,后续域只加自身 map);角色三态 admin/author/approver 与 Go 常量含序对拍钉死
- [ ] T5.2 执行时 RBAC × 运行时双重校验 + Gin 中间件 — M — 依赖 T5.1、T3.2 — 验收:越权工具调用被拒并落审计
- [ ] T5.3 IdentityProvider 接口桩 — S — 依赖 T5.1 — 验收:桩实现可替换内置认证(编译 + 替换单测)
- [ ] T5.4 definition CRUD API(草稿/发布/版本;发布时工具白名单校验)— M — 依赖 T5.1、T1.2 — 验收:运行实例钉住创建时版本;引用非法工具的发布返回 4xx
- [ ] T5.5 execution 触发/查询/列表 API — M — 依赖 T5.4 — 验收:触发后 execution 创建并推进;查询返回节点状态与审计指针

## M6 审计与观测(约 1.5 周)

- [ ] T6.1 审计写入器升级:异步批量、PII 脱敏钩子、保留期配置、prev_hash 占位 — M — 依赖 T2.6 — 验收:脱敏钩子对标记字段生效;批量写无丢失(对账单测);批量写契约 = `INSERT..ON DUPLICATE KEY UPDATE id=id` + affected-rows 与期望行数比对(禁 INSERT IGNORE——分区越界/截断会被静默降级,T1.3 审计确认)
- [ ] T6.2 OTel 贯穿(API → worker → LLM/工具)— M — 依赖 T6.1、T3.1、T3.2 — 验收:API 触发一次执行,audit_log 行 trace_id 与本次 trace 一致且可按其查询;审批挂起 → 续跑经 **trace link** 关联
- [ ] T6.3 审计查询 API(按 execution/trace/人过滤)— S — 依赖 T6.1 — 验收:三种过滤维度集成测试
- [ ] T6.4 Prometheus 指标 + 告警规则(死信、审批积压、卡死节点、429 率、队列 Redis 内存水线)— M — 依赖 T4.1、T3.1 — 验收:promtool test rules 通过;注入死信/审批挂起后指标计数断言

## M7 交付硬化(约 1 周)

- [ ] T7.1 配置体系(连接池上限、审计保留期、各类超时全配置化)+ 部署文档配额表 — M — 依赖 M6 — 验收:改配置 → 行为变化单测(池上限生效)
- [ ] T7.2 tiktoken 离线 loader + 词表随安装包 + 启动预构建 tokenizer — M — 依赖 T3.1 — 验收:断网环境冷启动成功,首个 LLM 节点不触发任何外网请求
- [ ] T7.3 安装自检(Redis 策略/AOF、词表、迁移版本)— M — 依赖 T7.2 — 验收:错误配置下拒绝启动并输出修复提示;含 audit_log 分区水位自检(INFORMATION_SCHEMA.PARTITIONS,不足拒绝启动)与业务账号对 audit_log 无 UPDATE/DELETE 权限断言(append-only 强制)
- [ ] T7.4 交付 compose 定稿 + 升级手册(golang-migrate + N-1 兼容窗口)— S — 依赖 T7.1 — 验收:对上一版本数据卷完成一次升级演练;含 dirty 中断恢复 runbook(migrate force → 修复 → re-up;迁移对象全 IF NOT EXISTS,重跑安全);含 outbox DEAD 重置规程:重置 PENDING 前必须先核验 Redis 无该 task_id 记录(idx_outbox_task 反查 + Inspector)——超时误报可造出「DEAD 行 + Redis 活任务」真相分裂(T2.2 审计移交)

## M8 验收(约 1 周)

- [ ] T8.1 CI 故障注入套件:worker 中途 kill / Redis 断连 / 审批 enqueue 失败 / 双投递 — L — 依赖 M2-M6 — 验收:全部注入下扫描间隔×3 内达终态,CI 绿
- [ ] T8.2 端到端示例:经 API 触发,文件路径输入 → file_read → LLM 解析 → 合规审查 → 摘要 → 人工审批(Web 页)→ 报告 — M — 依赖 T5.5、T3.5、T4.4 — 验收:全程审计链完整(trace 串联)
- [ ] T8.3 README(架构图、快速上手、故障注入演示)— M — 依赖 T8.1 — 验收:新环境按 README 30 分钟内跑通示例

## P1 停车场(不排期,防遗忘)

画布(React Flow 候选,DSL 已留元数据)、并行节点、回滚/断点续跑 UI、容器沙箱 + egress 默认拒绝代理、MCP 双向、审计哈希链、会签、OIDC/LDAP 插件、IM 审批卡片、工具包签名、Helm、SaaS 多租户、完整 taint 追踪、db_query 内置工具(只读账号/参数化/凭证作用域)。
