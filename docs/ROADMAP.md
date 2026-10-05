# braid 路线图(P3)

> 任务定义与依赖真源 = 本文件;执行状态真源 = GitHub issues(认领/进行中/闭环,`gh issue list`)。勾选仅作历史镜像;勾选提交随任务 PR、merge 即生效,PR 描述带 `Closes #N`(远端 Closes 偶发不触发,合并后 `gh issue close` 人工补)。
> 规模:S = 半天内,M = 1-2 天,L = 3+ 天(全职当量)。
> 里程碑依赖:M18(清账)∥ M20(收口)先行,M19(引擎纵深)随后串行(engine 包单 worktree 公约);任务级依赖单独标注,冲突时以任务级为准。
> 方向拍板(2026-10-05 用户):**三轨全开**(P3-1 清账波 + P3-2 引擎纵深主轴 + P3-3 可靠性收口);**SSRF 扩禁 = 全五段**;多租户重契约族/动态 DAG 评估/自由文本面**维持触发器休眠**。46 条线索扫描与处置全录见会话档;P3 各任务的语义级拍板(min_success 三件/fencing 立项/scp 审计 D-/回滚族②③④⑤裁剪)在其 GATE 1 逐项裁。
> P0 冻结记录 = docs/ROADMAP-P0.md;P1 = docs/ROADMAP-P1.md + docs/P1-DELIVERY.md;P2 = docs/ROADMAP-P2.md + docs/P2-DELIVERY.md。本文件 2026-10-05 起 = P3 活文档。

## M18 停车场清账波(P3-1,可唤醒面清零)

- [x] T18.1 SSRF 扩禁五段落地 — S — **已拍板全五段**(2026-10-05) — 验收:NAT64 64:ff9b::/96、198.18.0.0/15、192.0.0.0/24、0.0.0.0/8 非全零、240/4 剩余入禁;ssrf_test 锚点同步(want=false→true);t10.1 e2e 网段重选重签材料;D-t102 代理口径继承同步;D- 条目随 PR
- [x] T18.2 token 预算闸门消费点 — M — 验收:config 旋钮 + Validate/启动期接线 + 超限拒绝语义(审计分面);前置 T7.1/T7.2 已交付,悬置理由失效(全仓无消费点已核)(T18.2 交付:D-2026-10-05-t182-token-budget-gate,每执行口径,审计分面 llm.call.budget_denied)
- [x] T18.3 完整 taint 追踪 — M — 验收:输入→节点→审计的污点传播追踪(方案 D- 先行:追踪粒度/存储面/审计呈现;数据安全纵深叙事)— D-2026-10-05-t183-taint-tracking(字段级声明名+保守闭包 / 仅审计呈现零迁移 / node.taint_propagated 词表)
- [x] T18.4 回滚族⑥ 投影只读端点 — M — **已交付**(D-2026-10-05-t184-projection-read-endpoints:按路径 = node path 投影详情[?instance_no 寻址]/按轮 = attempt 台账瘦列表 + 单轮四件套,round ≡ attempt_no;零 engine 触碰) — 验收:api 按轮/按路径投影只读端点(纯读面追加)
- [ ] T18.5 回滚族①⑦ — S — 验收:重放 200 幂等语义(重复回滚 409→重放)+ trace_id 世系归属复核
- [x] T18.6 回滚族⑧ 留档保留期 — S-M — 验收:node_attempt 留档接 janitor(现四面未覆盖)+ docs/CAPACITY.md 补面(T18.6 交付:D-2026-10-05-t186-node-attempt-retention,30d 容量定档/heartbeat_at 时钟骑既有索引/活执行守卫/RUNNING 永不删/专属轮上限 25000 对账摄入帽,零 DDL)
- [x] T18.7 T17.4 观测缝隙收口 — S-M — 验收:reaper probe 面测试锚逐项补齐 + 探测不可达 fail-open 计数 + im_notified 对账分轮

## M19 引擎纵深主轴(P3-2,engine 单 worktree 串行)

