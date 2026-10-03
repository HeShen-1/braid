-- ============================================================================
-- braid 0013_login_audit_scoped_token.up.sql — T17.2 scoped token 审计 +
-- login method 列(issue #136;D-2026-09-30-t131 登记④收编,落点裁定 =
-- login_audit 域单表「账号域安全事件流」,D-2026-10-03-t172):
--   ① method 列:OIDC 与口令登录可区分。**backfill 口径钉死(铁律要求,
--      不可移入他处)**:存量行(0013 前)两路混流、逐行不可辨
--      (OIDC 自 T11.1 起与口令登录同一发射点写本表,行形状无差异),
--      一律 backfill 'password' —— 0013 前 method='password' 的计数
--      **含 OIDC 回调**,统计消费方须知;这是对不可逆信息丢失的显式
--      承认,不是精确回填。词域封闭仅末尾追加(ldap 预留,T17.6);
--      login.* 事件必携非空、scoped_token_issued 恒 NULL,双向不变量
--      由 chk_login_audit_method 钉死。
--   ② scoped_token_issued 事件词:event_type ENUM 末尾追加(词表封闭
--      仅末尾追加契约,0005)。签发面 = POST /api/v1/auth/tokens
--      (T17.1 自窄化端点);主体复用 username 列、来源 IP 复用
--      client_ip 列。audit_log 结构性进不去(execution_id NOT NULL +
--      exec_date 分区键物化,无执行锚的账号域事实进不去——0005 头
--      注释同判;D-t131 ④登记即 login_audit 域)。
--   ③ 签发观测列组(仅 scoped_token_issued 行携带,login.* 行恒 NULL):
--      scopes = 逗号 join(词表 resource:action 无逗号,无歧义,
--      FIND_IN_SET 可查);ttl_seconds = clamp 后实际授予值(子 TTL ≤
--      呈现 token 剩余寿命的收窄事实,不是请求原值);token_hash =
--      签发 token 的 SHA-256 hex——**原值绝不入审计(铁律),哈希
--      至多**;泄露取证时对嫌疑 token 求哈希即可关联签发行。
--      reason 对签发行恒 NULL(仅 login.failed 携带,契约不变)。
-- 风格契约全部继承 0001/0005 头注释:InnoDB / utf8mb4 /
--   utf8mb4_0900_ai_ci / ROW_FORMAT=DYNAMIC;append-only 授权契约不变
--   (api 账号 INSERT/SELECT only,preflight 断言面零改);ENUM 尾追加
--   与 ADD COLUMN 皆 metadata-only(ALGORITHM=INSTANT,存量表零重建)。
-- ============================================================================

SET NAMES utf8mb4;

ALTER TABLE login_audit
  MODIFY COLUMN event_type ENUM('login.succeeded','login.failed','login.locked','login.rate_limited','scoped_token_issued') NOT NULL
    COMMENT '词表封闭仅末尾追加;locked = 账号锁定窗内尝试,rate_limited = IP 固定窗口超限,scoped_token_issued = /auth/tokens 自窄化签发(T17.2;签发行 reason 恒 NULL、method 恒 NULL)',
  ADD COLUMN method ENUM('oidc','password') NULL DEFAULT 'password'
    COMMENT '登录方法:oidc = OIDC 回调,password = 口令登录;**存量行(0013 前)两路混流不可辨,一律 backfill password —— 0013 前 password 计数含 OIDC(口径钉死,统计须知)**;词域封闭仅末尾追加(ldap 预留 T17.6);scoped_token_issued 行恒 NULL(chk_login_audit_method)'
    AFTER event_type,
  ADD COLUMN scopes VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL
    COMMENT '仅 scoped_token_issued:授予作用域逗号 join(词表 resource:action 无逗号,FIND_IN_SET 可查);login.* 行恒 NULL'
    AFTER client_ip,
  ADD COLUMN ttl_seconds INT UNSIGNED NULL
    COMMENT '仅 scoped_token_issued:clamp 后实际授予 TTL 秒(子 TTL ≤ 呈现 token 剩余寿命的收窄事实,非请求原值);login.* 行恒 NULL'
    AFTER scopes,
  ADD COLUMN token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL
    COMMENT '仅 scoped_token_issued:签发 token 的 SHA-256 hex(取证关联面);token 原值绝不入审计(铁律);login.* 行恒 NULL'
    AFTER ttl_seconds,
  ADD CONSTRAINT chk_login_audit_method
    CHECK ((event_type = 'scoped_token_issued' AND method IS NULL)
        OR (event_type <> 'scoped_token_issued' AND method IS NOT NULL));
