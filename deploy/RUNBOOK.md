# braid 运维手册(runbook)

> 稳态运维原语;版本升级流程见 deploy/UPGRADE.md,配额与拓扑契约见
> deploy/README.md。只收「交付面之外、运维显式动作」;随任务按需增长。

## 1. 沙箱孤儿容器与泄漏 egress 网(T15.3/T15.4,D-t151 决策 5)

worker 沙箱容器与 egress 网都是 docker daemon 持久对象,worker 进程退场
不自动回收。两类残留同源(进程灭失或身份变更),清扫一节:

### 1.1 泄漏 egress 网(T15.4)

egress 网名 = 基段 + worker_id 实例段(缺省族
`braid-sbx-egress-<8 hex>`;自定基段换前缀)。worker 正常重启同名幂等
复用;**身份变更**(容器 recreate/升级路径——compose 缺省无 `hostname`
键时 `up -d` 必换容器短 ID)即泄漏一网:旧网无人监听,却占 daemon
地址池配额(缺省 ≈31 用户网/daemon,k8s host docker.sock 按节点计)。
**误删活网 = 该 worker 全部 http 沙箱出网即断**,rm 前每步都过:

```sh
# 1. 列出 egress 网(实例段族 + 裸基段网均入列;自定基段换前缀)
docker network ls --filter name='^braid-sbx-egress' --format '{{.Name}}'

# 2. 逐网核活:网关 IP 上代理端口有监听 = 活网,跳过。
#    端口 = 部署实际 BRAID_TOOL_SANDBOX_EGRESS_PORT(缺省 9210;自定端口
#    的部署替换之;PORT=0 内核指派部署本探针不适用,以第 3 步附着判定
#    + owner 核对为准)。端口无关替代:按网关 IP 查任意监听
#    (ss -ltn | grep <gw>)。网关 IP 见 inspect。
docker network inspect -f '{{.Name}} {{(index .IPAM.Config 0).Gateway}}' <net>
nc -z <gw> 9210

# 3. owner 核对:确认无活 worker 持该实例段——查最近 worker 启动日志行
#    `worker: sandbox egress proxy serving ... network=<name>`
#    (cmd/worker/main.go),或核对 node_attempt.worker_id 无同身份活体。
#    有活 owner = 跳过(探活与 owner 判定须紧贴第 4 步执行,窗口内
#    新起 worker 可能复用同名网——rm 前重跑第 2 步)。

# 4. 核无附着容器(非 0 = 仍有沙箱容器在飞:一律不回收,等容器自然
#    退出后重来;孤儿容器的处置原语见 §1.2 注记)
docker network inspect -f '{{.Name}} {{len .Containers}}' <net>

# 5. 无监听、无活 owner、无附着即回收
docker network rm <net>
```

升级一次性泄漏:从 T15.4 前版本升级后,旧裸基段网(缺省
`braid-sbx-egress`,无实例段后缀)不再被新代码创建/复用/删除——按上列
同规则核活核附着后回收一次,让地址池记账从干净基线开始。

边界:同 daemon 双持同名网结构性不可能(幂等创建);列表中每个名字至多
一个活 owner;同 worker_id 双进程 = 后者 bind 拒启(显式端口部署契约,
见 deploy/README.md §9 运维;`EGRESS_PORT=0` dev/test 不执法),不产生
第二张网。

### 1.2 孤儿沙箱容器(T15.3)

沙箱容器打 `braid.sandbox.owner=<WorkerID>` label,worker 启动清扫只清
自家(全局清扫**无代码路径**——kill -9 孤儿、recreate 前身容器等跨
owner 回收是本节显式原语)。死实例身份可知:owner label 与
`node_attempt.worker_id` 对账面同源同词形。T20.4(D-t204)起启动清扫带
租约活性复核:同 owner 的 worker 租约在场(DB 判活,以复核时点
StartLease 之前在场为准;含 ≤TTL 残窗)⇒ 本次启动自动跳过清扫——活体
同 owner 冲突下不清活体 peer 的在飞沙箱;被跳过的孤儿(含 kill -9 或
SIGTERM release 失败残窗内重启的死者残容器)由本节 ② 按 owner 精确清。

```sh
# ① 枚举 + 归属判读:每只容器的 owner label 即归属身份(与 DB 的
#    node_attempt.worker_id 对账可核)。
docker ps -a --filter label=braid.sandbox=1 \
  --format '{{.ID}} {{.Label "braid.sandbox.owner"}} {{.Status}}'

# ② 按 owner 精确清(推荐:只动目标死者身份的容器)。filter 值整体加
#    引号再替换占位符——worker_id 若取自 DB(node_attempt),其值域在
#    off 模式 worker 下不受字符集约束,不过 ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$
#    的形态不可能是任何沙箱容器的主人(docker 模式启动闸挡死),按敌意
#    输入对待、勿裸贴进 shell。
docker ps -aq --filter label=braid.sandbox=1 \
  --filter "label=braid.sandbox.owner=<死实例 WorkerID>" | xargs -r docker rm -f

# ③ 全局清扫(最后手段:多实例同 daemon 时先确认无其他活体 worker)。
docker ps -aq --filter label=braid.sandbox=1 | xargs -r docker rm -f
```

