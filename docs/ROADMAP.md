# braid 路线图(P4 进行中:M21 重建 + M22 审计修复第一波)

> 任务定义与依赖真源 = 本文件;执行状态真源 = GitHub issues(认领/进行中/闭环,`gh issue list`)。勾选仅作历史镜像;勾选提交随任务 PR、merge 即生效,PR 描述带 `Closes #N`(远端 Closes 偶发不触发,合并后 `gh issue close` 人工补)。
> 规模:S = 半天内,M = 1-2 天,L = 3+ 天(全职当量)。
> 里程碑依赖:M18(清账)∥ M20(收口)先行,M19(引擎纵深)随后串行(engine 包单 worktree 公约);任务级依赖单独标注,冲突时以任务级为准。
> 方向拍板(2026-10-05 用户):**三轨全开**(P3-1 清账波 + P3-2 引擎纵深主轴 + P3-3 可靠性收口);**SSRF 扩禁 = 全五段**;多租户重契约族/动态 DAG 评估/自由文本面**维持触发器休眠**。46 条线索扫描与处置全录见会话档;P3 各任务的语义级拍板(min_success 三件/fencing 立项/scp 审计 D-/回滚族②③④⑤裁剪)在其 GATE 1 逐项裁。
> P0 冻结记录 = docs/ROADMAP-P0.md;P1 = docs/ROADMAP-P1.md + docs/P1-DELIVERY.md;P2 = docs/ROADMAP-P2.md + docs/P2-DELIVERY.md;P3 = 本文件 + docs/P3-DELIVERY.md(**2026-10-07 交付时 21/21;收官后 followup = T20.9(2026-10-10),不重算 P3 计数**);**P4 = 本文件 M21 节(2026-10-09 立项,控制台重建波)+ M22 节(2026-10-14 立项,审计修复第一波)**。

## M21 控制台重建波(P4-1,UI 重做 + 路由正名)

> 方向拍板(2026-10-09 用户两轮问答,D-2026-10-09-ui-console):**统一单 SPA**(部分推翻 D-t44「审批页零构建链」的构建链约定,保留其安全姿态条款);**深色仪器盘**单一世界(用户拍板);Tailwind v4 + Lucide + react-router 8,全部 exact pin;自托管 Geist/Geist Mono(OFL,入库);**路由硬切**:旧审批台/画布前缀 → `/`(→ `/workflows/`)、`/login`、`/workflows/*`、`/executions/*`、`/approvals/*` 与静态面 `/assets/*`,旧路径 404 不留重定向;桌面优先,不做移动端;零后端新端点。
> signature move = **执行时间带**:审批挂起段斜纹填充,把「挂起不占 worker」画成有物理长度的东西。

