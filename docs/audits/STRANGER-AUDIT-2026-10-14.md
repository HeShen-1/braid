# braid 对抗审计·主编终报告(2026-10-14)

## 1) 一句话总结 + 四桶计数

**一句话总结**：对抗核证后 56 条发现存活(0 CRITICAL / 5 HIGH / 28 MEDIUM / 23 LOW),核心引擎与 API 契约面无安全/数据正确性问题；问题集中在控制台呈现层(成功反馈、可观测性、状态诚实、错误可懂性)与 prod 交付文档的两处断链——陌生人旅程 dev 路径带 5 处硬摩擦勉强可通，prod 路径断头于「无账号可登录」。

| 桶 | CRITICAL | HIGH | MEDIUM | LOW | 合计 |
|---|---|---|---|---|---|
| design(前端设计) | 0 | 3 | 19 | 15 | **37** |
| backend(后端功能) | 0 | 0 | 1 | 3 | **4** |
| integration(前后端联调) | 0 | 1 | 6 | 3 | **10** |
| docs(文档) | 0 | 1 | 2 | 2 | **5** |
| **合计** | **0** | **5** | **28** | **23** | **56** |

另：被杀 15 条(见第 4 节)，原始 71 条 → 存活 56 条。

## 2) 存活发现分级明细

### CRITICAL(0 条)
无。

### HIGH(5 条)

- **F02 · prod 栈零登录账号，首个用户建号方法未写进任何 prod 侧文档** `[docs|HIGH]`
  repro:prod compose 零 seed 服务，`grep -rnE 'user create|建号|种子账号' deploy/*.md deploy/prod-up.sh` 零命中；迁移零 INSERT;唯一书面方法在 §2.2 ④ dev 叙事，prod 照抄必失败。
  修法：deploy 侧补「首个登录账号」一节(compose 网内以 root DSN 跑 `braid user create` + 角色与口令纪律)，README §2.1 与 prod-up.sh 就绪横幅直接指路。
- **F03 · 定义列表分页被 UI 忽略：超 20 条草稿/已发布工作流在界面静默消失** `[integration|HIGH]`
  repro:发布 21 条后侧栏只显 20 条且计数显示 20;第 21 个已发布工作流不进触发下拉，无触发路径(执行面正确消费同一契约，唯定义面两处漏接)。
  修法：listDrafts/listPublished 两处消费 page envelope(total),接入执行面已有的翻页器模式。
- **F04 · 发布成功后反馈不可见：flash 渲染在已卸载的编辑器顶栏 + DRAFT-only 列表，发布像「工作流凭空消失」** `[design|HIGH]`
  repro:点「发布」→ 零反馈，左列条目消失、刷新仍空、按钮仍可点；切执行面看「N 个已发布」才确认成功。
  修法：成功反馈与编辑器卸载解耦(顶层 toast 或列表态成功条)，并给发布结果一个「查看已发布」出口。
- **F05 · 保存草稿校验失败把 oneOf 全臂原始转储整屏抛出，且错误横幅常驻不清除** `[design|HIGH]`
  repro:新建空 LLM 节点点「保存草稿」→ 底部 25+ 行 schema 转储，唯一有用事实(minLength)埋在噪音中；填好模板后旧错误仍驻留至下次保存。
  修法：后端 asInvalidDSL 把 jsonschema 各 cause 按 InstanceLocation 拆成结构化 issues(IssuesPanel 已支持渲染)，编辑期间旧错误清除/降权。
- **F10 · 回滚/续跑零确认层：单击节点行+按钮立即改写执行状态** `[design|HIGH]`
  repro:单击时间带节点行武装目标 → 单击「回滚到 llm」→ 无确认立即生效(19ms 后审计落 execution.rolled_back);续跑同样单击即执行；同条「取消执行」却有确认层。
  修法：回滚接入 ui/confirm.tsx(与同面「取消执行」对齐)；续跑属高频操作可维持直按或一次布防，不必硬套同档确认。

### MEDIUM(28 条)