升级路径的 recreate 孤儿(自管部署未钉 hostname 的形态:前身容器带旧
短 ID owner)同法回收——② 的 owner 取前身容器短 ID;交付 prod compose
已钉 hostname,recreate 前后身份一致,由新实例启动清扫自收。**首个
T15.3 升级窗特例**:旧二进制所起的前身容器没有 owner label,② 与启动
清扫都结构性够不到,只能用 ③(先确认无其他活体 worker),此后再无此
形态(详见 deploy/UPGRADE.md 注记)。

边界(D-t151 #8-③;T20.4/D-t204 起活性复核已转正为代码缓解,见本节
开头注记):手工同 hostname ×2 等活体同 owner 冲突的互杀主面已由复核
收口;残余面 = 复核时点之后才 acquire 的同 owner 新生成(以复核时点
在场为准)、残窗内重启的误跳过一次、复占双签发(归 T20.1 fencing)。
与 §1.1 泄漏网的耦合:附着容器非零的网一律不回收(§1.1 第 4 步),
先按本节清容器再回收网。

## 2. 调度手工原语(T17.3,T7.4 收口)

调度推进是事件驱动(scheduler.Instantiate 派发根节点、完成事件推进
后继);判死收敛是 reaper 的自动路径(T2.5:心跳陈旧 attempt 判 DEAD、
deadline 到期的 EXECUTING 预算内 RedispatchNode)。本节收**自动路径
结构性不可达时**的运维手工原语——正常态一律等自动路径,勿用本节。

### 2.1 RequeueNode:EXECUTING→PENDING 手工重入队

**适用**:EXECUTING 行卡死且 reaper 扫描够不到——`deadline_at IS NULL`
的不可扫描行(T2.5 扫描谓词对 NULL 恒假,数据缺陷形态)、reaper 长期
停摆窗内的数据修复。**红线:reaper 在场且 deadline 在期时禁用本节**——
重入队会造出 reaper 不扫的 PENDING 行(自动路径只认 EXECUTING),等于
亲手关掉自动恢复通道;等 reaper 收敛(RedispatchNode)才是正路。

