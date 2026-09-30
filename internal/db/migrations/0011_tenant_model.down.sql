-- braid 0011_tenant_model.down.sql — 0011 回退:FK → 键 → 归属列 → 租户表
-- (与 up 的 列→键→FK 顺序对称逆行;FK 先拆否则换键时 1553 报「索引被
-- FK 需要」,见 up ② 顺序契约)。
-- 边界如实登记:非零租户数据在场时,definition 全局键还原会在「跨租户
-- 同名同号」行上 1062 冲突——down 语义 = 回到单租户(平台租户)世界,
-- 先运维收敛数据再 down(0009 email 回退同判)。

ALTER TABLE users DROP FOREIGN KEY fk_users_tenant;
ALTER TABLE workflow_definition DROP FOREIGN KEY fk_def_tenant;
ALTER TABLE workflow_execution DROP FOREIGN KEY fk_exec_tenant;

ALTER TABLE workflow_execution
  DROP INDEX idx_exec_tenant_name_created,
  ADD KEY idx_exec_name_created (definition_name, created_at) COMMENT '按工作流(跨版本)列执行,T5.5 列表(0001 语义还原)';

ALTER TABLE workflow_definition
  DROP INDEX uk_def_tenant_name_version,
  DROP INDEX idx_def_tenant_name_status_version,
  DROP INDEX idx_def_tenant_status_updated,
  ADD UNIQUE KEY uk_def_name_version (name, version) COMMENT '版本定位 + 同名同号并发发布闸(T5.4);NULL 草稿不占号(0001 语义还原)',
  ADD KEY idx_def_name_status_version (name, status, version) COMMENT 'T5.4 默认触发:该工作流最新 PUBLISHED 版本一次后向索引扫描(0001 语义还原)',
  ADD KEY idx_def_status_updated (status, updated_at) COMMENT '草稿箱/发布列表按近序分页(T5.4,0001 语义还原)';

ALTER TABLE users DROP COLUMN tenant_id;
ALTER TABLE workflow_definition DROP COLUMN tenant_id;
ALTER TABLE workflow_execution DROP COLUMN tenant_id;
ALTER TABLE audit_pending DROP COLUMN tenant_id;
ALTER TABLE audit_log DROP COLUMN tenant_id;

DROP TABLE IF EXISTS tenants;
