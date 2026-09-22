-- ============================================================================
-- braid 0002_auth_users.up.sql — M5 T5.1「用户/角色模型 + JWT + bcrypt」:users + user_role
-- 风格契约全部继承 0001_init 头注释(见该文件,不重复立约):InnoDB /
--   utf8mb4 / utf8mb4_0900_ai_ci / ROW_FORMAT=DYNAMIC;主键 = 应用侧
--   Snowflake(禁自增);时间一律 DATETIME(3) UTC(created_at 库端默认、
--   updated_at ON UPDATE);ENUM 仅封闭词表且只许末尾追加;IF NOT EXISTS +
--   down 全 DROP IF EXISTS → 重复执行 no-op;无触发器/存储过程;密钥不
--   入 SQL(JWT 签名密钥只经 BRAID_AUTH_JWT_SECRET env 注入,密码哈希列
--   本身合法)。
-- 主体字符串契约:users.username 即「主体字符串」,与 workflow_definition.
--   created_by / workflow_execution.triggered_by / audit_log.actor_id 的
--   VARCHAR(128) 同值域直通(0001:47 为此留位「users 落地后可增量加
--   back-FK」——本迁移不顺带 ALTER 引擎表,扩面留独立增量迁移)。宽度
--   维持 128 与既有主体列对齐;t1.3 审计 medium_low[36] 曾建议 255
--   (OIDC sub 上限),未采纳:单边加宽造成主体值域分裂;T5.3 换源出现
--   时连同既有主体列一起加宽。username 一经创建不可改名(主体字符串
--   稳定性 = 审计归因完整性),P0 无改名路径。
-- bcrypt 列:输出恒 60 ASCII 字符($2a$/$2b$/$2y$ 前缀),CHAR(60)
--   CHARACTER SET ascii COLLATE ascii_bin 沿用 dsl_hash / trace_id 定长
--   hash 列先例;chk_user_password_hash 把「写入明文/裸哈希」变成 3819
--   响亮失败——防降级门闸(t1.3 审计「禁静默降级」哲学在 auth 的落点)。
-- 保留字:username 拒绝 'system'(0001 的 triggered_by 系统触发哨兵),
--   防人工账号与系统归因在 audit 面混淆;双闸 = 本 CHECK + Go 侧
--   reservedUsernames。
-- ============================================================================

SET NAMES utf8mb4;

-- -----------------------------------------------------------------------------
-- 1. users — 内置用户:登录名兼主体字符串 + bcrypt 密码哈希 + 启停状态。
--    username COLLATE utf8mb4_bin(逐字二进制比较):登录定位与唯一键都
--    不做大小写折叠(uk_exec_idem 的 idempotency_key 同款理由)——
--    「Alice」与「alice」是两个账号,折叠归并 = 认证面账号混淆。
--    status 仅 ACTIVE/DISABLED 两态;DISABLED 由登录路径在密码验证通过
--    后判定(错密码优先返回统一凭证错误,防「探测账号是否被禁」的状态
--    oracle;判定顺序由 T5.1 单测钉死)。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
  id            BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake,应用生成(全库主键契约一致,禁自增)',
  username      VARCHAR(128) COLLATE utf8mb4_bin NOT NULL COMMENT '登录名兼主体字符串:直通 created_by/triggered_by/actor_id(同 VARCHAR(128) 值域);域 ^[a-z][a-z0-9_-]{0,127}$ 且保留字 system 拒绝(审计系统触发哨兵,见 chk_user_username;Go 侧同则,双闸);一经创建不可改名(审计归因稳定性)',
  password_hash CHAR(60) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'bcrypt 哈希($2a$/$2b$/$2y$ 前缀,恒 60 ASCII);形态由 chk_user_password_hash 门禁(防明文/裸哈希静默降级);cost 参数内嵌于哈希串,不另设列',
  status        ENUM('ACTIVE','DISABLED') NOT NULL DEFAULT 'ACTIVE' COMMENT 'ACTIVE=可登录;DISABLED=登录拒绝(密码验证通过后判定);词表仅末尾追加',
  created_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  updated_at    DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT 'UTC;status 翻转随库端 ON UPDATE 刷新',
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_username (username) COMMENT '登录 WHERE username=? 精确定位 + 账号唯一(utf8mb4_bin 二进制比较,无大小写折叠);重复创建 → 1062 → auth.ErrConflict(T5.1)',
  CONSTRAINT chk_user_username CHECK (username <> 'system' AND REGEXP_LIKE(username, '^[a-z][a-z0-9_-]{0,127}$')),
  CONSTRAINT chk_user_password_hash CHECK (CHAR_LENGTH(password_hash) = 60 AND LEFT(password_hash, 4) IN ('$2a$', '$2b$', '$2y$'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='内置用户(Q13):登录名=主体字符串,bcrypt 哈希,启停状态;token 不落库(internal/auth 无状态 JWT)';

-- -----------------------------------------------------------------------------
-- 2. user_role — 用户×角色授权(多对多):bootstrap 主账号需三角色齐持,
--    创作者与审批人同人是常态。role 词表被 ARCHITECTURE Q13 锁死三种,
--    用 ENUM(封闭词表契约);Go 常量与 information_schema 含序对拍钉死
--    (engine/enum_test 先例)——追加角色 = DDL 末尾追加 + Go 常量 +
--    RBAC 判定面同 PR 旅行。
--    行为 insert-only 授权(撤销授权/用户管理 API 属后续任务,不在
--    T5.1):无 updated_at(node_attempt 先例:一次终写,无改写路径)。
--    反查「谁持有某角色」(T4.5 通知扇出候选)显式不建索引:人数量级
--    (几十行)全表扫无感,投机索引否决。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_role (
  id         BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake,应用生成;复合 PK (user_id,role) 物理上更省一列,但「全部主键=应用侧 Snowflake」是 0001 头注释硬契约,豁免须 D-条目经用户确认,本迁移不夹带',
  user_id    BIGINT UNSIGNED NOT NULL COMMENT '授权目标用户',
  role       ENUM('admin','author','approver') NOT NULL COMMENT '角色词表(Q13 锁死三种):admin=平台管理 / author=workflow 作者 / approver=审批人;小写对齐 node_execution.type「词表名词」先例;词表仅末尾追加',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC;授权时刻;insert-only 无 updated_at(无改写路径,node_attempt 先例)',
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_role (user_id, role) COMMENT '重复授予显式失败(1062 → auth.ErrConflict);前缀 (user_id) 即登录取角色扫描(T5.1 Authenticate 经 JSON_ARRAYAGG 单行聚合读齐)',
  CONSTRAINT fk_user_role_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='用户×角色授权(Q13 三角色,多对多):FK RESTRICT 保身份不静默蒸发,词表含序与 Go 常量对拍钉死';
