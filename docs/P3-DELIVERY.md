# braid P3 交付总结(冻结记录)

> P3 = docs/ROADMAP.md M18-M20,21/21 任务于 2026-10-07 全量交付。窗口:f190aab(P3 立项轮转,2026-10-05).. 48f10a8(PR #193)。数据经 git/文件实证;执行队列镜像 = issues #154-#191 段 21 个任务 issue(全闭环)。**P0+P1+P2+P3 四期收官:95 任务、PR #1-#193、迁移仍 14 组(P3 全程零 DDL)。**

## 数字面板

| 维度 | 数值 |
|---|---|
| 交付节奏 | 七波次(W1-W7)× ≤3 并行 lane,3 天全收(2026-10-05 → 10-07) |
| PR / commit | 20 个交付 PR(#157-#193 段)、64 commit(44 非 merge + 20 merge) |
| 代码量 | 154 files changed,+15,339 / −842 |
| 迁移 | **零**——21 任务零 DDL,0015 号全程预留未动(语义设计优先于 schema 变更的整期实证) |
| CI | 7 作业维持零 Secrets;告警规则 21 → **24** 条(+fail-open 计数、LeaseFenceRejections 等) |
| 测试 | 顶层 Test 函数 992 → **1105**(+113);drill 演练扩至八臂 |
| 决策治理 | P3 期 18 条 D- 条目(DECISIONS 累计 78 条) |
| 质量门禁 | 各任务对抗审计 8-36 agent 不等(t19.3 两段 36 agent 0 CH;t18.4 12 agent 0 CH);CRITICAL/HIGH 清零纪律全程零事故;拍板类(T19.4)以负样本定案零改动收口 |

## 里程碑亮点(证据 = D- 条目 / 文件路径)

**M18 停车场清账波(7/7)**
- SSRF 扩禁全五段(D-2026-10-05-ssrf-five-ranges):NAT64 64:ff9b::/96、198.18.0.0/15、192.0.0.0/24、0.0.0.0/8 非全零、240/4 剩余入禁,五段各带测试锚
- 完整 taint 追踪(D-2026-10-05-t183):dsl.ComputeTaintSets 静态污点闭包(字段级声明名+保守闭包)、audit 词表 node.taint_propagated、validateTaintPayload PII 形状闸、Complete/branch 双锚发射——零迁移的纵深叙事
- token 预算闸门消费点(D-t182):每执行口径 config 旋钮 + 启动期接线 + 审计分面 llm.call.budget_denied
- 回滚族四面收口:⑥投影只读端点(D-t184,按路径/按轮,纯 api 读面零 engine 触碰)、①⑦重放 200 幂等 + trace_id 世系(D-t185,段首家族扩四词)、⑧node_attempt 留档接 janitor 第五面(D-t186,零 DDL 骑既有索引)
- T17.4 观测缝隙收口(D-t187):probe 面测试锚逐项补齐 + 探测不可达 fail-open 计数 + im_notified 对账分轮

**M19 引擎纵深主轴(6/6,engine 单 worktree 串行全链闭环)**
- min_success 部分容忍(D-2026-10-05-t191):三件拍板——独立整数字段正交 on_failure / 终态保持 SUCCEEDED 零枚举(波及面六面盘点实证零词表连锁)/ null 保位 + failed_count 配置驱动;finishIfDone 控制面计数 + discarded_after_gather
- foreach {{nodes.*.output.*}} 动态扇出(D-t192):破不变式三面重论证(finishIfDone 正确性 / lockAllRows 全行锁 / uk_node_exec 语义)+ dispatch-time reconcile 展开(缺行补插 + 盈余 SKIPPED 保留 + gather 活跃集滤除),零 DDL
- SUCCEEDED/CANCELLED 执行回滚(D-t193):取消楔停行集双捆绑楔源、CancelExecution 原语(在飞照样楔停)、RestartExecution 三终态白名单、worker 活态闸(非 EXECUTING 幂等 ack)、POST /executions/:id/cancel + canvas
- parallel 增量恢复(D-t195):重置面保活 + 三重门控(意图×终态 / 快照依赖闭包交集 / 行集原子性),取消楔源恢复自动增量;零 engine 改动
- 在飞回滚 quiesce 协议(D-t196):替换「409 blocked_inflight 等 reaper settle」的静默收敛协议
- T19.4 reaper 自动续跑:拍板类**定案维持不开**(零改动收口,重开触发器 = 真实运营续跑诉求须新 D-)

**M20 可靠性收口波(8/8)**
- 租约 fencing token(D-2026-10-06-t201,D-t151 决策8① 悬置落裁):zombie 双签发/双执行窗收口,签发-回读同事务锚定;fence 可观测三族计数器 + LeaseFenceRejections 告警 + drill arm8 演练
- fleet 闸 TOCTOU 闭合(D-t203):闸经 PublishGate 移入 Publish 事务,走私被行锁串行化;审计 MEDIUM 池自饿死同步收口
- 租约活性复核转正(D-t204):sweep 前复核「同 owner 活租约 ⇒ 跳过启动清扫」,边界①-⑥ 论证
- scp 拒绝面审计补全(D-2026-10-07-t202):scope 执法自路由中间件移入 handler 层,scoped 拒绝落审计
- 静态分区水位时弹收口(D-t207,遗留自 t2.5 时弹族):裁安装口径钉序侧——`braid audit-ready` 判定动词 + dev/prod/helm/e2e 四栈分层执法 + 水位/p_min 双哨兵,零迁移
- drill arm3 断言追认(D-t20.8/先存回归):master 基线对拍定性后独立收口,断言改造两幕(伪活租约钉误跳过负断言 + 收敛正断言)
- LDAP 时序残差垫平(D-t205)+ docker 网络泄漏回收 runbook(D-t20.6,纯文档)

## 运维模式沉淀(P3 增量)

- **同事务锚定原则**:同事务共览内存行集的旅程必须有同事务锚(t192 审计 HIGH 变异实证);签发-回读同理(t201 审计 HIGH)——跨事务锚不住的东西不要假装锚了
- **门控设计先问「没建模哪种依赖」**:t195 HIGH 教训——数据门漏控制依赖,门控枚举的不变式集合要按依赖类型穷举
- **先存回归纪律**:基线(git-archive 干净导出)对拍定性 → 独立任务收口,不在无关任务顺手修(scope 纪律)
- **零迁移整期**:21 任务全部以语义设计消化(正交字段/词表/闭包/门控),0015 号预留未动——schema 稳定是多期工程的健康信号
- **波次制协调**:协调会话 fork/收尾全流程标准化,七波 21 任务 3 天;engine 单 worktree 公约 + 槽位分配表 + rollback 面互斥公约全程零互踩