- **F01 · §2.1 prod 路径断链①:反代指引被指向不含该内容的 RUNBOOK** `[docs|MEDIUM]`
  repro:`grep -nE '反代|proxy|8081|listen' deploy/RUNBOOK.md` 仅 1 处无关命中；SPA 转发清单埋在 deploy/README.md §6「指标 exposition 端口」节内。
  修法:prod-up.sh 两处指路改锚 .env.example 反代注记 + deploy/README §6/§11,或 RUNBOOK 增补一节反代接入。
- **F08 · 待办队列 j/k 键盘光标视觉不可见** `[design|MEDIUM]`
  repro:j 前后仅 data-cursor 布尔翻转，计算样式对比度 1.08:1(自家 DESIGN.md 要求非文本图形 ≥3:1),肉眼零差异；activeElement 恒 BODY。
  修法：光标行指示提到 ≥3:1 线(左边框或不透明底)，补 aria-current/焦点暴露。
- **F12 · 「deploy/.env.example = env 旋钮单源」承诺失真：seed/CLI 凭证类键不在单源内** `[docs|MEDIUM]`
  repro:`grep -nE 'BRAID_SEED_|BRAID_CLI_PASSWORD|…' deploy/.env.example` 零命中，而 README 132/482 两处宣称「全部 env 旋钮单源」。
  修法：BRAID_SEED_* 补入 .env.example,或将两处「全部…单源」措辞收窄为「四进程与交付栈配置 env」。
- **F13 · 审计链 UI 静默截断在 100 条：next_cursor 从不消费，面板计数误导** `[integration|MEDIUM]`
  repro:listAuditEvents 固定 limit=100 丢弃 next_cursor,面板把 audit.length 当总数显示「100 条」且无加载更多。
  修法：消费 next_cursor 加「加载更多」，计数改读 total。
- **F14 · 登录响应 username/roles/expires_at 三字段被 UI 丢弃，无角色感知界面** `[integration|MEDIUM]`
  repro:approver-only 登录后写操作按钮照常可点，每次点击收 403;LoginResponse 只声明 token/token_type/expires_at。
  修法：补声明并消费三字段——按角色隐藏/禁用写操作入口，临期提示。
- **F16 · 节点级证据(投影/轮次台账三端点)零消费：重试轮次 input/output/usage 不可见** `[design|MEDIUM]`
  repro:失败 llm 节点单击时间带行只武装回滚目标，全页无 attempt 详情入口；三只读端点零调用。
  修法：节点行点击加「轮次台账」展开，消费既有 /nodes/:id、/attempts 端点，零新端点。
- **F17 · 审计 payload 与 LLM 用量/成本字段全被 UI 丢弃** `[integration|MEDIUM]`
  repro:审计表只有五列，API 响应里 model/tokens/cost 与 payload 均有值但未渲染。
  修法：审计行加 usage/cost 列与 payload 悬停/展开。
- **F18 · 触发的输入正文后端有、UI 永不显示：失败复盘看不到喂进工作流的输入** `[design|MEDIUM]`
  repro:执行详情全页找不到触发输入；API detail DTO 的 input 有值，definition_hash 同样零消费。
  修法：详情头部加 input KV 一行(definition_hash 对账入口可顺带)。
- **F19 · trace 世系与链 id 全部不可下钻：TRACE_ID/「链」hex 是死文本** `[design|MEDIUM]`
  repro:手工 `curl /api/v1/audit?trace_id=<值>` 可查而 UI 无入口；attempt 递进在数据层已发生、UI 零呈现。
  修法：trace_id/链 hex 加一键反查(带出 trace 维度审计查询)，attempt 展示消费既有数据。
- **F20 · 已发布工作流在工作流面不可见且定义不可检查：版本历史/归档三端点零 UI** `[integration|MEDIUM]`
  repro:/workflows 显示「0 个工作流」而执行面「N 个已发布」，计数互相矛盾；versions/archive 只能 curl,结果无处看。
  修法：工作流面加「已发布/版本历史」视图(消费 GET /definitions/:name/versions),版本行挂 archive 入口。
