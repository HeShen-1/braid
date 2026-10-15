#!/bin/sh
# braid 按组件授权(T7.4 定稿):deploy/README.md §5 矩阵的可执行真源。
# 由交付栈的 grants one-shot 服务在 **迁移完成之后** 执行(表级 GRANT 要求
# 表已在场——不能挂 mysql 的 initdb.d,那跑在迁移之前);已有数据卷的
# 开通/扩权/改密见 deploy/UPGRADE.md §3(同源语句,手工执行)。
# 密码从环境变量读取(deploy/.env 交付栈段);账号名与 compose 各服务的
# BRAID_MYSQL_DSN 一致。幂等:CREATE USER IF NOT EXISTS + GRANT 重跑 no-op
# (IF NOT EXISTS 形态不重设密码,改密走 ALTER USER,UPGRADE.md §3)。
# 断言对拍(T17.3):本文件的表级 GRANT 语句被 internal/preflight 的
# TestComponentGrantsParity / TestComponentGrantsSnapshot 解析消费——
# 组件断言面(componentRequired)与本文逐表全量 equality,改本文的表级
# 面必须同轮改断言面(否则测试红);第五账号 sandbox-exec 零 DB 面不建号
# (矩阵见 README §5)。
set -eu

# 密码必设(fail-fast 优于半配置栈:缺密码账号建不出来,组件起不来,栈首启即红):
: "${MYSQL_ROOT_PASSWORD:?MYSQL_ROOT_PASSWORD unset}"
: "${BRAID_DB_API_PASSWORD:?BRAID_DB_API_PASSWORD unset}"
: "${BRAID_DB_WORKER_PASSWORD:?BRAID_DB_WORKER_PASSWORD unset}"
: "${BRAID_DB_RELAY_PASSWORD:?BRAID_DB_RELAY_PASSWORD unset}"
: "${BRAID_DB_REAPER_PASSWORD:?BRAID_DB_REAPER_PASSWORD unset}"

# 密码字符集闸(对抗审计收口):下方 heredoc 展开把密码嵌进 SQL 单引号
# 字面量,密码含 ' 会断裂 SQL(fail-closed),含 $ 会被 sh 单趟展开篡改
# (存储密码与配置静默分叉)。限安全集,越界即拒。
for v in "$BRAID_DB_API_PASSWORD" "$BRAID_DB_WORKER_PASSWORD" \
         "$BRAID_DB_RELAY_PASSWORD" "$BRAID_DB_REAPER_PASSWORD"; do
	case "$v" in
	*[!A-Za-z0-9._~@+%-]*)
		echo "grants.sh: password has characters outside [A-Za-z0-9._~@+%-]; refusing (SQL/shell literal safety)" >&2
		exit 1
		;;
	esac
done

# MYSQL_HOST:独立容器(grants one-shot)设 mysql;容器内/手工 exec 场景
# 缺省 localhost。
mysql -h"${MYSQL_HOST:-localhost}" -uroot -p"$MYSQL_ROOT_PASSWORD" <<SQL
CREATE USER IF NOT EXISTS 'braid_api'@'%'    IDENTIFIED BY '${BRAID_DB_API_PASSWORD}';
CREATE USER IF NOT EXISTS 'braid_worker'@'%' IDENTIFIED BY '${BRAID_DB_WORKER_PASSWORD}';
CREATE USER IF NOT EXISTS 'braid_relay'@'%'  IDENTIFIED BY '${BRAID_DB_RELAY_PASSWORD}';
CREATE USER IF NOT EXISTS 'braid_reaper'@'%' IDENTIFIED BY '${BRAID_DB_REAPER_PASSWORD}';