- [x] T21.1 设计系统基座 + 画布面 UI 重做 — L — issue #196 — **已交付**(统一单 SPA 基座:Tailwind v4 CSS-first token 层 + 自托管 Geist/Geist Mono(OFL 入库)+ 组件原语 + 应用壳 + react-router 8(路由路径即最终形态);画布/节点/属性面板/执行面全量重做,含签名动作**执行时间带**(审批挂起段 135° 斜纹);执行详情恢复可折叠按需加载的定义版本图;Go 侧画布深链 SPA 回退(T21.2 起统一由根路径面承接)+ CSP 新增 `font-src 'self'`;删孤儿 `canvas.css`/`App.tsx`。验证:`go test ./...` + `go vet` + `golangci-lint run` 零告警、vitest 85 例全绿、build + dist 漂移门干净、playwright e2e 2 例(全旅程 30.2s + SPA 回退)全绿)
- [x] T21.2 审批台并入统一 SPA + 路由硬切 — L — issue #197 — 依赖 T21.1 — **已交付**(旧 `/ui/` 原生三件套(index.html/app.js/style.css)退役 → `ApprovalsView/ApprovalQueue/ApprovalDetail` 并入同壳同设计系统,登录全产品一处;三条对抗审计不变量逐条带测试锚(listSeq 陈旧响应丢弃 / 换详情清空未提交备注 / in-flight 双按钮防抖);增项 = 键盘快捷键(j/k/Enter/a/r/Esc,**输入框聚焦与 `ev.repeat` 一律让位**)、筛选与状态徽标、统一错误词表;驳回加**行内确认层**(一次性浮起层布防即移焦,与执行面「取消执行」同契约)。路由硬切:`/`、`/login`、`/workflows/*`、`/executions/*`、`/approvals/*`、`/assets/*`;`/ui` 与 `/ui/*` **零路由注册 → gin 404 无重定向**(web_test 路由表全等锚 + e2e `maxRedirects:0` 双面锚);vite base 改 `/`、dist 直出 `internal/api/web/`;全仓旧路径引用清零(清扫前 16 文件 71 处 → 写域 0,残留仅历史档与硬切断言本身);README 新增「0. 控制台」节 + 三张界面截图。验证:`go test ./...` + `go vet` + `golangci-lint run` 零告警、vitest 134 例全绿、build 在钉版 CI 镜像内逐字节同哈希、playwright e2e 2 例(全旅程 27.5s + 硬切/SPA 回退)全绿、真机路由探针 200/404 全对)