- **F21 · 执行列表「按名称」筛选是精确等值，UI 呈现为自由文本搜索，部分名查询得误导性空结果** `[integration|MEDIUM]`
  repro:输入 `audit`(数据存在多条 audit-*)→「0 条」；后端是 `definition_name = ?` 等值谓词。
  修法：两侧对齐一端——后端改 LIKE 前缀(可吃索引)或 UI 明示「须完整名称」/改下拉。
- **F22 · 工作流列表加载失败被静默吞成「暂无工作流」空态，违反本仓自立诚实空态原则，且无重试路径** `[design|MEDIUM]`
  repro:首载断网 → 左列「暂无工作流，右侧新建。」假空态；对照 ApprovalQueue 有三态契约且有测试钉死。
  修法：catch 不清列表，DraftList 增错误态+重试(对齐 ApprovalQueue)。
- **F23 · 执行表无加载态：首帧/翻页/换筛选期间渲染「暂无执行」空态** `[design|MEDIUM]`
  repro:弱网首帧先见「暂无执行 · 0 条」再突然弹数据；同仓 ApprovalQueue/WorkflowsRoute/ExecutionDetail 均有加载态。
  修法：补 loading(Skeleton 或 aria-busy+文案)，对齐既有三处实现。
- **F24 · 执行详情读失败永久停留「加载执行 #id…」:非 401 错误不上错误面也不停轮询** `[design|MEDIUM]`
  repro:深链 /executions/999999999 → 右栏无限加载文案，2.5s 轮询不退场；对照 ApprovalDetail 有完整错误面。
  修法：非 401 读失败渲染错误 Alert + 返回指引并停止轮询(对齐 ApprovalDetail)。
- **F25 · 审批面键盘焦点缺口：决定成功/Esc 后焦点落 body,j/k 光标不滚动进可视区** `[design|MEDIUM]`
  repro:键盘流 j→Enter→a 成功 → 焦点掉 body,Tab 序从头；20 条队列按 j 到第 15 条视口不跟随(scrollIntoView 全仓零命中)。
  修法：回队后焦点回归选中/首行，j/k 补 scrollIntoView。
- **F26 · 画布脏态不可见且可被静默丢弃：「新建」/切换草稿/刷新均无确认** `[design|MEDIUM]`
  repro:编辑未保存 → 点「新建」→ 编辑无痕清零；顶栏无脏态标记，无 beforeunload(核证另发现 URL effect 第三条丢失路径)。
  修法：顶栏加脏态指示；三条路径在 dirty 时加确认或自动暂存。
- **F27 · DESIGN.md 承诺的页头未兑现：RouteHeader 全仓零调用，整个应用没有 h1** `[design|MEDIUM]`
  repro:`grep -rn RouteHeader web/canvas/src` 仅定义处命中；读屏标题导航从 h2 起步。
  修法：兑现页头(接入 RouteHeader)或从 DESIGN.md 删除该承诺；至少给每路由一个 h1。
- **F28 · 错误词表双轨：登录/审批面译中文句，执行面与定义图直接上屏裸码** `[design|MEDIUM]`
  repro:同一 role_denied 在审批面是「缺少审批角色(approver/admin)。」,在执行面是裸「操作未完成 role_denied」。
  修法：抽共享错误词表(扩审批面十码表)供执行面/定义图复用，未知码退回裸码。
- **F29 · 顶栏计数与列表不自动同步：审批队列无自动刷新通道，与壳层脉冲互相矛盾** `[design|MEDIUM]`
  repro:开 /approvals 另端触发带审批执行 → 顶栏几秒内变「1 待审批」而队列仍「0 条」；决定后 chips 滞旧 ≤20s。
  修法：审批队列加短轮询通道，决定/触发后主动刷新 chips(事件后即刷而非等 20s tick)。
- **F30 · known-issue:#210 — TestAPIFenceAllWriteFaces 同毫秒种子撞 PK,CI 随机红** `[backend|MEDIUM]`
  repro:六次 seedApprovalFenceChain 各新建 snowflake.New(21),同毫秒首号相撞(workflow_definition PK 1062),CI 随机红。
  修法：种子显式错峰(固定节点号区分或钳制时间/seq),同族 per-call New() 一并扫。