-- api(写面 = 定义/执行 CRUD、审批事实、同步审计直写、登录审计、outbox 发射)
GRANT SELECT, INSERT, UPDATE ON braid.workflow_definition TO 'braid_api'@'%';
GRANT SELECT, INSERT, UPDATE ON braid.workflow_execution TO 'braid_api'@'%'; -- UPDATE = 回滚/续跑缝的 RestartExecution 状态翻转(engine wfcas)+ 锁定读面,同 node_execution 注记
-- node_execution 的 UPDATE 是锁定读面(T15.6 演练实证):MySQL 锁定读
-- (SELECT ... FOR UPDATE)要求对应写权限——审批决定事务的 TOCTOU 守卫
-- (approvalstore.lockApprovalNode)与 rollback/resume 缝(engine CAS +
-- rollback.go 的 node_attempt 锁读)都在 api 连接上执行;无 UPDATE =
-- least-privilege 部署下 1142 全红(t13 测试 root 连接不可见)。
GRANT SELECT, INSERT, UPDATE ON braid.node_execution     TO 'braid_api'@'%';
GRANT SELECT, UPDATE         ON braid.node_attempt      TO 'braid_api'@'%'; -- 回滚缝锁读 + CAS(同上;api 无 INSERT 面)
GRANT SELECT, INSERT          ON braid.approval_decision  TO 'braid_api'@'%';
GRANT SELECT, INSERT          ON braid.audit_log          TO 'braid_api'@'%';
GRANT SELECT, INSERT, UPDATE (last_id, last_row_hash) ON braid.chain_heads TO 'braid_api'@'%'; -- T9.3 哈希链锚:列级最小授权——api 写路径只推链尾两列;闩/水位列(broken_*/verified_*)不可写 = 「闩复位=人工」的权限面承载
GRANT INSERT                  ON braid.login_audit        TO 'braid_api'@'%';
-- audit_pending(回滚缝,T15.6 演练臂 ⑦ 实证):rollback/resume 重派节点的
-- node.state_changed 属热路径批量事件族(T6.1),走 Enqueue 暂存由 reaper
-- flush 搬运——api 在回滚事务内 INSERT 暂存行(消费半边归 reaper,矩阵
-- 的 api INSERT 与 worker 同款)。
GRANT INSERT                  ON braid.audit_pending      TO 'braid_api'@'%';
-- login_throttle(T11.3):登录防御状态簿记面——api 行锁单事务读-判-写
-- (成功重置走 UPDATE,无 DELETE;UPDATE 列级收窄:kind 仅 INSERT 写,
-- chain_heads 列级先例);reaper janitor 超龄回收(DELETE 谓词列需
-- SELECT,login_audit 同注记)。
GRANT SELECT, INSERT, UPDATE (window_start, count, locked_until, updated_at) ON braid.login_throttle TO 'braid_api'@'%';
-- snowflake_node_lease(T15.2,D-t151 决策 2):节点号租约仲裁面——
-- acquire INSERT / 复占与过期接管 UPDATE / auto 池扫描 SELECT / 优雅
-- 停机释放 DELETE(DELETE 是设计语义;表是运行态簿记非业务事实,不在
-- append-only 断言表集,preflight 台账预登记)。
GRANT SELECT, INSERT, UPDATE, DELETE ON braid.snowflake_node_lease TO 'braid_api'@'%';
GRANT SELECT                  ON braid.users              TO 'braid_api'@'%';
GRANT SELECT                  ON braid.user_role          TO 'braid_api'@'%';
GRANT INSERT                  ON braid.outbox             TO 'braid_api'@'%';
GRANT SELECT                  ON braid.schema_migrations  TO 'braid_api'@'%';

