# braid 升级手册(T7.4 交付)

适用范围:交付栈(`deploy/prod.docker-compose.yml`)与同形态自管部署的版本
升级、账号开通、故障恢复。配额与容量口径见 `deploy/README.md`(数字同源
声明以代码为准);本文只管「怎么安全地换版本、换出问题怎么收」。

## 0. 版本语义

- **版本锚 = 迁移版本**(`schema_migrations.version`,当前 0005)。交付包 =
  仓库:四入口二进制(Dockerfile)+ 迁移文件(`internal/db/migrations`)。
  二进制内嵌同套迁移(`internal/db` embed),`preflight.CheckSchemaVersion`
  在组件启动时比对两侧,版本不一致拒启或 warn(见 §2)。
- 生产迁移走 **golang-migrate CLI**(短连接,不走进程池——`deploy/README.md
  §1` 预留口径);braid 进程自身从不执行迁移。

## 1. 标准升级序

单副本、允许短停(P0 形态;无停机窗的滚动序见 §2):

```
1. 停组件:docker compose -f deploy/prod.docker-compose.yml stop api worker relay reaper
2. 备份:数据卷快照或 mysqldump 全库(审计铁律:audit_log 只增,备份含全库)
3. 迁移:golang-migrate up(命令见下)
4. 起组件:docker compose -f deploy/prod.docker-compose.yml up -d
5. 验证:api /healthz 200(docker exec braid-prod-api wget -qO-
   http://127.0.0.1:8081/healthz);四组件启动日志无 preflight 红;指标面正常
```

**取舍记录(容器级 healthcheck)**:交付栈只给 mysql/redis 配容器级
healthcheck(compose 依赖条件需要);braid 组件不配——存活面 = /healthz +
metrics exposition 由外部监控探(镜像内 busybox wget 亦可人工探,
`docker exec <容器> wget -qO- http://127.0.0.1:9090/metrics`)。

**recreate 孤儿(T15.3)**:第 4 步 `up -d` 对变更配置的服务做 stop →
recreate。交付 prod compose 的 worker 已钉 `hostname`(owner 身份跨
recreate 稳定),新实例启动清扫按 owner 自收前身残留;自管部署若沿用
compose 缺省 hostname(容器短 ID),recreate 必换身份,前身在飞沙箱容器
成为带旧 owner 的孤儿——按 deploy/RUNBOOK.md §1.2 的按-owner 回收原语
处理,不要用全局清扫(同 daemon 多 worker 时会误伤)。**首个 T15.3
升级窗特例**:旧二进制所起的前身容器没有 owner label,按-owner 过滤与
启动清扫都结构性地够不到它们,只能用 ③ 全局原语(先确认同 daemon 无
其他活体 worker),此后再无此形态。

迁移命令(交付栈内经 compose 内网;`multiStatements=true` 是 golang-migrate
执行单文件多语句迁移的前提,缺它 0001 直接红):

```sh
docker compose -f deploy/prod.docker-compose.yml run --rm migrate   # 默认 command ["up"]
# 等价的手工形态:
migrate -path internal/db/migrations \
  -database "mysql://root:<pwd>@tcp(<host>:3306)/braid?multiStatements=true" up
```

compose 的 `migrate` one-shot 服务已参数化(entrypoint 透传 command):
`run --rm migrate force N` 即 dirty 恢复形态(§4)。组件服务以
`depends_on: condition: service_completed_successfully` 排在其后:
`up -d` 即按「迁移完成 → 组件启动」的顺序收敛。

## 2. N-1 兼容窗口

**定义**:数据库版本 > 二进制内嵌迁移版本(先迁移、后换二进制)时,旧二进制
可继续运行——`preflight.CheckSchemaVersion` 对 `schema > binary` 仅 warn
(`schema newer than binary (N-1 window)`),不拒启。

**依据(不变量)**:迁移只增量——建新表(0004 `audit_pending`、0005
`login_audit`)、加唯一键(0003 `uk_node_id_exec`);旧二进制不读不写新对象,
新表/新键对旧语句透明。滚动升级序因此合法:

```
1. golang-migrate up(旧组件仍在跑,窗口内混跑是设计内状态,不是异常)
2. 逐组件换二进制并重启(建议 reaper 先起:分区水位/审计搬运尽恢复;
   api 最后:入口面恢复即对外宣告完成)
3. 全部组件换新后窗口关闭
```