- **F34 · 工作流设置面板打开时点击画布节点不切换右侧面板** `[design|MEDIUM]`
  repro:开设置 → 点节点 → 右栏不动(RF 内部选中集无变化致回调不触发)，节点还带误导性选中环。
  修法：toggleWorkflow 同时清 React Flow 选中态，或面板切换改由节点点击驱动。
- **F36 · 新建节点全部落在画布同一坐标互相叠盖，加 4 个只见 1 个** `[design|MEDIUM]`
  repro:依次点 4 个工具条按钮，4 张节点卡 rect 完全重合；保存/加载后堆叠持续。
  修法：addNode 落点加网格步进/占位偏移，并自动选中新节点(与 F64 同修)。
- **F38 · 分支节点画布连线与 DSL 的 next/default 字段脱节，须手填「目标节点 id」** `[integration|MEDIUM]`
  repro:case_0 拖线成功后 edges 有记录而 cases[0].next 仍空串；节点 id 属性面板无处查询，填错撞 schema 转储。
  修法：连线手势按 anchor 语义回填 next/default,属性面板目标 id 改下拉选择。
- **F39 · LLM 失败报错是开发者向三段链式码，且与自身「默认模型」配置展示自相矛盾** `[design|MEDIUM]`
  repro:横幅「node llm failed: llm_call_failed: llm: request needs model and messages」,同页定义图显示「默认模型」已配置——而后端不存在默认模型机制。
  修法：错误分层映射成人话(缺模型/连不上分诊)，对齐或删除「默认模型」措辞，保存/发布补 model 缺失提示。
- **F40 · 审批决定内容(谁批/驳了什么、备注全文)事后在 UI 无处回看** `[design|MEDIUM]`
  repro:决定落库后队列清空、无历史视图，驳回备注在执行详情/审计列表/错误文案三处均不出现；唯一入口是无提示深链。
  修法：审批面加已决定历史入口(或已决执行的审批面板保留决定时间线)，链到 /approvals/:execId/:nodeId。
- **F41 · 人工驳回被渲染成红色「执行错误 node_failed」系统异常样式** `[design|MEDIUM]`
  repro:被驳回执行头部红色横幅与真故障同一视觉语言，顶栏「7 失败」计数同样把驳回混入。
  修法:approval_rejected 单独呈现(「审批驳回」中性样式/文案)，不套系统错误横幅。
- **F43 · 触发表单无输入契约提示：首触发即失败，错误用 DSL 术语说话** `[design|MEDIUM]`
  repro:选 audit-j2-demo 输 {} 触发必失败，横幅「input_eval … ref {{inputs.text}} unresolved」全内部词汇；面板不显示所需字段。
  修法：触发面板消费定义 inputs 契约展示所需字段，报错映射为人话(缺参数 X)。
- **F45 · 回滚后「时长/运行」全部变成墙钟年龄，真实运行时长无处可见** `[design|MEDIUM]`
  repro:实际 1.5s 的执行回滚重跑后显示 1h37m(含 ~96 分钟人工闲置)，时间带红条横贯整轴。
  修法：消费既有 attempts 端点展示「本轮时长」，或标签明示「跨轮回滚全程」。

### LOW(23 条，打磨池)

- **F15 · OIDC 登录零 UI 入口且无 SPA 落地面** `[integration|LOW]`
  repro:/login 无 SSO 入口；oidc callback 返回 JSON token 而非落回画布。修法：补 D- 登记/停车场上架「企业 SSO Web 集成无主」(缺口在 D-t111 断链的延期指针)，不作为画布缺陷修。
- **F32 · 提示词模板引用语法与数据接线概念零解释** `[design|LOW]`
  repro:{{inputs.text}} 试错被拒('not' failed),合法语法只在输入框 placeholder 与 schema 报错文本里。修法：模板框旁补一行合法引用示例与 user_data 运行时行为说明。
- **F37 · minimap 叠在节点上方拦截点击(配色半边已被核证为误报)** `[design|LOW]`
  repro:DSL 预览展开后 elementsFromPoint 首命中 minimap svg,节点点不动。修法：画布过短时隐藏/可折叠 minimap。
