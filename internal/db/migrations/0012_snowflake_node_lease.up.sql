-- ============================================================================
-- braid 0012_snowflake_node_lease.up.sql — T15.2 副本身份与雪花节点号自动
-- 分配(D-2026-10-01-t151 决策 2,实现口径全量钉死不得偏离):雪花节点号
-- 从「部署层静态分配」改为「MySQL 租约表仲裁」,消除人工外部分配
-- BRAID_*_SNOWFLAKE_NODE(k8s 多副本一律 auto;chart 静态钉 1/2/3 随本
-- 迁移摘钉转 auto,compose 显式 env 语义保留)。单表四职责:
--   ① node_id 0..1023 全局单池:api/worker/reaper 三类组件共用不分池,
--      relay 无生成器不参与;PK = node_id,acquire = INSERT 抢注、PK 竞态
--      败者有限重扫(≤3 次退避)后拒启;auto 池线性取空闲号(缺行或
--      过期行),显式 claim(含内建默认 1/2,视同显式)按号直达。
--   ② holder 身份 = kind:hostname:同 holder 复占即时(正常重启复航);
--      被他持 = 拒启(kill -9 残窗 ≤ TTL 有界,runbook 清行应急);
--      跨类前缀防同宿主 api/worker 互认复占。
--   ③ expires_at:租约 TTL(缺省 30s,续期间隔 TTL/3);过期判定全 DB 端
--      NOW(3)(会话 time_zone='+00:00' 连接契约,结构性排除应用时钟
--      漂移;不声称排除一切双持——MySQL 异步 failover 尾吞 + 心跳停顿的
--      zombie 双签发窗如实登记于 D-t151 决策 8-①,fencing 不随本迁移实施);
--      续期失败 = 停止签发 + 优雅下线 asynq 消费 + 响亮,复航 = 重新
--      acquire(进程退出重启承载);优雅停机 SIGTERM 必须 DELETE 释放;
--      回收 = 懒回收(过期行由下一 claimant CAS 接管,无后台 GC;行数
--      上界 = 池容量 1024,恒有界)。
--   ④ capabilities JSON NULL:实例能力宣告面(T15.6 消费:mode=all 硬闸
--      查在场实例全部宣告,NULL = 未宣告 fail-closed);建表即含一次建齐
--      ——省一次 ALTER 与一个迁移序号(D-t151 决策 2);T15.2 acquire
--      恒写 NULL。
--
-- 授权面(铁律):braid_api/braid_worker/braid_reaper 三账号
-- INSERT/UPDATE/SELECT + 优雅释放 DELETE(grants.sh / UPGRADE.md §3 /
-- README.md §5 / internal/preflight 断言台账四面同步);braid_relay 零面。
-- 本表是运行态簿记面,非业务事实:行生命周期 = 进程生命周期,不进审计、
-- 不进 outbox;DELETE 是设计语义(优雅释放)而非 append-only 违约——
-- 受保护表集(audit_log/login_audit)不含本表。
--
-- 风格契约继承 0001:InnoDB / utf8mb4 / utf8mb4_0900_ai_ci /
-- ROW_FORMAT=DYNAMIC;时间 DATETIME(3) UTC;密钥不入 SQL。主键不是应用侧
-- Snowflake——本表是「Snowflake 节点号的仲裁者」,node_id 是池序号,
-- 0011 tenants id=0 哨兵之外的第二类豁免,本注释即豁免记录。
-- ============================================================================

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS snowflake_node_lease (
  node_id      SMALLINT UNSIGNED NOT NULL COMMENT '雪花节点号(10bit 池序号 0..1023);PK = INSERT 抢注面,显式 claim 与 auto 池分配共用同一仲裁',
  kind         ENUM('api','worker','reaper') NOT NULL COMMENT '持有者组件类(T15.6 mode=all 硬闸扫三类;relay 无生成器不在词表,词表仅末尾追加)',
  holder_id    VARCHAR(255) NOT NULL COMMENT '持有者实例身份 = kind:hostname(k8s pod 名/compose 容器名天然唯一且 restart 稳定;同 holder 复占即时,被他持拒启)',
  expires_at   DATETIME(3) NOT NULL COMMENT '租约到期时刻(UTC);过期判定全 DB 端 NOW(3);懒回收 = 到期行由下一 claimant CAS 接管',
  capabilities JSON NULL COMMENT '实例能力宣告(T15.6 mode=all 硬闸消费面;NULL = 未宣告 fail-closed;T15.2 acquire 恒写 NULL)',
  PRIMARY KEY (node_id),
  CONSTRAINT chk_lease_node CHECK (node_id <= 1023)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='雪花节点号租约(T15.2,D-2026-10-01-t151 决策 2):四组件实例注册面;运行态簿记非业务事实,SIGTERM DELETE 释放是设计语义';