**维护纪律**:未来迁移若破坏该不变量(改既有列语义、重命名、删对象),必须
两阶段(先增后删、间隔一个发布窗),并在 `docs/DECISIONS.md` 登记决策——
单阶段破坏性迁移会让窗口内旧二进制行为未定义。

**T15.2 首次升级例外(一次性;0012 租约表落地窗)**:上述滚动序在
「旧二进制 = 静态节点号、新二进制 = 租约 auto」配对上产生**确定性撞号
窗**——旧静态持有者(1/2/3)在 `snowflake_node_lease` 无行,新副本的
auto 线性取号看不见它们;k8s RollingUpdate 新旧 pod 共存窗内,新副本
取 0/1/2 即与旧 api=1 / worker=2 同号双活,症状 = 业务表 PK 1062 响亮
写入中止(不损已提交数据)。因此**首次升级到 T15.2、以及自 T15.2 回滚
到旧版,必须先排空旧 pod**(临时 `--replicas=0`,或该次发布对 Deployment
用 Recreate 策略)再换二进制;「迁移先行、滚动换代」的混跑合法性自
**两端都是租约二进制**起恢复(后续升级无此条款)。compose 单副本
`stop` → `up -d` 天然排空,不受影响。

## 3. 按组件账号授权

**矩阵**(五账号 × 全表最小授权:四个 MySQL 业务账号 + sandbox-exec
零 DB 面)与设计说明见 `deploy/README.md §5`;
**可执行语句真源 = `deploy/grants.sh`**。三条路径:

- **首装**(交付栈):`prod.docker-compose.yml` 的 **grants one-shot 服务**
  在 migrate 完成后自动执行(表级 GRANT 要求表在场,不能挂 mysql 的
  initdb.d——那跑在迁移之前,1146;§6 演练教训 1)。重新 `up -d` 会幂等
  重跑,零手工。
- **已有数据卷的升级**(交付栈从旧版升级、或自管部署):

  ```sh
  docker compose -f deploy/prod.docker-compose.yml run --rm grants
  # 脱离 compose 的自管部署:sh deploy/grants.sh(需在环境里提供
  # MYSQL_ROOT_PASSWORD / MYSQL_HOST 及四账号密码;语句幂等)
  ```

  语句幂等(`CREATE USER IF NOT EXISTS` + `GRANT` 重跑 no-op);版本升级
  带来新写面时(如 T7.4 的 login_audit;T15.2 的 snowflake_node_lease
  ——三账号 INSERT/UPDATE/SELECT + 优雅释放 DELETE,D-t151 决策 2),
  重新执行即完成增量扩权。
- **改密**:`ALTER USER 'braid_api'@'%' IDENTIFIED BY '<new>';` 后同步改
  `deploy/.env` 并滚动重启对应组件(IF NOT EXISTS 形态建账号不重设密码)。
  密码字符集限 `[A-Za-z0-9._~@+%-]`(grants.sh 的 SQL/shell 字面量安全闸)。

**验证**:组件启动即自检——`preflight.CheckAuditAppendOnly` 对授权违约
(schema 级 DELETE、业务账号 UPDATE/DELETE)拒启并给出可执行的 REVOKE
修复提示;漏授权的运行期症状是 1142 响亮报错(见 README §5 末条)。

## 4. dirty 中断恢复 runbook

**现象**:组件拒启,报 `preflight: schema_migrations dirty at version N
(previous migration interrupted); fix: ... migrate force N and re-run up`。

**golang-migrate force 语义**:

- `migrate force N`:把 `schema_migrations` 钉到版本 N 并清 dirty。此后
  `up` 从 **N+1** 继续,不重放 N 本身。
- `migrate force N-1` 后 `up`:重放 N 本身。

**重放安全性现状核验**(2026-09-27,本手册交付时点):

| 迁移 | 对象 | 重放安全? |
|------|------|-----------|
| 0001 / 0002 / 0004 / 0005 | 全部 `CREATE TABLE IF NOT EXISTS`(down 全 `DROP IF EXISTS`) | 是,重放 no-op |
| 0003 | `approval_decision` 建表 | 是(IF NOT EXISTS) |
| 0003 | `ALTER TABLE node_execution ADD UNIQUE KEY uk_node_id_exec` | **否**:MySQL 8 无 `ADD UNIQUE KEY IF NOT EXISTS` 语法,重放报 1061(duplicate key name) |