- **F42 · 审批详情缺关键决定上下文：无触发人/已等时长/输入/执行面链接** `[design|LOW]`
  repro:alice 详情页整页文本无触发人、无已等时长、无外链。修法：加一条执行详情页内链(零后端成本)；已等时长随 #202 增项落地。
- **F44 · 处置面板黑话：「静默在飞/楔停/409/重置 N 行」无平白解释** `[design|LOW]`
  repro:hint 全协议术语；续跑横幅复用「已回滚到」模板误标动作。修法：hint 增平白版一句话，续跑横幅改「已续跑」措辞。
- **F47 · migrate CLI 版本两处口径不一；shot.mjs 复现命令缺前置说明** `[docs|LOW]`
  repro:README 教装 v4.20.1,prod compose 钉 v4.18.2;shot.mjs 需 dev 栈在跑但 README 未提。修法：注明差异缘由(或统一)，§0 补两前置一句。
- **F48 · 内部任务/决策编号高频出现且无编号真源入口** `[docs|LOW]`
  repro:README 13 处 T-xx/D-xx,无「真源在哪」指路句。修法：文档地图加一句「T- 号见 docs/ROADMAP*.md、D- 号见 docs/DECISIONS.md,可全文检索」。
- **F49 · 回滚/取消错误码契约文档漂移：api.ts 注释词与线协议码不一致** `[integration|LOW]`
  repro:线码 rollback_blocked_inflight vs 注释 blocked_inflight;cancel/resume 族注释缺席。修法：注释改线协议全名并补族。
- **F50 · 登录错误码消化表含三个后端永不发送的码：account_locked 是死代码** `[integration|LOW]`
  repro:后端登录面只发 invalid_credentials/user_disabled/too_many_attempts 等；account_locked 全仓 Go 历史零存在。修法：删三分支死代码。
- **F53 · 时间带图例与状态芯片词表漂移：「成功」vs「已成功」** `[design|LOW]`
  repro:同屏图例「成功/失败/取消」vs 芯片「已成功/已失败/已取消」。修法：图例改读 EXEC_STATUS[state].label 单源。
- **F54 · 触发执行无 busy 文案；defs 为空时双提示叠加** `[design|LOW]`
  repro:busy 只 disabled 不变字；空 defs 时两条灰字提示同屏且其一不可能执行。修法：busy 换文案，空 defs 只渲染正确一条。
- **F55 · 运行脉冲降级方式是整组消失(布局跳动)，401 时冻结陈旧计数** `[design|LOW]`
  repro:瞬时 5xx → 顶栏三计数闪失；过期后在无轮询页可无限期冻结。修法：失败保留上次值+降级提示，401 补登出出口。
- **F56 · 执行详情会签面板只读且无「去审批面处理」引导** `[design|LOW]`
  repro:面板无任何动作/链接，深链 /approvals/:execId/:nodeId 已在但执行面零生成。修法：面板加一条该深链。
- **F57 · 执行表/审批队列行选择语义弱：tr 无 role 与选中态播报** `[design|LOW]`
  repro:可点行是 tabIndex=0 的 tr 无 role;同产品时间带是真 button+aria-pressed。修法：行补 role/aria-selected(或改真按钮)。
- **F60 · README §1.2 写「OIDC/LDAP 留接口桩(P1)」,实为完整实现** `[backend|LOW]`
  repro:internal/enterprise/oidc+ldap 全量实现与测试在库。修法：该行改现状口径，顺带补 persona→角色映射一句。
- **F61 · README/PRODUCT 能力地图未覆盖已交付面：MCP 双向、IM 短链、哈希链校验** `[backend|LOW]`
  repro:三面均已交付且 README 全文 grep 零命中。修法：README 能力地图补一行，「超时升级通知」补 SMTP+钉钉双通道表述。
- **F63 · 术语毛刺残项：「未建档」应为「未保存」；空态「执行面」与导航「执行」不同名** `[design|LOW]`
  repro:两处内部语言外露。修法：改「未保存」「执行页」。
- **F64 · 新建工作流后节点不自动选中；触发参数行号易误读** `[design|LOW]`
  repro:加节点后右栏仍「未选中节点」。修法：addNode 自动选中(随 F36);序号 aria-hidden。
