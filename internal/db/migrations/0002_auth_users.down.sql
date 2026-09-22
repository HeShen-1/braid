-- braid 0002_auth_users.down.sql — 逆 FK 依赖序先拆 user_role 再拆 users;
-- DROP IF EXISTS → 重复执行 no-op(0001 契约)。
DROP TABLE IF EXISTS user_role;
DROP TABLE IF EXISTS users;
