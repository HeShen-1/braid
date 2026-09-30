-- ============================================================================
-- braid 0010_login_throttle.up.sql — T11.3 follow-up(D-2026-09-29-t113-
-- auth-depth #2):登录防御 DB 锚定。loginLimiter 进程内实现退役(D-t64
-- 已知边界「重启清零/多副本各自计数」消解),防御状态迁入 MySQL 真相源
-- ——防御状态簿记面,镜像 chain_heads「可 UPDATE 簿记 ≠ 审计事实」哲学
-- (D-t93):login_audit 仍是 append-only 审计事实(api INSERT only 不
-- 变),本表是可变工作状态,不触任何 append-only 授权契约。
--
-- 键形(D-t113 #3 钉死:锁定键 = username 单键,不取 per-(u,ip) 复合):
--   'u:'+username(rune 截 128,与 limiterKey/审计列同式)→ 连败锁定行;
--   'ip:'+client_ip(≤45)→ IP 固定窗口行。两 kind 同表,natural PK。
--
-- 语义(与进程内实现逐字对齐):连败锁定(成功 UPDATE 重置、期满自愈、
-- 闲置衰减 4×lockout 后重开计数)/ IP 固定窗口 / 防枚举 429 同形(表内
-- 两成因仍不可区分)/ 防御拒绝只发指标不落 login_audit(D-t64 写放大
-- 边界不动)。阈值跨副本精确 = 行锁单事务(读-判-写同事务,D-t113 #2)。
--
-- 时间列全 app 侧注入(时钟可测;不设 ON UPDATE 依赖——每次写行必显式
-- 携带,窗口/衰减判定因此确定性可测)。行生命周期:成功 → UPDATE 重置;
-- 闲置 → janitor 过期回收(reaper,SELECT+DELETE,镜像 purgeLoginAudit;
-- 行空间由 distinct 键限界,接替进程内 8192 封顶的防灌水职能)。
--
-- 风格契约继承 0001/0005:InnoDB / utf8mb4 / utf8mb4_0900_ai_ci /
-- ROW_FORMAT=DYNAMIC;IF NOT EXISTS + down 全 DROP IF EXISTS;无 Snowflake
-- 主键(natural key,chain_heads 先例);密钥/凭证永不入行。
-- 授权契约:api 账号 SELECT/INSERT/UPDATE(reaper SELECT/DELETE 服务
-- janitor 谓词列);deploy/grants.sh + deploy/README.md §5 同 PR 旅行。
-- ============================================================================

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS login_throttle (
  throttle_key VARCHAR(130) COLLATE utf8mb4_bin NOT NULL COMMENT '自然键:"u:"+username(byte 截 128、UTF-8 边界安全,auditUsername/limiterKey 同式;0005 同句的 rune 误标不追改——已应用迁移不可改)或 "ip:"+client_ip(≤45);键域有界 = 防灌水;utf8mb4_bin 逐字比较镜像 users.username 先例(ai_ci 折叠大小写 = 防御键混淆,审计 LOW)',
  kind         ENUM('username','ip') NOT NULL COMMENT '行语义分派:username = 连败锁定,ip = 固定窗口限流',
  window_start DATETIME(3) NOT NULL COMMENT 'ip 行:固定窗口起点(过期重开);u 行:最近计数写入时刻 = 闲置衰减纪元(record 独占,check 的活性触达只写 updated_at 不刷新此列——两纪元解耦,orch-review HIGH 修订)',
  count        INT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'ip 行:窗口内尝试数;u 行:连败计数(成功清零;锁定期满后重开)',
  locked_until DATETIME(3) NULL COMMENT '仅 u 行:锁定期满时刻(NULL = 未锁;期满自愈,无显式解锁面)',
  updated_at   DATETIME(3) NOT NULL COMMENT '最近触达时刻(app 侧注入;janitor 过期回收谓词列,idx 同名)',
  PRIMARY KEY (throttle_key),
  KEY idx_login_throttle_updated (updated_at) COMMENT 'janitor 过期回收路径(DELETE 谓词列;SELECT 服务 DELETE 谓词,grants 注记同 login_audit 先例)'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='登录防御状态簿记面(T11.3,D-t113 #2):连败锁定 + IP 固定窗口;api SELECT/INSERT/UPDATE,reaper SELECT/DELETE;时间全 app 侧注入';