- **F65 · 工具条「审批」与顶部导航「审批」同名，误点即离开编辑器** `[design|LOW]`
  repro:两控件可访问名同为「审批」。修法：工具条标签改「审批节点」消歧。
- **F66 · 执行级失败在审计事件链中无对应事件** `[backend|LOW]`
  repro:FAILED 执行审计仅 4 条节点事件，无 execution 终态事件；词表从未被正面裁决。修法：先在 DECISIONS.md 追加 D- 条目裁决，再决定是否补 execution 终态事件。
- **F68 · 审计时间渲染「7m60s 前」:秒未向分钟进位** `[design|LOW]`
  repro:fmtDuration 余数未取模，每分钟边界 500ms 坏窗。修法：rs 改 floor/取模进位，补单测。
- **F69 · 下拉占位文案渲染为第一个可选项** `[design|LOW]`
  repro:[role=option] 首位即「选择已发布工作流…」且带勾选。修法：占位 option 补 disabled(外壳已支持透传)。
- **F71 · 审计事件裸暴露内部事件名与 hex 链段，无词汇表** `[design|LOW]`
  repro:node.taint_propagated 无解释，「链」hex 与头部 TRACE_ID 关系无处可查。修法：展示层加事件标签/词汇表与链关系说明(不触审计词表契约)。

## 3) 建议修复波次

**第一波 = P4 残件 + 审计 CRITICAL/HIGH(整体规模：L,约 8-11 人日，建议拆 4-5 个 PR;几乎纯前端+docs,engine 包零触及，可并行 worktree)**

| 项 | 内容 | 规模 |
|---|---|---|
| #202 残件 | ⑤时间带对齐 + ⑥审批两级视图 + 增项「已等待多久」(requested_at,F58 核证实际剩三项非两项) | L |
| #207 | Tailwind 取材面收口(登记口径：source(none)+显式 @source) | S |
| #210 | fence 测试种子错峰 + 同族 per-call New() 扫尾 | S |
| #213 | 非开发项：修复已落 master(39ef08d),master 复跑全绿后直接闭单 | S |
| F02 | prod 首账号文档缝 | S |
| F03 | 定义列表分页接入 | M |
| F04 | 发布成功反馈重构 | M |
| F05 | schema 错误结构化(后端拆 cause + 前端清时序) | M |
| F10 | 回滚确认层 | S |

**第二波 = MEDIUM 28 条(整体规模：L+,约 19-25 人日；建议按主题拆 3 个子波)**

1. 子波 A·可观测性/契约消费(执行详情证据面)：F13+F17+F18+F19+F16+F45+F29+F14 ≈ M×5+S×3 —— 全部消费既有 DTO/端点，零后端新端点。
2. 子波 B·错误与状态诚实：F05 之外的 F28+F39+F41+F43+F22+F23+F24+F21 ≈ M×2+S×6。
3. 子波 C·编辑器/审批交互 + 文档 + 测试：F26+F34+F36(+F64 顺带)+F08+F25+F38+F40+F20+F01+F12+F27+F30 ≈ M×4+S×7。

