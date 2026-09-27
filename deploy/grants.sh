#!/bin/sh
# braid 按组件授权(T7.4 定稿):deploy/README.md §5 矩阵的可执行真源。
# 由交付栈的 grants one-shot 服务在 **迁移完成之后** 执行(表级 GRANT 要求
# 表已在场——不能挂 mysql 的 initdb.d,那跑在迁移之前);已有数据卷的
# 开通/扩权/改密见 deploy/UPGRADE.md §3(同源语句,手工执行)。
# 密码从环境变量读取(deploy/.env 交付栈段);账号名与 compose 各服务的
# BRAID_MYSQL_DSN 一致。幂等:CREATE USER IF NOT EXISTS + GRANT 重跑 no-op
# (IF NOT EXISTS 形态不重设密码,改密走 ALTER USER,UPGRADE.md §3)。
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
GRANT SELECT, INSERT          ON braid.workflow_execution TO 'braid_api'@'%';
GRANT SELECT, INSERT          ON braid.node_execution     TO 'braid_api'@'%';
GRANT SELECT, INSERT          ON braid.approval_decision  TO 'braid_api'@'%';
GRANT SELECT, INSERT          ON braid.audit_log          TO 'braid_api'@'%';
GRANT INSERT                  ON braid.login_audit        TO 'braid_api'@'%';
GRANT SELECT                  ON braid.users              TO 'braid_api'@'%';
GRANT SELECT                  ON braid.user_role          TO 'braid_api'@'%';
GRANT INSERT                  ON braid.outbox             TO 'braid_api'@'%';
GRANT SELECT                  ON braid.schema_migrations  TO 'braid_api'@'%';

-- worker(写面 = 执行引擎状态迁移 CAS、attempt 领取、异步审计暂存、outbox
-- 发射;INSERT 面 = node_attempt + audit_pending + outbox——execution/node
-- 行由 api 的 Instantiate 建,worker 只迁移状态)
GRANT SELECT                  ON braid.workflow_definition TO 'braid_worker'@'%';
GRANT SELECT, UPDATE          ON braid.workflow_execution  TO 'braid_worker'@'%';
GRANT SELECT, UPDATE          ON braid.node_execution      TO 'braid_worker'@'%';
GRANT SELECT, INSERT, UPDATE  ON braid.node_attempt        TO 'braid_worker'@'%';
GRANT SELECT                  ON braid.approval_decision   TO 'braid_worker'@'%';
GRANT SELECT, INSERT          ON braid.audit_log           TO 'braid_worker'@'%';
GRANT INSERT                  ON braid.audit_pending       TO 'braid_worker'@'%';
GRANT INSERT                  ON braid.outbox              TO 'braid_worker'@'%';
GRANT SELECT                  ON braid.users               TO 'braid_worker'@'%';
GRANT SELECT                  ON braid.user_role           TO 'braid_worker'@'%';
GRANT SELECT                  ON braid.schema_migrations   TO 'braid_worker'@'%';

-- relay(写面 = outbox 投递状态迁移;零审计写——audit_log 无权限是设计而非遗漏)
GRANT SELECT, UPDATE          ON braid.outbox            TO 'braid_relay'@'%';
GRANT SELECT                  ON braid.schema_migrations TO 'braid_relay'@'%';

-- reaper(写面 = 判死重派 INSERT outbox / 审计搬运 / 分区水位 / 保留期清理;
-- UPDATE 面 = 状态迁移 CAS,INSERT 面 = outbox 重派,audit_pending 只 SELECT+DELETE
-- ——暂存发射(INSERT)在 worker 路径。audit_log 的 DROP 仅服务于 DROP
-- PARTITION——MySQL 对该语句要求 ALTER+DROP 双权限,缺 DROP 则 janitor
-- 回收路径 1142;D-t73 preflight 断言面是 UPDATE/DELETE,DROP 权限不触发拒启)
GRANT SELECT                  ON braid.workflow_definition TO 'braid_reaper'@'%';
GRANT SELECT, UPDATE          ON braid.workflow_execution  TO 'braid_reaper'@'%';
GRANT SELECT, UPDATE          ON braid.node_execution      TO 'braid_reaper'@'%';
GRANT SELECT, UPDATE          ON braid.node_attempt        TO 'braid_reaper'@'%';
GRANT SELECT                  ON braid.approval_decision   TO 'braid_reaper'@'%';
GRANT SELECT, INSERT, ALTER, DROP ON braid.audit_log       TO 'braid_reaper'@'%';
GRANT SELECT, DELETE          ON braid.audit_pending       TO 'braid_reaper'@'%';
GRANT SELECT, INSERT, DELETE  ON braid.outbox              TO 'braid_reaper'@'%';
-- login_audit 的 SELECT 服务 DELETE 的 WHERE 谓词列(event_at 保留期条件);
-- 无 SELECT 则 purge 报 1143(演练实证,MySQL 权限规则:UPDATE/DELETE 的
-- 谓词列需要 SELECT)。
GRANT SELECT, DELETE          ON braid.login_audit         TO 'braid_reaper'@'%';
GRANT SELECT                  ON braid.schema_migrations   TO 'braid_reaper'@'%';
SQL
