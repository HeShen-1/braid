# braid P1 交付总结(冻结记录)

> P1 = docs/ROADMAP-P1.md M9-M14,18/18 任务于 2026-09-30 全量交付。窗口:4ba9979(T9.1 合入,2026-09-28).. 7df564f(#115)。数据经 git/文件实证;执行队列镜像 = issues #80-#113(全闭环)。

## 数字面板

| 维度 | 数值 |
|---|---|
| 交付节奏 | 六波次(W1-W6)× ≤3 并行 lane,3 天全收(2026-09-28 → 09-30) |
| PR / commit | 19 个 PR(#83-#115 段)、80 commit(61 非 merge + 19 merge) |
| 代码量 | 277 files changed,+40,239 / −1,359 |
| 迁移 | 0007 rollback_resume / 0008 audit_hash_chain / 0009 users_email / 0010 login_throttle / 0011 tenant_model |
| CI | 4 → 6 作业(lint/test/gitleaks/promtool + helm(#105) + web(#111)),零 Secrets |
| 测试 | 29 个包,895 个顶层 Test 函数(不含 t.Run 子测试) |
| 决策治理 | P1 期 18 条 D- 条目(DECISIONS.md 累计 46 条) |
| 质量门禁 | 16-agent 设计对抗 ×1、6 视角对抗审计 ×9(9-70 agent 规模)、orch-review 三维全量;CRITICAL/HIGH 清零后合并,全程零事故 |

## 里程碑亮点(证据 = D- 条目 / 文件路径)

**M9 引擎纵深**
- Scatter-Gather 并行节点:单节点多实例行模型(1 控制行 + N 实例行),行集不变式与全行锁协议保留、worker 零改动;fan-out 上限 1000,gather 输出按输入序确定性物化(D-2026-09-28-t91,迁移 0006)
- 回滚/断点续跑同 execution 分支:ResetNodeToPending/RerunNode/RestartExecution 三原语 + attempt 单调续号;失败源闭包自动收编使终态可达成为结构性保证;守卫链五道(D-2026-09-28-t92,16-agent 设计对抗拍板,迁移 0007)
- 审计哈希链 fail-closed:逐行链「写入即算」+ chain_heads FOR UPDATE 序列化锚;链序 = prev_hash 链接序(非 id 序);断链 = 该执行响亮拒绝新 append;历史行事后篡改可检(D-2026-09-29-t93,迁移 0008)

**M10 工具沙箱(安全护城河)**
- 容器沙箱执行器:docker CLI 直 exec 零 go.mod;stdin JSON 信封(凭证不进 argv);SSRF 执法随行入容器防 DNS rebinding TOCTOU;零静默降级(D-2026-09-28-t101)
- egress 默认拒绝代理:--internal 网络 + 进程内 CONNECT-only 代理,「直连默认拒绝」由拓扑结构性保证;DNS 全代理侧解析 = rebinding 结构性消失;ALPN 钉死 http/1.1(D-2026-09-29-t102 + followup PR #98)
- 工具包签名 fail-closed:Ed25519 对清单原始字节、发布/加载信任主体分离、签名态下 unsigned 键响亮拒(D-2026-09-29-t103)
- db_query 三层只读纵深:词法拒绝注入载体 + transaction_read_only=1 + GRANT SELECT-only;超限报错绝不静默截断(D-2026-09-30-t104)

**M11 身份与多租户**
- OIDC 本地账号锚定:授权边界钉在 findByUsername 单点,零 DDL;PKCE S256 + state + nonce;半配置启动即败(D-2026-09-28-t111)
- SaaS 行级隔离:中间件注入 + store 层谓词单点;双租户对抗测试(语句→caller 反证口径)为验收硬条款;跨租户审计三维度裁剪 + tool.call.denied 字段级脱敏(D-2026-09-30-t112,迁移 0011)
- auth 纵深:token 绑定(UA-hash claim,失配 fail-closed)+ 登录防御 DB 锚定(login_throttle 行锁跨副本原子)(D-2026-09-29-t113,迁移 0010,PR #96/#99)

**M12 可观测与运维**
- OTel P1:W3C traceparent 外发注入、任务协议携真 span id(零迁移)、relay/reaper span 补齐、孤儿链口径消灭(D-2026-09-29-t121)
- 运维加固六项一项不落:keyset 分页(offset 携带即 400)、DB 池软上限、SMTP CA env 化、队列旋钮启动交叉校验、Redispatches 词表、按 approver 扇出投递(D-2026-09-29-t122,迁移 0009)

**M13 MCP 生态**
- server 面:协议单版本拒绝协商、stateless;scp 作用域 fail-closed + 自窄化签发(泄露窄 token 不能扩权不能续命)(D-2026-09-30-t131)
- 消费面:外部工具与内置同 Tool 接口同四道门,白名单/沙箱结构性不降级;静态声明拒动态发现;仅 streamable https;2^53 数值保真边界如实登记(D-2026-09-30-t132,19-agent 审计 0C/0H)

**M14 交互层**
- React Flow 画布:dist 入库 + 跨语言 fixtures 双消费(vitest 字节锚 + Go 发布同口径);构建确定性 sha256 实证(D-2026-09-30-t141,51-agent 审查)
- 会签:ApprovalOutcome 纯函数单点(any/all)+ SQL 投影单源常量;REJECTED 先于计数;uniqueItems 防死锁(D-2026-09-30-t142)
- 钉钉审批卡片:HMAC 签名短链(零存储,撤销双层);单租户部署契约硬闸(tenant≠0 拒发)(D-2026-09-30-t143)
- Helm chart:静态门验收(helm lint --strict + kubeconform + compose 键集对拍);hook 相位 CRITICAL 由四视角独立命中并修正,k3s 实证首装全 Running(D-2026-09-30-t144,70-agent 审计)

## 运维模式沉淀

- **并行公约**:engine 包单 worktree 持有、migrations 序号独占(同窗至多一个迁移任务在飞)、Redis DB 槽位人工分配(槽位×4+包基号)、merge 前 rebase + go build(语义互踩教训 #53×#54)、六波次零事故
- **对抗审计常态化**:设计期 16-agent 设计对抗、交付期 6 视角 + 双反驳验证;429 阵亡者主会话串行补验;CRITICAL/HIGH 清零才进合并
- **金向量独立性**:测试黄金值必须来自与被测实现不同源的权威样例(D-t143 dingSign 教训)
- **远端事件面已知问题**:Closes 关键字不自动关 issue(四连触发),合并后需 `gh issue close` 人工补闭环