**LOW 23 条 = 打磨池**：不排波；多数可与相邻 MEDIUM 同文件顺带修(F64 随 F36、F42 一半随 #202、F63/F53/F69 纯文案一行)，其余按你的体验反馈抽取。

## 4) 被杀名单(15 条)

- F06 会话过期静默踢回登录页丢草稿 — 死因：TTL 硬编码 24h 使「40-60 分钟过期」不可能；踢出本身是钦定契约(401 收敛登出)+ D-t141 已知边界②登记在案。
- F07 a/r 快捷键无确认层与鼠标双标 — 死因：r 实走与鼠标同一确认层，批准单击直达是 T21.2 明示设计，键盘/鼠标双标不成立。
- F09 下拉弹层整层 inert 点不动 — 死因：审计脚本 getByText 定位器歧义命中表格行，Playwright 误报拦截；CDP 真实点击 2/2 成功，Base UI 结构正常。
- F11 input_eval 执行是处置死胡同 — 死因：续跑自动定位、rolled_back 词表、重置 0 行全是 D-t92 决策 1/5/9 锁定设计；输入不可变则重跑必再败是因果必然。
- F31 #213 ApprovalsView 偶发红 — 死因：修复已落 master(39ef08d),issue 被刻意保留为 flake 家族跟踪单，属簿记状态非缺陷。
- F33 画布窄视口挤压不可读 — 死因：「不做移动端、窄窗只保证不横向溢出」是 D-2026-10-09-ui-console/v2 明示裁剪；竖排工具条等头条症状实测不成立。
- F35 下拉触发 CSP console error — 死因：三重已知登记(field.tsx 注释 + .handoff×2)+ CSP/Base UI 均为锁定决策，实测每页一次且零可见影响。
- F46 处置语义零文档救场无门 — 死因：契约已以内联 hint/确认层/flash 回显在决策点交付，D-t92+DESIGN.md+ARCHITECTURE 即契约页；「救场无门」漏看了勾选框下方常显 hint。
- F51 scoped token 签发面零 UI — 死因:D-t131 决策 4 明文定位为外部 agent 运维面，从未承诺给画布前端。
- F52 IM 短链/MCP/healthz 未入画布 — 死因：各端点均有锁定决策(D-t143/D-t175/D-t131/ui-console 决策 6)裁定独立通道或合理深藏。
- F58 known-issue:#202 残件 — 死因：在册 OPEN 任务+形态决策已拍板，属已知登记非审计新发现(且实际剩三项非两项)。
- F59 known-issue:#207 Tailwind 取材面 — 死因：已立项 #207,带真机复现、修复口径与验收标准。
- F62 空态「右侧新建」与按钮位置不符 — 死因：自引截图反证自己——主区空态卡的「新建工作流」按钮恰在文案所指的右方，误报。
- F67 chips 缺「待执行」档且取消不可见 — 死因：三计数「运行脉冲」是 DESIGN.md 明文规格(在飞/需介入心跳)；取消成功在详情有 flash+芯片+列表刷新三重即时反馈。
- F70 input_eval 在时间带无失败标记 — 死因：节点留 PENDING 是引擎明示设计(失败从未 claim,画红才是编造数据)，登记在案且横幅就在时间带正上方。

## 5) 「陌生人 60 分钟可用性」结论

**不达标，共 7 个缺口；其中 dev 路径 5 处硬摩擦(旅程能走通但各耗 5-15 分钟)，prod 路径 2 处硬断头(60 分钟内无法完成)。**

- 缺口 1(prod,断头)：栈起成功 ≠ 能登录——零账号、首个用户建号方法零文档(F02)。
- 缺口 2(prod,断头)：控制台无入口指引，被指的 RUNBOOK 无答案(F01)。
- 缺口 3(dev):新建工作流首存即遇整屏 schema 转储，J1 实证新人真实卡三轮才悟出须填提示词(F05)。
- 缺口 4(dev):连加 4 个节点完全叠盖，只见 1 个，误以为没加上(F36)。
- 缺口 5(dev):提示词引用语法零解释，{{inputs.text}} 试错进死胡同，只能从报错反推(F32)。
- 缺口 6(dev):首触发无输入契约提示，{} 必失败且报错是内部术语(F43)。
- 缺口 7(dev):发布成功零反馈、草稿从列表「凭空消失」，需切执行面跨页对数才能确认(F04)。

说明：dev 路径(§2.2 dev-up 叙事)在种子账号在场时旅程链路完整可走通(J1/J2 旅程实证)，判定为「带伤可达」；prod 路径(§2.1 一键部署)终点是一个无人能登录且无入口指引的系统，判定为「断头」。第一波修完 F02/F04/F05 后 dev 旅程的主要卡点即清，F01/F10 随后补齐 prod 闭环。

## 6) 附录：已拍板待办(非审计发现，不计入上述任何计数)

- LICENSE = Apache-2.0(已拍板；当前仓公开但无许可证，法律上无人可合法使用)
- 英文 README 前置 + 中文版并存(已拍板全球定位)
- CONTRIBUTING.md + SECURITY.md + issue 模板 + CI badge
- 首个 release tag(v1.0.0,时机随用户体验反馈后定)