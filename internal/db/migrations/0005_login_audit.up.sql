-- ============================================================================
-- braid 0005_login_audit.up.sql — T6.4 全量移交回收(D-t44 §7-①):登录审计。
-- 背景:audit_log 结构性进不去(execution_id NOT NULL + exec_date 分区键
--   从 workflow_execution.created_at 物化,登录事件无执行锚)——登录事件
--   流是账号域事实,独立 append-only 表承载(ARCHITECTURE Q10 审计等级的
--   账号域延伸;D-t44 §3「登录事件流归 T6.4 告警面」的持久半边)。
-- 风格契约全部继承 0001/0002 头注释:InnoDB / utf8mb4 /
--   utf8mb4_0900_ai_ci / ROW_FORMAT=DYNAMIC;主键 = 应用侧 Snowflake
--   (禁自增);时间 DATETIME(3) UTC;IF NOT EXISTS + down 全 DROP IF
--   EXISTS;密钥/凭证不入行(只记 username 原样有界 + IP + 结果/成因码,
--   永不含密码或 token)。
-- append-only 授权契约(镜像 audit_log):api 账号对 login_audit 仅
--   INSERT/SELECT;无 UPDATE/DELETE 路径(Go 侧唯一写点 = internal/api
--   login handler 的独立小事务)。preflight CheckAuditAppendOnly 的表
--   枚举扩面(把本表纳入启动断言)登记为 T7.4 移交——本迁移先以注释
--   声明契约,避免 preflight 面与本任务耦合扩张。
-- ============================================================================

SET NAMES utf8mb4;

-- -----------------------------------------------------------------------------
-- 1. login_audit — 登录事件流(append-only):限流/账号锁定(D-t44 §7-①
--    另两件)的持久观测半边;Prometheus braid_login_attempts_total 是
--    告警消费面,本表是可 SQL 审计的历史。
--    username 原样有界(未知/恶意输入同样留痕,Go 侧 rune 截断 128 对齐
--    users.username 宽度):审计要记「攻击者尝试了什么」,不做词域过滤;
--    与 users.username 同 COLLATE utf8mb4_bin 便于精确 join 归因。
--    client_ip = gin ClientIP()(SetTrustedProxies(nil) 下 = RemoteAddr);
--    IPv6 文本上限 45。
--    event_type ENUM:封闭词表仅末尾追加(Go 侧闭集常量 + map 同步锚,
--    internal/api)。reason 仅 login.failed 携带(auth.CredentialsError
--    三态 + user_disabled;仅 DB 面,不进对客户端响应——防枚举契约)。
--    无分区:登录事件量级被限流钳制(默认 10/min/IP),远低于 audit_log
--    的分区诉求。保留期:janitor 的 event_at 批量清理随 reaper 保留期
--    旋钮接入(rev2 登记,T7.4/后续任务;量级钳制下非紧急)。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS login_audit (
  id         BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake,应用生成(api snowflake node=1;全库主键契约,禁自增)',
  event_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC,库端默认(与 users.created_at 同钟系)',
  event_type ENUM('login.succeeded','login.failed','login.locked','login.rate_limited') NOT NULL COMMENT '词表封闭仅末尾追加;locked = 账号锁定窗内尝试,rate_limited = IP 固定窗口超限',
  username   VARCHAR(128) COLLATE utf8mb4_bin NOT NULL COMMENT '登录尝试的 username 原样(rune 截断 128;未知/域外值同样留痕——审计记攻击面,不过滤);与 users.username 同 COLLATE 便于 join 归因',
  client_ip  VARCHAR(45) NOT NULL COMMENT '客户端 IP 文本(gin ClientIP,trusted proxies=nil 下 = RemoteAddr;IPv6 上限 45)',
  reason     VARCHAR(32) NULL COMMENT '仅 login.failed:invalid_input|unknown_user|wrong_password|user_disabled(auth 内部日志词表复用);succeeded/locked/rate_limited 恒 NULL',
  PRIMARY KEY (id),
  KEY idx_login_audit_event_at (event_at) COMMENT '时间窗查询(告警回看/保留期清理皆按事件时间扫)'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='登录事件流(T6.4):append-only,api 账号 INSERT/SELECT only;username/IP/结果+成因码,永不携密码或 token';
