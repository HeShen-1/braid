# braid P2 交付总结(冻结记录)

> P2 = docs/ROADMAP.md M15-M17,18/18 任务于 2026-10-04 全量交付。窗口:93a202e(P2 立项,2026-09-30).. d82c235(PR #153)。数据经 git/文件实证;执行队列镜像 = issues #116-#153(全闭环)。**P0+P1+P2 三期收官:74 任务、PR #1-#153、迁移 14 组、CI 4→7 作业。**

## 数字面板

| 维度 | 数值 |
|---|---|
| 交付节奏 | 五波次(W1-W6)× ≤3 并行 lane,5 天全收(2026-09-30 → 10-04) |
| PR / commit | 19 个交付 PR(#119-#153 段)、73 commit(53 非 merge + 20 merge) |
| 代码量 | 199 files changed,+16,610 / −1,112 |
| 迁移 | 0012 snowflake_node_lease / 0013 login_audit_scoped_token / 0014 login_audit_method_ldap |
| CI | 6 → 7 作业(+e2e,#145/D-t166),零 Secrets 姿态维持 |
| 测试 | 顶层 Test 函数 895 → **992**(+97);告警规则 16 → 21 条 |
| 决策治理 | P2 期 14 条 D- 条目(DECISIONS 累计 60 条) |
| 质量门禁 | 11-agent HA 设计对抗、12-agent CLI 对抗(12 HIGH 去重 2 真)、六视角 × 多任务、orch-review 三维全量;CRITICAL/HIGH 清零纪律全程零事故 |

## 里程碑亮点(证据 = D- 条目 / 文件路径)

**M15 规模化与多副本 HA(P2 主轴)**
- HA 拓扑总裁定(D-2026-10-01-t151,11-agent 对抗审计 rev2):四组件全水平扩展目标态;队列 Redis = 单实例 + 结构韧性(outbox+reaper 承载正确性,RPO 口径量化),**Sentinel 以负样本论证否决**(异步复制 failover 丢写窗无上界,官方自认 + Jepsen 1126 条);MySQL HA 判归运营域,failover 不破哈希链论证
- 副本身份三件套(迁移 0012):雪花节点号 = MySQL 租约表单真相源(三层解析/auto 池抢注/续期失败停发并优雅下线/SIGTERM 释放);沙箱 owner 化清扫(部署契约 hostname + 宽限 ≥35s);egress 网名实例段独立子网(同端口不撞 + worker 间物理隔离)
- mode=all 舰队硬闸 + 七臂故障注入演练(D-2026-10-01-t156):capabilities 挂租约表 fail-closed,503 fleet_capability_gate;演练栈实证发现并修复 least-privilege 锁定读授权裂缝(t13 root 测试结构性不可见的盲区)
- 压测基线与容量模型(D-2026-10-01-t155,docs/CAPACITY.md):全仓首个 benchmark 三族;MaxRollbackRows 实测定档 4000;audit_trace_map 以实测裁定不建(1M 行 16.9ms/页,距交互线 59×);结构性结论:吞吐帽 = relay 单事件单事务(HA 重开条款消费数据)

**M16 开源与交付面**
- engine/enterprise 物理分层(D-2026-10-01-t161):~2550 行迁企业树,缝留核心(api.OIDCProvider + auth.AnchorLoginUser);边界门双腿 = internal/boundary 包图断言(含防空转锚)+ depguard
- CLI + seed 一键 demo(D-2026-10-03-t162 + D-2026-10-04-t163):建号复用 auth 同一原语、发布/触发走 REST 语义零复制;seed 幂等四件套接进 dev-up.sh;对抗审计 12 HIGH 去重 = 2 真缺陷修于合并前
- 镜像本地导出发布通道(D-2026-10-03-t165,deploy/release.sh):四臂 build/save/load/k3s-import 全实证,GHCR 评估不启用(额度 + 零 Secrets 姿态,三条重访触发器登记);对抗审计 1 HIGH = 判脏守卫 untracked 全盲(porcelain 三态修)
- Playwright e2e 第 7 作业 + 回滚-重跑 UI(D-2026-10-04-t166):deploy/e2e-up.sh 单一真源,钉版镜像,审批拒绝驱动确定性零 mock

**M17 安全与可靠性清账**
- scp 全端点执法(D-2026-10-01-t171):REST 19 authed 端点 RequireScope,词表 3→5,gin 路由集双向集合相等断言防漏挂;D-t131 红线②⑤正式解除
- 组件级授权断言 + scoped token 审计(D-2026-10-04-t173 + 0013):CheckComponentGrants 启动期硬拒 + grants.sh 对拍三测试族(parity/快照/REVOKE 红臂);「组件起得来、首写 1142」前移到启动期
- asynq 双执行硬化 + 告警四组(D-2026-10-04-t174):反应式 Inspector 对拍(哨兵空回滚→事务外探测→verified 二次入事务,不可达 fail-open);告警 16→21 条;PR 审计 2×HIGH 同根因修(probe 须携 outbox 行自带 queue 列)
- IM 预取闭合 + LDAP enterprise(D-2026-10-04-t175 + t176,0014):GET 只读确认页 + POST 落票单点(预取扫描器结构性零落票);markdown 词法全角化中和;LDAP search-then-bind(DN 注入结构性消亡),容器化 e2e 七臂零 ci.yml 改动,go-ldap StartTLS 越界修 = 自管拨号绝对 deadline

## 运维模式沉淀(P2 增量)

- **演练即验收**:七臂故障注入栈 + least-privilege 账号真跑,抓出 root 测试结构性不可见的授权裂缝——「演练账号与生产同权」从根上免疫盲区
- **审计双闸不可互替**:本地双 reviewer 共享上下文盲区由 PR 级盲态审计命中(t17.4 实证);六视角与 orch-review 并行成常态
- **负样本论证**:Sentinel 否决、GHCR 不启用、audit_trace_map 不建——每个「不做」都有量化依据与重访触发器,决策文本的可信度来自承认边界
- **runner 环境假红处置**:sandbox OOM 假红可连红两次(池退化),判定序 = 本地对拍绿 → rerun,三连红才升级
