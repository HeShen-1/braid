# braid 路线图(P1)

> 任务定义与依赖真源 = 本文件;执行状态真源 = GitHub issues(认领/进行中/闭环,`gh issue list`)。勾选**仅作历史镜像**、不作为状态真源;勾选提交随任务 PR,merge 即生效,PR 描述带 `Closes #N` 自动关 issue(D-2026-09-22-git-native-collab)。新任务追加到对应里程碑末尾(标注发现日期);砍掉的任务划线注明原因,不删除。
> 规模:S = 半天内,M = 1-2 天,L = 3+ 天(全职当量)。约 18 任务:全职当量 8-10 周,断续投入约 3-4 个月。
> 里程碑依赖:M9 → (M10 ∥ M12)→ M11 → M13 → M14。任务级依赖单独标注,**冲突时以任务级为准**;P1 停车场见文末。
> P0(38 任务,2026-09-27 全量交付)的冻结交付记录与全部交付注记见 **docs/ROADMAP-P0.md**。

## M9 引擎纵深(并行/回滚/防篡改)

- [ ] T9.1 并行节点(Scatter-Gather)— L — 验收:DSL 新增 parallel 节点(foreach 集合展开 fan-out + gather 汇聚);调度器入度推进复用,并发执行断言(多 attempt 同时 RUNNING);部分失败语义显式建模(fail-fast 收敛 vs 继续收齐再判,gather 处钉死一种并文档化);表驱动 + 真 asynq e2e(≥3 路并行恰一次汇合)全绿
- [ ] T9.2 回滚 / 断点续跑 — L — 验收:回到指定节点以 input_snapshot 重跑(ARCHITECTURE 底层假设:回滚 = 节点重跑非补偿);下游节点按拓扑重派;新 attempt 不覆盖旧终态(审计可追溯两条路径);续跑(进程崩溃后 execution 恢复推进)与回滚同基座;语义拍板先行(D- 条目:同 execution 分支 vs 派生 execution);e2e:回滚→重跑→终态 + 审计链完整
- [ ] T9.3 审计哈希链 — M — 依赖 T9.2(先钉回滚不改写历史的语义)— 验收:append 时 prev_hash = H(prev_hash ‖ 本行关键列),启动自检 + 定期校验断链(断链 = 响亮拒绝新 append);reaper 搬运路径同链;性能:逐行链 vs 批内链的权衡拍板(D- 条目);篡改检测测试(改一行 → 校验红)

## M10 工具沙箱(护城河:安全审查级)

- [ ] T10.1 容器沙箱执行器 — L — 验收:ToolRunner 容器级落地(每工具调用隔离容器:只读根 fs、资源上限、无特权);file_read/http 迁入沙箱路径跑通;降级开关(env)保 dev 栈可用;e2e:沙箱内路径逃逸/资源超限被拒
- [ ] T10.2 egress 默认拒绝代理 — M — 依赖 T10.1 — 验收:沙箱出网仅经本地代理;DNS/IP 白名单策略面(env);默认拒绝下 http 工具经代理可达白名宿主;非白名解析/直连被拒的 e2e
- [ ] T10.3 工具包签名 — M — 验收:工具定义发布带签名;加载侧验签失败即拒;密钥管理走 env(不进代码/日志)
- [ ] T10.4 db_query 内置工具 — M — 依赖 T10.2 — 验收:只读账号(连接串 env 注入)、参数化强制(非参数化拒)、结果集上限;凭证作用域 = 仅该工具

## M11 身份与多租户

- [ ] T11.1 OIDC 插件 — M — 验收:IdentityProvider 缥兑现(redirect 流,回调路由面新立);错误族复用 T5.3 契约(CredentialsError→401 / ErrUserDisabled→403 / 其余 500);LDAP bind 留桩;配置半开启动即败
- [ ] T11.2 SaaS 多租户 — L — 依赖 T11.1、M9 — 验收:tenant 模型 + 行级隔离(隔离机制拍板 D- 条目:中间件注入 vs MySQL 层);审计读面收紧 + tool.call.denied 账号态字段暴露重议(D-t63 #5,**外部读者出现前必须重议**——本任务即触发);跨租户访问被拒的对抗测试(语句→caller 反证口径)
- [ ] T11.3 auth 纵深批 — M — 验收:token 绑定(D-t44 §7-⑦)、登录防御 DB 锚定(D-t64:多副本部署升级)、username 定向锁定 DoS 纵深评估(结论入 D-);三项一 PR,各自独立验收

## M12 可观测与运维(M9 后可与 M10 并行)

- [ ] T12.1 OTel P1 — M — 验收:Jaeger exporter(env 切换,off/stdout 保留);W3C traceparent 外发注入(llm/http 出站带);relay/reaper 进程 span 补齐(现两进程零观测);任务协议携真 span id(payload 可伪造的内部信任边界按 D-t62 修订注收口);notify 消费侧 middleware 短路(孤儿链口径)
- [ ] T12.2 运维加固批 — M — 验收六项:审计查询 keyset 分页(D-t63 #5)、BRAID_DB_MAX_OPEN 软上限护栏(#67 系)、BRAID_SMTP_CA_FILE env 化(D-t45 §7 蒸发回收)、队列旋钮启动交叉校验(D-t43 §7 蒸发回收)、Redispatches{attempt_failed} 词表发射(D-t64 留位)、按 approver 扇出投递(D-t45 #2:用户资料 email 列 + 逐人收件);一项不落逐项验收

## M13 生态(MCP)

- [ ] T13.1 MCP server 面 — L — 验收:平台工作流暴露为 MCP 工具(外部 agent 可发现/触发/查询);鉴权走 token 作用域;协议版本钉死
- [ ] T13.2 MCP 消费面 — M — 依赖 T13.1 — 验收:外部 MCP 工具接入 tools.Registry(与内置工具同权限模型);白名单/沙箱约束不降级

## M14 交互层

- [ ] T14.1 画布(React Flow)— L — 验收:DSL 元数据(节点位置)驱动的可视化编辑;画布 → DSL 导出过发布校验;并行/分支/审批节点全类型支持;纯前端静态资源进 /ui/(零后端新端点优先)
- [ ] T14.2 会签 — M — 验收:approval mode=all(全部批准才过,任一驳回即败);部分批准的中间态可视化;超时语义与或签一致(escalate)
- [ ] T14.3 IM 审批卡片 — M — 依赖 T14.2 — 验收:至少一个 IM 平台(钉钉/飞书 webhook 形态);卡片动作回平台 API(鉴权短链);与 SMTP 升级通知共存
- [ ] T14.4 Helm chart — M — 验收:values 覆盖镜像/密钥/副本;preflight 与 init 作业模板化;与 prod compose 交付面等价

## P1 停车场(不排期,防遗忘)

完整 taint 追踪、PII 脱敏放开评估(D-t61 #5:当前词表无自由文本 ⇒ P0 净效果零掩蔽,放开自由文本 payload 前评估)、metrics exposition 无认证(T7.1 评审登记:api /metrics 与组件 BRAID_METRICS_ADDR 同模式,仅文档契约「绑回环/留内网」代码不强制;内网部署可接受,公开面部署需反代封禁)。