即「迁移对象全 IF NOT EXISTS、重跑安全」对本仓**不完全成立**:0003 的
ALTER 是唯一例外。恢复分两臂:

**臂 A:版本 N 的对象已全部在场**(中断发生在版本号落库前一步)——钉版本
跳过重放:

```sh
# 先核对 N 的每个对象在场(以 0003 为例):
#   SHOW CREATE TABLE node_execution;        # uk_node_id_exec 在?
#   SELECT COUNT(*) FROM information_schema.tables
#    WHERE table_schema=DATABASE() AND table_name='approval_decision';
# 交付栈(migrate one-shot 参数化,command 覆盖即 force 形态):
docker compose -f deploy/prod.docker-compose.yml run --rm migrate force N
docker compose -f deploy/prod.docker-compose.yml run --rm migrate   # = up
# 自管部署的手工等价:
migrate -path internal/db/migrations \
  -database "mysql://root:<pwd>@tcp(<host>:3306)/braid?multiStatements=true" force N
```

**臂 B:版本 N 部分应用(有对象缺失)**:

- 缺失对象少:从 N 的 `up.sql` 摘出缺失的那几条 DDL 手工执行,然后走臂 A。
- 或 `force N-1` + `up` 重放 N:IF NOT EXISTS 对象 no-op 安全;但含 ALTER 的
  迁移(现仅 0003)重放前先核对 ALTER 对象在场——在场则摘除该条再重放,
  否则 1061 循环。

**验证**:`SELECT version, dirty FROM schema_migrations;` → `(最新版, 0)`;
起组件,preflight 全绿。

**预防**:迁移一律经 compose 的 migrate one-shot 服务(或等价容器化执行),
不手工裸跑——容器原子性是 MySQL 非事务 DDL 下最简的收敛面。

## 5. outbox DEAD 重置规程(T2.2 移交回收)

**背景**:relay 投递尝试穷尽 → outbox 行转 DEAD(`internal/outbox/status.go`
MarkDead;T6.4 死信告警源)。**DEAD ≠ 未投递**:超时误报路径可造出
「DEAD 行 + Redis 仍持有活任务」的真相分裂——不经核验直接重置,会在 Redis
侧任务还活着时让 relay 重投同 task_id(撞 ErrTaskIDConflict,或更糟:语义
双活)。**核验先行,三步不可省略**:

```sh
# ① 反查事件行(idx_outbox_task),确认为 DEAD、读 last_error 定因:
SELECT id, execution_id, event_type, task_id, deliver_attempts,
       last_error, updated_at
  FROM outbox WHERE task_id='<task_id>';

# ② 核验队列 Redis 无该 task_id 记录(活任务体或 archived 成员):
redis-cli -h <queue-host> --scan --pattern 'asynq:*' | grep -F '<task_id>'
# 空输出 = Redis 侧无记录 ⇒ 可安全重置;
# 非空(典型 asynq:{<queue>}:t:<task_id> 任务体键)⇒ 先处理 Redis 侧
# (等终态 / del 任务体),否则重置后 relay 重投即撞 ErrTaskIDConflict。

# ③ 重置(运维手术面;带 DEAD 谓词防误伤,available_at 置当前 ⇒ relay
#    主扫谓词 available_at<=NOW(3) 立即可拾;deliver_attempts 不清零,
#    保留尝试史——再穷尽会再转 DEAD,T6.4 告警兜底):
UPDATE outbox SET status='PENDING', available_at=NOW(3)
 WHERE task_id='<task_id>' AND status='DEAD';
```

**观察**:relay 下一轮扫描即投递(默认轮询间隔);`braid_outbox_*` 指标或
Asynqmon 确认行转 DISPATCHED。

## 6. 升级演练记录(2026-09-27)

对上一版本数据卷完成一次完整升级演练(N-1 兼容窗口实证,验收臂)。

**版本锚**:旧 = `c9f9d76^`(T6.4 前:0004 迁移 + 旧四入口二进制);新 = 本
交付(T7.4:0005 + preflight 扩面 + 交付栈)。数据卷全程同一卷(升级 =
卷上推进,不重建)。

