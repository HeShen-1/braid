-- ============================================================================
-- braid 0013_login_audit_scoped_token.down.sql — T17.2 回退:login_audit
-- 撤 method/scopes/ttl_seconds/token_hash 四列 + event_type 回缩四词
-- ENUM。先 DELETE scoped_token_issued 行再回缩 ENUM(严格模式下枚举
-- 回缩遇存活词值即报错,顺序不可倒置);签发审计行随回退丢弃(镜像
-- 0009 down 对 email 资料的丢弃判,append-only 授权契约约束的是业务
-- 账号,迁移账号回退面不受限)。存量行的 method 值一并随列消亡,
-- 重新 up 后按 DEFAULT 'password' 口径重建(混流近似口径,见 up 注释)。
-- ============================================================================

SET NAMES utf8mb4;

DELETE FROM login_audit WHERE event_type = 'scoped_token_issued';

ALTER TABLE login_audit
  DROP CONSTRAINT chk_login_audit_method,
  DROP COLUMN token_hash,
  DROP COLUMN ttl_seconds,
  DROP COLUMN scopes,
  DROP COLUMN method,
  MODIFY COLUMN event_type ENUM('login.succeeded','login.failed','login.locked','login.rate_limited') NOT NULL
    COMMENT '词表封闭仅末尾追加;locked = 账号锁定窗内尝试,rate_limited = IP 固定窗口超限';