-- worker(写面 = 执行引擎状态迁移 CAS、attempt 领取、异步审计暂存、outbox
-- 发射;INSERT 面 = node_attempt + audit_pending + outbox + node_execution。
-- api Instantiate 建初始行,worker 的动态 foreach 在派发位补落实例行)
GRANT SELECT                  ON braid.workflow_definition TO 'braid_worker'@'%';
GRANT SELECT, UPDATE          ON braid.workflow_execution  TO 'braid_worker'@'%';
GRANT SELECT, INSERT, UPDATE  ON braid.node_execution      TO 'braid_worker'@'%';
GRANT SELECT, INSERT, UPDATE  ON braid.node_attempt        TO 'braid_worker'@'%';
GRANT SELECT                  ON braid.approval_decision   TO 'braid_worker'@'%';
GRANT SELECT, INSERT          ON braid.audit_log           TO 'braid_worker'@'%';
GRANT INSERT                  ON braid.audit_pending       TO 'braid_worker'@'%';
GRANT SELECT, INSERT, UPDATE (last_id, last_row_hash) ON braid.chain_heads TO 'braid_worker'@'%'; -- T9.3:列级最小授权同 api(写路径只推链尾两列)
GRANT INSERT                  ON braid.outbox              TO 'braid_worker'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON braid.snowflake_node_lease TO 'braid_worker'@'%'; -- T15.2 租约仲裁(同 api 四权限面,注释见上)
GRANT SELECT                  ON braid.users               TO 'braid_worker'@'%';
GRANT SELECT                  ON braid.user_role           TO 'braid_worker'@'%';
GRANT SELECT                  ON braid.schema_migrations   TO 'braid_worker'@'%';

-- relay(写面 = outbox 投递状态迁移;零审计写——audit_log 无权限是设计而非遗漏)
GRANT SELECT, UPDATE          ON braid.outbox            TO 'braid_relay'@'%';
GRANT SELECT                  ON braid.schema_migrations TO 'braid_relay'@'%';

-- reaper(写面 = 判死重派 INSERT outbox / 审计搬运 / 分区水位 / 保留期清理;
-- UPDATE 面 = 状态迁移 CAS,INSERT 面 = outbox 重派 + audit_pending 的
-- parallel 实例耗尽收口事件。audit_log 的 DROP 仅服务于 DROP
-- PARTITION——MySQL 对该语句要求 ALTER+DROP 双权限,缺 DROP 则 janitor
-- 回收路径 1142;D-t73 preflight 断言面是 UPDATE/DELETE,DROP 权限不触发拒启)
GRANT SELECT                  ON braid.workflow_definition TO 'braid_reaper'@'%';
GRANT SELECT, UPDATE          ON braid.workflow_execution  TO 'braid_reaper'@'%';
GRANT SELECT, UPDATE          ON braid.node_execution      TO 'braid_reaper'@'%';
GRANT SELECT, UPDATE, DELETE  ON braid.node_attempt        TO 'braid_reaper'@'%'; -- T18.6 留档保留期清理:终态行批量 DELETE(谓词列 status/heartbeat_at/execution_id 走既有 SELECT,同 login_audit 先例);活执行守卫在本域只读 workflow_execution
GRANT SELECT                  ON braid.approval_decision   TO 'braid_reaper'@'%';
GRANT SELECT, INSERT, ALTER, DROP ON braid.audit_log       TO 'braid_reaper'@'%';
GRANT SELECT, INSERT, DELETE  ON braid.audit_pending       TO 'braid_reaper'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON braid.chain_heads  TO 'braid_reaper'@'%'; -- T9.3:同步 Append 发射面(attempt.reaped 等)head 懒建 + 校验作业水位/断链闩 UPDATE + janitor 分区回收级联解除链义务(DELETE)
GRANT SELECT, INSERT, DELETE  ON braid.outbox              TO 'braid_reaper'@'%';
-- login_audit 的 SELECT 服务 DELETE 的 WHERE 谓词列(event_at 保留期条件);
-- 无 SELECT 则 purge 报 1143(演练实证,MySQL 权限规则:UPDATE/DELETE 的
-- 谓词列需要 SELECT)。
GRANT SELECT, DELETE          ON braid.login_audit         TO 'braid_reaper'@'%';
GRANT SELECT, DELETE          ON braid.login_throttle      TO 'braid_reaper'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON braid.snowflake_node_lease TO 'braid_reaper'@'%'; -- T15.2 租约仲裁(同 api 四权限面,注释见上)
GRANT SELECT                  ON braid.schema_migrations   TO 'braid_reaper'@'%';
SQL