原语 = `engine.RequeueNode`(internal/engine/nodecas.go;状态机唯一
EXECUTING→PENDING 边,D-t25#5「保留为 runbook 手工原语」)。同族辅助
原语 = `engine.CloseAttemptDead`(残留 RUNNING attempt 收口;不收口则
api 回滚的守卫② 拒 409 BlockedInflight)。**核验先行**(镜像 UPGRADE.md
§5 outbox DEAD 重置纪律):迟到的同 attempt 任务会被 worker 接管重跑
(claimAttempt 对 RUNNING 行的接管分支),不核验就重置 = 双烧。

```sql
-- ① 定位 + 读版本(所有 CAS 手术的裁决基础;0 行 = 状态已变,重读重判):
SELECT id, node_id, status, version, worker_id, deadline_at,
       attempts, attempts_base
  FROM node_execution WHERE id=<node_execution_id>;
-- 适用面自核:status='EXECUTING'(快照灭失不妨碍重派:重认领时
-- ClaimNode 同语句重写 input_snapshot,engine/snapshot.go 契约)。

-- ② 核验无活执行面(三查缺一不可):
--    a. attempt 侧:最近 attempt 非 RUNNING,或 RUNNING 但心跳陈旧
SELECT id, status, worker_id, heartbeat_at FROM node_attempt
 WHERE node_execution_id=<node_execution_id> ORDER BY id DESC LIMIT 1;
--    b. worker 侧:该 worker_id 无活体(容器/进程在不在,§1.2 同款核对);
--    c. 队列侧:asynq 无该任务活副本(task_id = att:<id>:<当前 attempts>)
SELECT id, task_id, status FROM outbox
 WHERE task_id=CONCAT('att:', <node_execution_id>, ':', <当前 attempts>);
-- redis-cli -h <queue-host> --scan --pattern 'asynq:*' | grep -F '<task_id>'
-- 非空 = 先处理 Redis 侧(等终态 / del),否则重入队后迟到任务撞接管分支。

-- ③ 残留 RUNNING attempt 手工收口(CloseAttemptDead 原语形态):
UPDATE node_attempt
   SET status='DEAD', error_code='manual_requeue',
       error_message='ops manual requeue (RUNBOOK §2.1)'
 WHERE id=<attempt_id> AND status='RUNNING';

-- ④ 重入队本体(RequeueNode 原语形态;CAS 带 status+version 双谓词,
--    全清形态:快照/租约/截止/非终局错误一并清,恢复干净 PENDING):
UPDATE node_execution
   SET status='PENDING', worker_id=NULL, input_snapshot=NULL, deadline_at=NULL,
       error_code=NULL, error_message=NULL, status_changed_at=NOW(3),
       version=version+1
 WHERE id=<node_execution_id> AND status='EXECUTING' AND version=<① 读到的版本>;
-- RowsAffected=0 = 并发方(迟到 Complete / reaper)先动了,回 ① 重读重判。
```

**再派发**(PENDING 无扫描者——这是 D-t25#5 不启用自动重入队的原因,
手工路径同样适用):重入队本身不触发任何调度。两条正路按场景选:

- 前驱未全终态:等前驱完成事件级联(advance 会扫到该 PENDING 后继)。
- 前驱全 SUCCEEDED 的冻结行:**api 回滚的自动解析**——执行落到 FAILED
  后 `POST /executions/{id}/rollback`(不带 target)按字典序取「前驱
  全就绪的 PENDING 冻结行」为目标(input_eval 型终败的 resume 面),
  其全图 advance 会带着新鲜快照重派本行。执行还在 RUNNING 的:先让
  失败收敛(reaper 判死 / 业务终败)进 FAILED 白名单,守卫② 要求全图
  零 EXECUTING/WAITING_APPROVAL 残留(本节 ②③ 已保证该行不阻塞)。

**留痕**:手工 SQL 不写 audit_log(append-only 面本就无 UPDATE 通道;
状态迁移经审计链的自动化路径才有审计行)。操作证据归运维通道:变更单、
操作日志、§2.1 步骤号引用。WAITING_APPROVAL 行不适用本节(归审批流
T4.3:超时 fail 策略或审批决定,手工改写会绕过审批事实)。

## 3. audit 分区水位:preflight 拒启 / 水位告警(T20.7)

**症状**:组件启动即退,日志带
`preflight: audit_log partition watermark <日期> does not cover today <日期>:
audit INSERTs would all fail (MySQL 1526)`;或 Prometheus
`AuditPartitionWatermarkLow`(余量 <3 天)/ `AuditPminRows`(p_min 落行)告警。

**机理**:audit_log 按日 RANGE 分区且**不设 MAXVALUE**(锁定决策:越界
1526 响亮失败 = 受监控条件)。静态分区(迁移 0001)只盖到 2026-10-04;
当日及未来分区由 reaper 内 janitor 每 tick 预建到 today+8——水位是
**DB 状态**,不是任何进程的状态。api/worker(审计写方)preflight 对
「水位不覆盖当日」硬拒;relay/reaper 仅 warn(水位自愈者,硬拒会死锁)。

**修复**(按错误消息自身指引,两选一):

```sh
# ① 正路:起 reaper(janitor 首 tick 即补当日分区并顶到 +8)
docker compose -f deploy/prod.docker-compose.yml up -d reaper
#    reaper 容器在跑但 unhealthy(进程残死/janitor 不收敛)时,up -d 是
#    no-op——改用 restart(重启即重跑 janitor 首轮):
#    docker compose -f deploy/prod.docker-compose.yml restart reaper
# ② 应急(无 reaper 可起时):手工补一条当日分区(与 janitor 输出同构)
#    错误消息里已带可直接执行的 ALTER 语句,照抄执行即可。
```

**安装口径(T20.7,D-t207)**:全部受管栈已钉序「reaper/janitor 先行」——
dev(dev-up.sh audit-ready 闸)/ prod·drill(compose reaper healthcheck
+ depends_on)/ helm(api/worker initContainer audit-ready -wait 120s)/
e2e(脚本等 janitor 日志)/ t13 测试栈(sqltest 前滚补分区,无进程竞态,
internal/db/sqltest/sqltest.go)。绕过口径(手起进程、单服务
`compose restart`——restart 不重放 depends_on)在水位健康时无害;水位
过期时确定性撞 preflight 硬拒,即本节症状,按上修。reaper 停摆 ≥8 天后
的任何重启,先手工判定(UPGRADE.md §2 的 audit-ready 命令)。

**p_min 告警**(独立判据):exec_date < 2026-01-01 的行落 p_min 兜底
分区——非零即写方时钟漂移或异常回填,核各写方宿主时钟与 DB 时钟偏差;
计数不自愈(append-only),核因后归档处置。
