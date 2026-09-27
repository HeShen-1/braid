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
5. 验证:api /healthz 200;四组件启动日志无 preflight 红;指标面正常
```

迁移命令(交付栈内经 compose 内网;`multiStatements=true` 是 golang-migrate
执行单文件多语句迁移的前提,缺它 0001 直接红):

```sh
docker compose -f deploy/prod.docker-compose.yml run --rm migrate
# 等价的手工形态:
migrate -path internal/db/migrations \
  -database "mysql://root:<pwd>@tcp(<host>:3306)/braid?multiStatements=true" up
```

compose 的 `migrate` one-shot 服务已挂上述命令(`deploy/prod.docker-compose.yml`),
组件服务以 `depends_on: condition: service_completed_successfully` 排在其后:
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

## 3. 按组件账号授权

**矩阵**(四账号 × 全表最小授权)与设计说明见 `deploy/README.md §5`;
**可执行语句真源 = `deploy/grants.sh`**。三条路径:

- **首装**(交付栈数据卷首次初始化):`prod.docker-compose.yml` 的 mysql
  服务把 grants.sh 挂进 `/docker-entrypoint-initdb.d` 自动执行,零手工。
- **已有数据卷的升级**(交付栈从旧版升级、或自管部署):以 root 手工执行
  grants.sh 同源语句:

  ```sh
  docker exec -i braid-prod-mysql sh -c 'cd /docker-entrypoint-initdb.d && sh grants.sh' < /dev/null
  # 或脱离 compose:sh deploy/grants.sh(读 deploy/.env 同名环境变量)
  ```

  语句幂等(`CREATE USER IF NOT EXISTS` + `GRANT` 重跑 no-op);版本升级
  带来新写面时(如 T7.4 的 login_audit),重新执行即完成增量扩权。
- **改密**:`ALTER USER 'braid_api'@'%' IDENTIFIED BY '<new>';` 后同步改
  `deploy/.env` 并滚动重启对应组件(IF NOT EXISTS 形态建账号不重设密码)。

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
migrate ... force N
migrate ... up
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
