-- ============================================================================
-- braid 0009_users_email.down.sql — T12.2 回退:users.email 列随列消亡
-- (含约束 chk_user_email)。已回填的资料邮箱丢弃;扇出面退回 P0 形态
-- (BRAID_SMTP_NOTIFY_TO 固定收件,DSL approvers 名单进正文)。
-- ============================================================================

ALTER TABLE users
  DROP COLUMN email;