- [x] T21.3 设计系统 v2:双主题 + shadcn 复合组件层 — L — issue #201 — 依赖 T21.1/T21.2 — D-2026-10-09-console-v2:范围 = 七条反馈的 ①②⑦(共享设计系统面)。① 双主题三态(auto/light/dark,深色仍为默认)+ 浅色地面/墨色阶梯按**与深色同一条验收线**重算定档(正文 ≥4.5:1、图形 ≥3:1),**六态状态色相允许在浅色地面重配**(逐态复核);② 自研 token 仍是唯一真源,shadcn 语义变量仅作别名注入(`:root` + 主题选择器),切主题 = 换别名值,三类自研类 111 个零改动;⑦ shadcn/ui 只引入复合交互组件 dialog/select/dropdown-menu/tabs/tooltip/popover/checkbox/switch,14 个基础原语(primitives 9 + field 5)保留自研,**Select/Checkbox 单点收敛**(自研外壳 + shadcn 底层,禁两套并存);UI 偏好(主题 + 折叠态)走 localStorage,与认证 token 载体分离。验收:三件套零告警 + vitest(新增浅色/切换用例)+ dist 同 commit 重建 + 漂移门 + playwright e2e;**深色/浅色两套各跑一遍对比度实测**并写入 DESIGN.md(Colors 节重写 + 带对比度的 Named Rules 逐条重算)+ `.impeccable/design.json` 同步 + impeccable CRITICAL/HIGH 清零 + 桌面/窄窗截图两轮 — **已交付**(T21.3 阶段 3:① 主题三态 auto/light/dark 控件落壳顶栏,偏好 `localStorage('braid.theme')`,首绘前由同源外部脚本 `assets/theme-boot.js` 落定(CSP 是 `script-src 'self'`,内联脚本在生产不执行),CSS 默认世界改成深色;② 工作流面左右栏各自可收起,折叠态进 `localStorage`(`prefs.ts`);⑦ 8 个 shadcn 复合组件入库(dialog/select/dropdown-menu/tabs/tooltip/popover/checkbox/switch;registry 自带的 `button.tsx` 被守卫拦下并删除,关闭键改用自研 `.btn` 词汇)+ Select/Checkbox 单点收敛(自研外壳 + Base UI 底层,registry 外观 utility 由适配 CSS 收回自研词汇)。**实测推翻两处阶段 2 的取值**:(a) 浅色六态按「最不利地面」二次重配——旧值在选中行 + 自色 tint 上只有 3.2–4.0:1,新值最紧 4.60:1;(b) 状态芯片底色改**不透明**(半透明时标签对比度随行状态漂移,深色 FAILED 在选中行 3.88:1);另把深色 `line-2` 由 `#556a7e` 提到 `#5b7185`(旧值对控件**自身填充**只有 2.94:1,对抗审计 M1)。两个对比度脚本都改成真门(欠档即 exit 1)。验证:三件套全绿(`go test ./...` / `go vet` / `golangci-lint run` 0 issues)、vitest 155 例、dist 同 commit 重建、playwright e2e 2/2、对比度两套世界实测写入 DESIGN.md(契约面 15 对 + 浏览器渲染面三面,欠档 0)+ `.impeccable/design.json` 同步 + impeccable 机械检测无 CRITICAL/HIGH(余 2 条 overused-font 属 Geist 既定决策))
- [x] T21.4 画布与审批交互补充 — M — issue #202 — 依赖 T21.3(共享设计系统与组件层):③ 节点多锚点连线(**已交付**:PR #211 —— `metadata.canvas.edges` 承载,锚点集合按 spec 推导不落盘)+ ④ 节点删除可见入口与确认层(**已交付**:PR #212 —— 真机复现结论 = 键盘删除四处全同步、唯一缺口是零可见入口;面板入口与键盘 Delete/Backspace 同走一条确认漏斗,确认层抽取为 `ui/confirm.tsx` 三面共用,增项「取消后焦点回按钮」随本 PR 一起改)+ ⑤ 执行时间带对齐(**已交付**:PR #223 —— 真机定位结论 = 标签列不背锅(`minmax(12rem,18rem)` 在有自由空间时最大化步骤把所有行含轴行统一顶到 18rem,各行本就同宽);真形态在轴刻度:旧 flex + space-between 只在「3 等宽刻度」时中点恰好压线,5 刻度档 25%/75% 刻度各向内偏 ±标签宽/4(实测 10.2px,带日期 22px),窄轨时刻度整体溢出轨道右缘;修法 = 刻度绝对定位到时间分数、边缘向内锚定/中间居中,真机复测中间刻度中心与网格线逐点重合)+ ⑥ 审批面改同页两级视图(**已交付**:PR #223 —— 列表级 = 队列占满全页,详情级 = 详情占满全页 + 返回队列/Esc 回列表;伴随修正:归属他节的迟到决定错误原先在任何选中变化时被清掉(静默吞),改为仅在打开新详情时重置、回列表级仍见队列错误条)+ 增项 = 确认层取消后焦点回按钮(**已交付**:PR #212,执行面/审批面/画布三处)+ 审批队列「已等待多久」(**已交付**:PR #223 —— `pendingApprovalDTO` 加 `requested_at` = `status_changed_at`(进入 WAITING_APPROVAL 的时刻;`started_at` 跨回滚重跑不变,等待起点须随最新一轮),队列加「已等待」列复用 `Duration` 组件)。验收:三件套 + vitest(179/179,新增 ⑤ 刻度锚定契约、⑥ 两级视图、增项 requested_at 语义三组锚)+ dist 同 commit + e2e(本地 e2e 栈 3/3 含审批决定全旅程);④ 真机复现结论入 PR(**已入 PR #212**)
- [x] T21.5 漂移门残余盲区 — S — issue #205 — **已交付**。开工前真机复核**修正原假设**:web 作业第一步本就是就地重建(`npm run build` 写 `internal/api/web`),`git status` 门比对的是「重建结果 vs HEAD」——**已在抓「忘了重建 dist」**(复现:改 `main.tsx` 不重建 → 门红 12 项 `D/M/??`)。真残余缝 = 门的判据依赖「构建写到门所看的路径」:`vite build.outDir` 改走而 dist 不同步时,重建落新路径、旧 dist 原样 → 门对冻结目录恒绿,此后源码再漂也一路绿,而 `go:embed web` 已陈旧(同一工作树双证:旧门绿、新门红)。交付 = 路径无关判据:① `npm run build:verify`(`vite build --outDir .dist-verify --emptyOutDir`,不再就地重写 checkout);② 判据本体 `web/canvas/scripts/dist-drift-gate.sh`(对象三守卫 + `diff -r --brief` 逐文件差异清单)替换旧门,不留双门;③ CI 门自证步骤(注入内容漂移 / 缺文件 / 门对象缺失,三注不红即作业红——防门自身恒绿,该门曾静默恒绿三天)。负向四例本机全红 + 正向对照绿 + 钉版 CI 镜像内重建与入库产物逐字节相等。**独立对抗审计**(fork,CRITICAL 0 / HIGH 2 / MEDIUM 4 / LOW 4)全修:① `diff --no-dereference`(旧判据能抓的 regular→软链类型变更,而 `go:embed` 静默丢软链 → 路由 chunk 运行时 404,无第二道门)② `.dist-verify` 同名**软链**可被提交 → `.gitignore` 去尾斜杠 + 两侧同源守卫(`-ef`)③ `build:verify` 前置 `rm -rf`(静默不写的构建不许留残树)④ 门自证 6 注(含**默认参数调用**,钉住 CI 实际调用面 + 守卫 2/3)⑤ 保留旧判据「dist 与 HEAD 一致」那半信号 ⑥ 守卫面从 `assets/` 放宽到整个对象 ⑦ 脚本自 `cd` 仓库根(cwd 无关,仓库外 fail-closed)⑧ `vite build.outDir` 配置断言(漂移当场红)⑨ `.dockerignore` 收 `.dist-verify`;回执贴 PR #206。附收尾撞出的**独立缺陷** → T21.6 / issue #207(Tailwind 取材面 = `web/canvas/**` 全量非应用文件:改脚本注释就会改写入库 CSS;非本次引入,旧门同红,ROADMAP 尚未登记)。验证:三件套零告警(`go test ./...` 29 包 rc=0 / `go vet` / `golangci-lint run` 0 issues)+ vitest 155/155 + CI **7/7**