**三阶段实录**:

1. **旧世界起栈**:旧镜像 + 旧迁移(migrate 到 0004)+ root DSN(T7.4 前
   无按组件授权面,root 是历史真实形态;preflight 对 root 走管理平面
   warn)。四组件全绿;手工钉入证据行 `audit_log(drill.pre.0004)`、
   `outbox(drill:pre-0004, DISPATCHED)`。
2. **N-1 窗口实证**:旧组件**不停**,跑新迁移到 0005(0005 建 login_audit);
   强制重启旧二进制 → 四组件照常起,启动日志逐个出现
   `WARN preflight: schema newer than binary (N-1 window) schema_version=5
   binary_version=4`——「迁移先行、滚动换代」的窗口内混跑合法,实证。
3. **升级换代**:停旧栈(卷保留)→ 新形态起栈(migrate no-op + grants
   one-shot 建四账号 + 新镜像四组件)。

**验证矩阵**:

| 验证点 | 结果 |
|--------|------|
| 旧数据行在场(audit_log / outbox) | ✅ 逐行核对 |
| 四组件账号建立(braid_api/worker/relay/reaper) | ✅ |
| 四账号最小授权下 preflight 全绿(CheckAuditAppendOnly 扩面断言真实通过) | ✅ 零 ERROR |
| 0005 新表面工作(登录尝试 → login_audit `login.failed` 行 + 401) | ✅ |
| reaper 保留期清理路径(janitor 跨 tick) | ✅ 修 SELECT 后零错 |

**演练抓到并修复的两个真问题**(交付前收敛,演练价值所在):

1. grants 初版挂 mysql `initdb.d` → 表级 GRANT 先于迁移执行,表不存在
   1146。修正:grants 独立 one-shot 服务,排在 migrate 之后(栈序:建库 →
   迁移建表 → 表级授权 → 组件起)。
2. reaper 对 login_audit 仅授 DELETE → 保留期清理报 1143:MySQL 对带
   WHERE 的 DELETE 要求谓词列(event_at)有 SELECT。修正:矩阵与
   grants.sh 补 `SELECT,DELETE`(README §5 已登记通则)。

**结论**:0004 → 0005 数据卷升级路径(§1 升级序 + §3 授权)演练通过。

## 7. 雪花租约清行应急(T15.2 kill -9 残窗)

**背景**:节点号已租约池化(0012 `snowflake_node_lease`,D-t151 决策 2)。
正常停机(SIGTERM)组件自释放租约(DELETE);**kill -9 / 宿主灭失不留
释放机会**——行留到 `expires_at`(TTL 缺省 30s)后被下一 claimant 懒回收
接管,残窗 ≤ TTL 有界。应急清行只在两种情况需要:

1. **显式号被拒启且等不及残窗**(错误形如 `node lease held by another
   holder: node 2 by api:box-3 until ...`):
2. **行脏了**(如迁移/演练后的幽灵行,`expires_at` 异常)。

**步骤**(MySQL 端执行;先诊断后清行,清行 = 显式运维决策。**第一步必设
会话时区**:`expires_at` 存 UTC 字面值,braid 进程的连接契约是
`time_zone='+00:00'`,但手工客户端缺省跟随宿主时区——不设则 `NOW(3)`
按本地时区求值,过期判定系统性误报,可能误清活租约):

```sql
-- 0. 会话时区对齐连接契约(braid 进程侧同款;不设 = 误清活体面)
SET time_zone = '+00:00';

-- 诊断:谁持有哪些号、何时到期(过期判定全 DB 端 NOW(3))
SELECT node_id, kind, holder_id, expires_at,
       expires_at < NOW(3) AS expired
FROM braid.snowflake_node_lease ORDER BY node_id;

-- 清行:只清确认已死实例的行(holder = kind:<实例身份>——worker 侧
-- = BRAID_WORKER_ID(T20.4 起与 sandbox owner 同源),api/reaper =
-- hostname;以诊断 SELECT 回显的 holder_id 为准,勿手拼;勿清活体——
-- 清活体 = 制造双持窗)
DELETE FROM braid.snowflake_node_lease
WHERE node_id = <N> AND holder_id = '<kind>:<hostname>';
```

