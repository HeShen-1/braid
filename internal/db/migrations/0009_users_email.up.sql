-- ============================================================================
-- braid 0009_users_email.up.sql — T12.2 运维加固批(D-t45 #2 兑现):
-- users.email 列 + 按 approver 扇出投递的通知解析面。
--
-- 语义:可空用户资料邮箱;NULL = 未配置(扇出时该 approver 名字保留邮件
-- 正文 + ops 副本兜底,不阻塞他人收件)。应用面**只读**(notify sender 经
-- auth.EmailsByUsernames 解析快照 approvers → email);回填通道 = 运维 SQL
-- (账号域 INSERT-only 授权契约不变:api/worker 对 users 仍 SELECT only,
-- grants.sh 零改动;与账号创建同一运维面,0002 user_role 注释同口径)。
--
-- 形态闸:chk_user_email 与 auth Go 侧 emailRe 双闸(同则逐字,漂移由
-- TestEmailDomainPinned 变异锚暴露——usernameRe/chk_user_username 先例);
-- POSIX [:space:] 形态使 DDL 字面与 Go 字面字节一致(免反斜杠转义歧义:
-- MySQL 字符串默认把 '\s' 吃成 's')。不设 UNIQUE:两账号同邮箱 = 各收
-- 一封,无正确性损害;UNIQUE 反而制造运维回填冲突面。
--
-- 字符集 ascii_bin(镜像 password_hash 判;EAI/UTF-8 邮箱非本期,超 ASCII
-- 形态被字符集拒——与公网邮箱现状一致)。VARCHAR(254) = RFC 5321 最大
-- path 长度。
-- ============================================================================

ALTER TABLE users
  ADD COLUMN email VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NULL
    COMMENT '用户资料邮箱(T12.2 扇出投递解析面;NULL = 未配置,回填走运维 SQL;应用账号只读)'
    AFTER password_hash,
  ADD CONSTRAINT chk_user_email
    CHECK (email IS NULL OR REGEXP_LIKE(email, '^[^@[:space:]]+@[^@[:space:]]+$'));