- [x] T21.6 画布 CSS 取材面收口 — S — issue #207 — 口径 A(推荐已拍板随 M22):`@import 'tailwindcss' source(none)` + 显式 `@source '../'`,CSS ⟵ 应用源码写成不变量 — 验收:改 scripts 注释重建后入库 CSS 逐字节不变;src 类零丢失(选择子超集断言);三件套 + vitest + dist 同 commit + 漂移门绿

## M22 审计修复第一波(P4-2,docs/audits/STRANGER-AUDIT-2026-10-14.md 落地)

> 来源 = 2026-10-14 陌生人旅程审计波(104 agents 对抗核证,56 存活:0C/5H/28M/23L;报告与波次建议全录 docs/audits/STRANGER-AUDIT-2026-10-14.md)。本波 = P4 残件(#202⑤⑥、#207、#210、#213 闭单)+ 全部 HIGH(F02/F03/F04/F05/F10);engine 零触及,纯前端 + api 错误路径 + docs,可并行 worktree。开发工作流拍板(2026-10-14 用户):复杂度分叉——复杂票 fork worktree 并行 + 主会话集成,简单票主会话直修(ponytail 代码风格 + mp-code-review 交叉对抗审,发现回主编码会话修完重验);项目铁律(CI/PR/测试三件套)不变。

- [x] T22.1 F05 校验错误结构化 — M — issue #214 — 后端 asInvalidDSL 把 jsonschema 各 cause 按 InstanceLocation 拆成结构化 issues(前端 IssuesPanel 已支持渲染)+ 前端编辑期间旧错误清除/降权(修「oneOf 全臂原始转储整屏」+「错误横幅常驻」) — 验收:三件套 + vitest + dist 同 commit + 漂移门;结构化 issues 对空 LLM 节点首存场景端到端可读
- [x] T22.2 F03 定义列表分页接入 — M — issue #215 — listDrafts/listPublished 消费 page envelope(total),接入执行面已有翻页器模式(修「超 20 条静默消失」+「第 21 个已发布不进触发下拉」) — 验收:三件套 + vitest + dist 同 commit;21 条草稿/发布全量可达
- [x] T22.3 F04 发布成功反馈重构 — M — issue #216 — 成功反馈与编辑器卸载解耦(顶层 toast 或列表态成功条)+ 「查看已发布」出口(修「发布像凭空消失」) — 验收:三件套 + vitest + dist 同 commit + e2e 旅程不回归
- [x] T22.4 F10 回滚确认层 — S — issue #217 — 回滚接入 ui/confirm.tsx(与同面「取消执行」同契约);续跑维持直按或一次布防,不硬套同档确认 — 验收:三件套 + vitest + dist 同 commit
- [x] T22.5 F02 prod 首账号文档缝 — S — issue #218 — deploy 补「首个登录账号」一节(compose 网内 root DSN 跑 `braid user create` + 角色与口令纪律),README §2.1 与 prod-up.sh 就绪横幅指路 — 验收:prod 路径陌生人可登录(纯 docs)
- [x] 残件承接:#202(**已交付**:PR #223,T21.4 ⑤⑥ + 增项一并收口)、#207(T21.6 Tailwind 取材面,主会话直修)、#210(fence 种子错峰,主会话直修)、#213(修复已落 39ef08d,master 复跑 web 作业绿后直接闭单)

### M22-2 用户体感反馈(2026-10-15 亲测,排第二波前排;先拍板后动工)

- [ ] UX-F1 控制台视觉整备(参照系已拍板 2026-10-15:n8n/Linear/Vercel/Temporal 分面取长,D-2026-10-15-console-references)— issue #227(父票)— 四子项:
  - [ ] F1-a 壳层/审批 Linear 化:近黑细线、行式紧凑、键盘提示外显 — M — 吸收审计 F08/F25/F27/F28 层级密度族
  - [ ] F1-b 画布 n8n 化:亮色画布默认 + 彩色类型节点(对比度门内)+ 胶囊形 — M — 依赖 D-2026-10-15 分面默认;与 UX-F2 连线/布局同域可同批
  - [ ] F1-c 执行总览 Vercel 化:大数字卡片 + sparkline — M — 并审计子波 A 可观测数据面(F13/F17/F18/F29 为其内容源)
  - [ ] F1-d 执行列表 Temporal 化:密表格 + tab + 过滤 — M — 并审计 F21/F23/F24(筛选/加载/错误态)
- [ ] UX-F2 画布连线不灵活 + 初始排版堆叠 — M-L — issue #228 — 恒上下单锚改全向锚/自由拉线(触及 T21.4③ 不落盘口径则新 D-);并审计 F36/F64(新节点同坐标堆叠 + 不自动选中)
- [ ] UX-F3 节点删除入口不可发现 — S — issue #229 — icon-only 无名按钮补 aria-label/title/文字形态,同面板同病扫描(与 F08 a11y 同波)


## M18 停车场清账波(P3-1,可唤醒面清零)

- [x] T18.1 SSRF 扩禁五段落地 — S — **已拍板全五段**(2026-10-05) — 验收:NAT64 64:ff9b::/96、198.18.0.0/15、192.0.0.0/24、0.0.0.0/8 非全零、240/4 剩余入禁;ssrf_test 锚点同步(want=false→true);t10.1 e2e 网段重选重签材料;D-t102 代理口径继承同步;D- 条目随 PR
- [x] T18.2 token 预算闸门消费点 — M — 验收:config 旋钮 + Validate/启动期接线 + 超限拒绝语义(审计分面);前置 T7.1/T7.2 已交付,悬置理由失效(全仓无消费点已核)(T18.2 交付:D-2026-10-05-t182-token-budget-gate,每执行口径,审计分面 llm.call.budget_denied)
- [x] T18.3 完整 taint 追踪 — M — 验收:输入→节点→审计的污点传播追踪(方案 D- 先行:追踪粒度/存储面/审计呈现;数据安全纵深叙事)— D-2026-10-05-t183-taint-tracking(字段级声明名+保守闭包 / 仅审计呈现零迁移 / node.taint_propagated 词表)
- [x] T18.4 回滚族⑥ 投影只读端点 — M — **已交付**(D-2026-10-05-t184-projection-read-endpoints:按路径 = node path 投影详情[?instance_no 寻址]/按轮 = attempt 台账瘦列表 + 单轮四件套,round ≡ attempt_no;零 engine 触碰) — 验收:api 按轮/按路径投影只读端点(纯读面追加)
- [x] T18.5 回滚族①⑦ — S — **已交付**(D-2026-10-06-t185-rollback-replay-and-trace-lineage:重放 = api 层错误路径读回最新 rolled_back 台账[not_rollbackable/ErrConflict 两收敛面,操作级幂等不校验 actor,200+审计重建 DTO 零新写 + Replayed 计数器];⑦ = 段首家族扩四词[rolled_back/rollback.denied/cancelled/cancel.denied 携 linked=X,词表闸单点化],重跑任务 trace 同链锚) — 验收:重放 200 幂等语义(重复回滚 409→重放)+ trace_id 世系归属复核
- [x] T18.6 回滚族⑧ 留档保留期 — S-M — 验收:node_attempt 留档接 janitor(现四面未覆盖)+ docs/CAPACITY.md 补面(T18.6 交付:D-2026-10-05-t186-node-attempt-retention,30d 容量定档/heartbeat_at 时钟骑既有索引/活执行守卫/RUNNING 永不删/专属轮上限 25000 对账摄入帽,零 DDL)
- [x] T18.7 T17.4 观测缝隙收口 — S-M — 验收:reaper probe 面测试锚逐项补齐 + 探测不可达 fail-open 计数 + im_notified 对账分轮

## M19 引擎纵深主轴(P3-2,engine 单 worktree 串行)

- [x] T19.1 min_success 部分容忍 — M — D- 三件语义拍板先行(①新字段 vs on_failure 第三值;②部分成功执行终态语义——波及 api 读面/告警/gather 物化口径;③部分失败 results 物化形态)— D-2026-10-05-t191(①独立整数字段正交 on_failure;②终态保持 SUCCEEDED 零枚举,波及面六面盘点实证零词表连锁;③null 保位 + failed_count 配置驱动)
- [x] T19.2 foreach {{nodes.*.output.*}} 动态扇出 — L — 依赖 T19.1 — D- 破不变式三面重论证(finishIfDone 正确性 / lockAllRows 全行锁 / uk_node_exec 语义)+ dispatch-time 展开 + DBTX 批量读(T19.2 交付:D-2026-10-05-t192,Instantiate 分流/派发位 reconcile[缺行补插+盈余 SKIPPED 保留]/gather 活跃集滤除;零 DDL 不占 0015)
- [x] T19.3 SUCCEEDED/CANCELLED 执行回滚 — M-L — 独立 D- — 捆绑楔源预扫 + 执行级取消原语(现白名单仅 FAILED);是 T19.5/T19.6 的前置(T19.3 交付:D-2026-10-05-t193,楔源=取消楔停行集双捆绑[取消事务预扫+resume 自动定位退化链 FAILED→CANCELLED→PENDING 冻结]、CancelExecution 原语[在飞照样楔停,T19.6 前置]、RestartExecution 白名单三终态、worker 活态闸[认领时非 EXECUTING 幂等 ack,与轮次校验正交]、POST /executions/:id/cancel + CancelSet + canvas;零 DDL 不占 0015)
- [x] T19.4 reaper 自动续跑策略 — M — 拍板类(D-t92#5 现明文不自动复活终态执行;拍板是否开)— **定案:维持不开**(2026-10-06 用户拍板,零改动收口;重开触发器 = 真实运营续跑诉求,须新 D-)
- [x] T19.5 parallel 增量恢复 — M — 依赖 T19.3 — 独立 D-(单实例粒度恢复,现整行集重跑;与实例收口/ReapInstanceExhausted 语义交叉)(T19.5 交付:D-2026-10-06-t195,重置面保活 + 三重门控[意图 target×终态/快照依赖闭包交集/行集原子性]、dispatchParallel claim 过滤 + 尾部 gatherCheck、零 engine 改动零迁移;取消楔源恢复自动增量,再生意图整行集重跑)
- [x] T19.6 在飞回滚 quiesce 协议 — L — 依赖 T19.3 — 独立 D-(现 409 blocked_inflight 等 reaper settle)(T19.6 交付:D-2026-10-06-t196,quiesce 显式旗标[缺省 409 观测态保持——状态码/Retry-After/blocked 逐字,detail 补 quiesce 出路;等待保值/楔停换速度两态一等公民] + 单事务楔停收敛[attempt 收口 code=rollback_quiesce + CancelNode + 审计,闭包归属=既有收编谓词零追加,D-t193 原语行级子集] + 观测三面[rolled_back.quiesced_count / DTO 重放镜像 / Quiesced 计数器] + 守卫① FOR UPDATE[D-t195 审计 MEDIUM④ 遗留收编] + canvas quiesce 复选;零 DDL 不占 0015、零 engine)

## M20 可靠性收口波(P3-3,与 M18 并行,零 engine 耦合)

- [x] T20.1 租约 fencing token — M-L — D- 立项先行 — zombie 双签发/双执行窗收口(贯穿租约仲裁与任务执行两面;D-t151 决策8①「另议」至今未裁)(T20.1 交付:D-2026-10-06-t201-lease-fencing,等值加冕路线——0015 fencing_epoch 单列,持有变化同语句 +1(复占也 bump,收 D-t204 复占双活面);执法 = 编排层写事务首置同源锁定读,engine 零触;判等统一 utf8mb4_bin;双 metric 族 + LeaseFenceRejections 告警 + drill arm8 废黜臂)
- [x] T20.2 scp 拒绝面审计补全 — M — D- 微条先行 — scope 执法语义自路由中间件移入 handler 层(现 scoped 拒绝短路于中间件、不落审计)(T20.2 交付:D-2026-10-07-t202-scp-denied-audit,RequireScope 中间件退役 → in-helper requireScope 单点[判定核 auth.RequireScope 双面单源不动],22 路由迁移;4 条 action 路由 scoped 拒绝复用既有 denied 事件族 reason=scope_denied[零新事件词/写入器/迁移]+Denied 计数器扩 label;探针合法化两行;非锚/读面/角色半边/MCP 边界登记)
- [x] T20.3 fleet 闸 TOCTOU 闭合 — S-M — 发布面 DSL 读取移入 Publish 事务(触发面闸钉版快照为最后防线,收前置窗)
- [x] T20.4 租约活性复核转正 — S-M — sweep 前复核(T15.3 followup 位;活体同 owner 互杀面可选缓解)
- [x] T20.5 LDAP 时序残差垫平 — S-M — unknown_user/wrong_password 时域差(P1 加固通道;先解服务账号自锁风险)(T20.5 交付:D-2026-10-06-t205-ldap-timing-pad,零命中纯客户端垫平 + 启动实测 rebind 中位 clamp [1ms,1s];自锁结构性消亡 + 对齐/存在锚/结构锚/判定保真四族断言,手搓 BER fake 零栈承载)
- [x] T20.6 docker 网络泄漏回收 runbook — S — 文档活:并入孤儿清理节 + default-address-pools 上界注记(≈31 用户网/daemon)(T20.6 交付:RUNBOOK §1.3 场景节——症状[worker 拒启词面 + daemon 池耗尽 27+/≤26 两代词面]/只读两步诊断/回收红线[禁 prune:零附着活网误删面]/扩容注记[整组替换 + 新建网面 + 段避让];上界精推 = 31 席 − docker0 = 用户网 30,宿主网段重叠再吃席,历史 ≈31 = bridge 族触顶行数;§1.1 与 README §9 加指针)
- [x] T20.7 静态分区水位时弹收口 — S-M — 遗留自 t2.5 时弹族;独立 D- 两口径先行(① 0015 扩静态水位,治本但会再过期;② 安装口径钉死 reaper 先起,零迁移)— 验收:dev/t13/e2e/生产口径统一,水位过期不再触发 preflight 硬拒 × janitor 首刷竞态(e2e-up 已先行钉死,33c95d1)(T20.7 交付:D-2026-10-05-t207-install-order,裁②侧;braid audit-ready 判定动词 + 四栈分层执法 + 水位/p_min 双哨兵)
- [x] T20.8 drill arm3 断言追认 T20.4 语义 — S — 先存回归(#174 评论登记):sandboxSweepHoldoff「同 owner 活租约 ⇒ 跳过启动清扫」(D-t204 边界①,cmd/worker/main.go:399)与 T15.6 arm3「restart 即清扫」期望冲突,arm3 确定性 60s 超时(master 基线同红,arm1/2 过)— 验收:arm3 断言改造为「等 TTL 过期/显式释放后清扫收敛」或 arm 序列调整避免复占活租约形态;TestMultiReplicaDrill 三臂全绿(T20.8 交付:GATE 1 裁断言改造路线——arm3 两幕:伪活租约钉误跳过负断言 + WARN 词面 tools 导出常量编译期钉死 + 清行/优雅释放/TTL 过期后下次启动收敛;drill 五臂绿 + 三件套绿 + 对抗审计 1 MEDIUM/8 LOW 全收)
- [x] T20.9 reaper 雪花节点内建缺省收口 — S — issue #208 — 内建缺省 `auto` → 显式 3(与 api=1/worker=2 同形,即 compose 惯例号):`auto` 池取「首个空闲号」在单副本 bare start 下与 api/worker 争 1/2 且号不可预测,而 auto 的正当语义是多副本自取号(k8s pinned-env 僵局);env 通道与多副本 `auto` 语义不变(D-2026-10-10-t209)。验收:三件套 + reaper 反锚(env 空 → `Explicit(3)`;DefaultConfig 与 ParseNodeSpec 两条路径同源单常量,半修即红)+ 注释/文档同步 9 处;行为面 = env 传值面不动(prod `"3"` / dev `.env`=3 / drill `auto` / chart `auto`),唯一实测变化 = env-less 的 `deploy/e2e-up.sh` reaper 由 auto 池取 0 改显式 3(api=1/worker=2 不变,无碰撞)(T20.9 交付:D-2026-10-10-t209;三件套全绿 + 反锚(env 空 → `Explicit(3)`,两路径共用常量);issue #208)

## P3 停车场(触发器休眠,防遗忘)

- **维持休眠(2026-10-05 用户拍板)**:多租户重契约族(4 子项,须独立 D- 且重开五条已锁契约,触发 = 真实运营诉求);动态 DAG/子工作流/LLM 流式(触发 = 需求事件);自由文本 payload 面(联动 PII 脱敏评估 P1② 与回滚理由⑨,触发 = 面将放开)
- metrics 无认证:触发 = 公网/不可信网络部署(P1③)
- Sentinel 重开:触发 = 容量模型瓶颈实证(D-t151 决策3);audit_trace_map v1:触发 = 审计体量 >2000 万行(D-t155);GHCR 通道:三条重访触发器(D-t165);markdown 档 2:触发 = linkify 钓鱼证据(D-t175)
- module 真拆:随开源决策 Q7 复议,非 P3 阻塞(D-t161)
- 其余登记不修族维持原状,全录见 `braid-p2-survey.json`(仓外)与 DECISIONS 各 D- 登记节