**边界如实登记**(D-t151 决策 8-①):全 DB 端时钟只排除时钟漂移路径;
MySQL 异步 failover 尾吞授予/续期 + 进程冻结的 zombie 双持窗存在(症状 =
业务表 PK 1062 响亮写入中止,不损已提交数据),半同步收紧、fencing
token 登记后续,不随 T15.2 实施。**同宿主同类双进程**(裸机手工起两个
同类进程且共享 hostname ⇒ holder_id 相同):仲裁无法区分「本进程重启
复航」与「另一活实例」,双双通过复占持有同号(决策 8-③ 活体同 owner
冲突同族)——holder 唯一性是部署契约(k8s pod 名/compose 容器名天然
满足),裸机多实例须逐实例显式 `BRAID_*_SNOWFLAKE_NODE` 或独立 hostname。
另:**自 T15.2 回滚到旧版**的镜像共存窗(旧静态号 pod 起来时新副本租约
行仍在续期)与 §2 首次升级例外同构——回滚同样先排空,见 §2。

## 8. mode=all 会签混跑窗(人工纪律 + 启动期硬闸,T15.6)

**背景**(D-t142①/D-t151 决策 6):会签 `mode=all` 语义由三方新逻辑共同
支撑——api 定局守卫 / worker 续跑复核 / reaper 已定局判定。滚动混跑窗内
旧二进制对 mode=all 挂起等价 any:旧 api **首票即定局**(1/N 票
SUCCEEDED,否决权静默剥夺,不可逆)、旧 worker 无定局复核直接收条
premature 事件、旧 reaper 对部分票节点空转重投且永不 escalate。

**人工纪律(承载首个落地窗)**:api/worker/reaper **全量升级完成后**,
才发布/触发 mode=all 工作流;混跑窗内已挂起的 all 节点属已知破坏窗
(worker 侧定局复核响亮拒保住正确性,事件死信为预期噪音,勿告警升级)。

**启动期能力硬闸(T15.6 落地)**:三类实例 acquire 雪花租约时宣告
`approval_mode_all` 能力(`snowflake_node_lease.capabilities`);api 在
**发布/触发含 mode=all 的工作流**前查「在场实例(api/worker/reaper 三类,
未过期)全部宣告」,在场未宣告即拒(503 `fleet_capability_gate`;**发布面** body 带未宣告
holder 清单,触发面仅分类码——明细在结构化日志与指标;在场查询失败同拒
——误放 < 误拒)。拒绝痕迹 =
结构化 ERROR 日志 + `braid_api_fleet_gate_rejections_total{reason}` 计数器
(audit_log 是执行锚定域,物化前拒绝结构性落不进,零迁移不另建表)。
观测:`max(rate(...)) > 0` 即混跑窗存在的值守信号。

**闭合度如实登记(不高估)**:
- 硬闸只对「租约表全舰队上线后」的后续升级生效。**首个落地窗**:旧二进制
  不写租约表,闸看到的在场 = 新副本子集——旧 api 放行(它没有闸代码),
  该窗仍由上方人工纪律承载;
- **LB 路由旧 api = 零闸放行**(旧进程内无闸代码):多 api 副本滚动窗内
  请求打到旧副本不受保护,同由人工纪律承载;
- 判定后旧副本上线的**残余窗**同理;
- 某 kind 零在场 = vacuous 放行(停摆 ≠ 破坏,发布序纪律兜底);
- 回滚到旧二进制的残窗:旧(T15.2 代)acquire 三条写路径硬编码
  `capabilities = NULL`——旧副本重启即把行宣告抹回 NULL,闸看到的在场
  未宣告 → **误拒**(fail-closed 方向安全,升级回新版本即解);真正的
  残留风险是「行未过期 + 旧副本未重启」的窗口里宣告仍在(误放方向),
  由下方回滚前置 SELECT 承载。

**回滚前置校验(D-t142②,硬闸是前置防线不是替代,保留)**:回退到
mode=all 三方语义升级前的版本前必须执行——

```sql
SELECT COUNT(*) FROM node_execution
 WHERE status='WAITING_APPROVAL'
   AND JSON_UNQUOTE(JSON_EXTRACT(input_snapshot,'$.mode'))='all';
```

计数非 0 时先人工促票齐或明示接受破坏窗(旧判据 × 多票数据 = 部分票
节点在回滚后首个续跑复核窗按首票定局),否则不回退。