- [x] T19.1 min_success 部分容忍 — M — D- 三件语义拍板先行(①新字段 vs on_failure 第三值;②部分成功执行终态语义——波及 api 读面/告警/gather 物化口径;③部分失败 results 物化形态)— D-2026-10-05-t191(①独立整数字段正交 on_failure;②终态保持 SUCCEEDED 零枚举,波及面六面盘点实证零词表连锁;③null 保位 + failed_count 配置驱动)
- [x] T19.2 foreach {{nodes.*.output.*}} 动态扇出 — L — 依赖 T19.1 — D- 破不变式三面重论证(finishIfDone 正确性 / lockAllRows 全行锁 / uk_node_exec 语义)+ dispatch-time 展开 + DBTX 批量读(T19.2 交付:D-2026-10-05-t192,Instantiate 分流/派发位 reconcile[缺行补插+盈余 SKIPPED 保留]/gather 活跃集滤除;零 DDL 不占 0015)
- [ ] T19.3 SUCCEEDED/CANCELLED 执行回滚 — M-L — 独立 D- — 捆绑楔源预扫 + 执行级取消原语(现白名单仅 FAILED);是 T19.5/T19.6 的前置
- [ ] T19.4 reaper 自动续跑策略 — M — 拍板类(D-t92#5 现明文不自动复活终态执行;拍板是否开)
- [ ] T19.5 parallel 增量恢复 — M — 依赖 T19.3 — 独立 D-(单实例粒度恢复,现整行集重跑;与实例收口/ReapInstanceExhausted 语义交叉)
- [ ] T19.6 在飞回滚 quiesce 协议 — L — 依赖 T19.3 — 独立 D-(现 409 blocked_inflight 等 reaper settle)

## M20 可靠性收口波(P3-3,与 M18 并行,零 engine 耦合)

- [ ] T20.1 租约 fencing token — M-L — D- 立项先行 — zombie 双签发/双执行窗收口(贯穿租约仲裁与任务执行两面;D-t151 决策8①「另议」至今未裁)
- [ ] T20.2 scp 拒绝面审计补全 — M — D- 微条先行 — scope 执法语义自路由中间件移入 handler 层(现 scoped 拒绝短路于中间件、不落审计)
- [x] T20.3 fleet 闸 TOCTOU 闭合 — S-M — 发布面 DSL 读取移入 Publish 事务(触发面闸钉版快照为最后防线,收前置窗)
- [x] T20.4 租约活性复核转正 — S-M — sweep 前复核(T15.3 followup 位;活体同 owner 互杀面可选缓解)
- [ ] T20.5 LDAP 时序残差垫平 — S-M — unknown_user/wrong_password 时域差(P1 加固通道;先解服务账号自锁风险)
- [ ] T20.6 docker 网络泄漏回收 runbook — S — 文档活:并入孤儿清理节 + default-address-pools 上界注记(≈31 用户网/daemon)
- [ ] T20.7 静态分区水位时弹收口 — S-M — 遗留自 t2.5 时弹族;独立 D- 两口径先行(① 0015 扩静态水位,治本但会再过期;② 安装口径钉死 reaper 先起,零迁移)— 验收:dev/t13/e2e/生产口径统一,水位过期不再触发 preflight 硬拒 × janitor 首刷竞态(e2e-up 已先行钉死,33c95d1)

## P3 停车场(触发器休眠,防遗忘)

- **维持休眠(2026-10-05 用户拍板)**:多租户重契约族(4 子项,须独立 D- 且重开五条已锁契约,触发 = 真实运营诉求);动态 DAG/子工作流/LLM 流式(触发 = 需求事件);自由文本 payload 面(联动 PII 脱敏评估 P1② 与回滚理由⑨,触发 = 面将放开)
- metrics 无认证:触发 = 公网/不可信网络部署(P1③)
- Sentinel 重开:触发 = 容量模型瓶颈实证(D-t151 决策3);audit_trace_map v1:触发 = 审计体量 >2000 万行(D-t155);GHCR 通道:三条重访触发器(D-t165);markdown 档 2:触发 = linkify 钓鱼证据(D-t175)
- module 真拆:随开源决策 Q7 复议,非 P3 阻塞(D-t161)
- 其余登记不修族维持原状,全录见 `braid-p2-survey.json`(仓外)与 DECISIONS 各 D- 登记节
