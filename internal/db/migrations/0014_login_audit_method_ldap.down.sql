-- ============================================================================
-- braid 0014_login_audit_method_ldap.down.sql — T17.6 回退:method ENUM
-- 回缩双值(oidc|password),列注释恢复 0013 措辞(预留承诺随回退复挂,
-- 重新 up 再兑现)。先 DELETE 'ldap' 行再回缩(严格模式下枚举回缩遇
-- 存活词值即报错,顺序不可倒置;镜像 0013 down 丢弃判——迁移账号回退
-- 面不受业务 append-only 契约约束)。DEFAULT/NULL/CHECK 语义在 up/down
-- 两侧恒同,零回退暗面。
-- ============================================================================

SET NAMES utf8mb4;

DELETE FROM login_audit WHERE method = 'ldap';

ALTER TABLE login_audit
  MODIFY COLUMN method ENUM('oidc','password') NULL DEFAULT 'password'
    COMMENT '登录方法:oidc = OIDC 回调,password = 口令登录;**存量行(0013 前)两路混流不可辨,一律 backfill password —— 0013 前 password 计数含 OIDC(口径钉死,统计须知)**;词域封闭仅末尾追加(ldap 预留 T17.6);scoped_token_issued 行恒 NULL(chk_login_audit_method)';
