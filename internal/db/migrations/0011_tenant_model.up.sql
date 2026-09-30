-- ============================================================================
-- braid 0011_tenant_model.up.sql — T11.2 SaaS 多租户(D-2026-09-30-t112):
-- 租户模型 + 行级隔离的数据面半边(隔离执法 = app 层谓词,D-t112 #1;
-- 本迁移只落「租户是数据的行属性」,#2)。四件事:
--   ① tenants 新表 + 种子 id=0 'platform'(平台租户哨兵):存量数据与
--      未携租户的既有写路径全部归 0——单租户部署行为零变化,分项
--      commit 各自独立绿的锚。
--   ② 五表冗余 tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0:
--      users(用户单成员,D-t112 #2:username 全局唯一保持,uk_user_
--      username 不动)/ workflow_definition / workflow_execution(api
--      读面行级隔离的锚表;node 系经 execution 锚派生,零加列)/
--      audit_log + audit_pending(审计读面按读者租户裁剪,含模型名
--      actor 的 llm 查询——user 行判租不覆盖非用户 actor,D-t112 #4;
--      直写随分区键同语句解析,暂存行 0 占位、flush 随 exec_date 一并
--      物化)。
--      DEFAULT 0 = 既有代码路径(未显式携租户)迁移后即绿;NOT NULL =
--      钉死「未知租户 = 平台租户」而非「无租户」(归属不可缺省)。
--   ③ definition/execution 键租户前缀化(旧全局键退役,down 还原):
--      definition 命名空间 = 租户内(同名工作流跨租户可并存,发布并发
--      闸语义原样);execution 名称列表索引(c2 起 api 查询恒携租户
--      谓词)。reaper 扫描索引(status,activity)与幂等键(uk_exec_
--      idem——值域已含主体作用域哈希)无租户语义,不动。
--   ④ row_hash 19 列定集不动(D-t93):tenant_id 是访问控制注记列非
--      证据内容,不进哈希——链校验按显式列集重算,新列在场零扰动;
--      INSERT-only 授权 + preflight 防篡改面不变。
--
-- 部署注记:audit_log 是日分区表——分区表 ADD COLUMN 在 <8.0.29 不支持
-- INSTANT、走 INPLACE(逐分区重建);部署镜像 mysql:8.0 浮动 tag 下
-- ≥8.0.29 可 INSTANT,保守按 INPLACE 预算窗口(大保留期库低峰执行)。
-- 真正恒走表重建的是 workflow_execution 的索引实建(DROP+ADD 三键,
-- INPLACE rebuild)——本迁移最重的单表窗口。audit 两表不加 FK
-- (镜像 0004/0008 锁面判据);users/definition/execution 的 tenant_id
-- 加 FK RESTRICT(tenants 行是归属引用锚,租户删除 = 显式运维决策,
-- RESTRICT 防孤儿归属)。授权面:本迁移零授权变更(tenants 引用经 FK
-- 内部检查,api/worker 无直接读写面;租户开通 = 运维面,P1 登记)。
--
-- 风格契约继承 0001:InnoDB / utf8mb4 / utf8mb4_0900_ai_ci /
-- ROW_FORMAT=DYNAMIC;主键 = 应用侧 Snowflake(id=0 固定哨兵是唯一
-- 豁免,镜像 username='system' 保留字先例,本注释即豁免记录);
-- 时间 DATETIME(3) UTC;密钥不入 SQL。
-- ============================================================================

SET NAMES utf8mb4;

-- -----------------------------------------------------------------------------
-- 1. tenants — 租户模型:行级隔离的归属锚。种子行 id=0 'platform' =
--    平台租户(存量归属哨兵);id 固定值豁免 Snowflake(见文件头)。
--    slug 词域镜像 workflow_definition.name(^[a-z][a-z0-9-]{1,63}$),
--    Go 侧同则双闸(chk_user_username 先例);utf8mb4_bin 逐字比较
--    (uk_user_username 同款理由:ai_ci 折叠 = 租户引用面混淆)。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenants (
  id         BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake,应用生成(全库主键契约);id=0 = 平台租户哨兵(存量数据归属锚,单租户部署恒只此一行)——固定值豁免 Snowflake,镜像 username=system 保留字先例(0011 头注释即豁免记录)',
  slug       VARCHAR(64) COLLATE utf8mb4_bin NOT NULL COMMENT '租户短名(URL/API 引用面);域 ^[a-z][a-z0-9-]{1,63}$(镜像 workflow_definition.name 词域,Go 侧同则双闸);utf8mb4_bin 逐字比较(uk_user_username 同款:折叠 = 租户混淆)',
  name       VARCHAR(200)    NOT NULL COMMENT '展示名(自由文本,无词域)',
  status     ENUM('ACTIVE','DISABLED') NOT NULL DEFAULT 'ACTIVE' COMMENT 'ACTIVE=可承载新数据;DISABLED 的存量只读执法面归后续任务(P1 登记,本任务只落模型);词表仅末尾追加',
  created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  PRIMARY KEY (id),
  UNIQUE KEY uk_tenant_slug (slug) COMMENT '租户短名唯一(引用面稳定);重复创建 → 1062',
  CONSTRAINT chk_tenant_slug CHECK (REGEXP_LIKE(slug, '^[a-z][a-z0-9-]{1,63}$'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='租户模型(T11.2,D-2026-09-30-t112):行级隔离归属锚;五表冗余 tenant_id 引用本表;api/worker 无直接读写面(租户开通 = 运维面)';

INSERT INTO tenants (id, slug, name)
VALUES (0, 'platform', 'Platform (bootstrap tenant)')
ON DUPLICATE KEY UPDATE id = id; -- 幂等重放形态 = 审计批量写同款(禁 INSERT IGNORE——静默降级哲学)

-- -----------------------------------------------------------------------------
-- 2. 归属列:五表冗余 tenant_id(存量回填 0 = 平台租户),先加列后换键
--    最后加 FK(顺序契约:换键需列在场;FK 需 tenant 前缀索引在场,否则
--    MySQL 隐式建单列索引、down 先换键时 1553 报「索引被 FK 需要」——
--    up 列→键→FK / down FK→键→列 对称可逆)。定位:AFTER id
--    (users/definition/execution,归属列即主维度)/ AFTER execution_id
--    (audit 两表,镜像「租户随执行锚」语义)。
-- -----------------------------------------------------------------------------
ALTER TABLE users
  ADD COLUMN tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '归属租户(D-t112 #2:用户单成员,角色随人——user_role 零改动);0=平台租户(存量哨兵);行级隔离判据列;租户迁移后 token 残余访问 ≤ TTL 24h(roles 快照同界)' AFTER id;

ALTER TABLE workflow_definition
  ADD COLUMN tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '归属租户(D-t112):定义命名空间 = 租户内(uk 见 ③);0=平台租户;CRUD 读写判据列' AFTER id;

ALTER TABLE workflow_execution
  ADD COLUMN tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '归属租户(D-t112):api 读面行级隔离锚(node 系经本表锚派生);0=平台租户;Instantiate 必显式携带(DEFAULT 0 只服务存量回填,缺省写入会错标非零租户)' AFTER id;

ALTER TABLE audit_log
  ADD COLUMN tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '归属租户(D-t112 #4):审计读面按读者租户裁剪的判据列(模型名 actor 无法经 user 行判租的解);访问控制注记非证据内容,row_hash 19 列定集不含本列(D-t93 链零扰动);无 FK(0004 锁面判据);0=平台租户;分区表 INPLACE 重建见文件头' AFTER execution_id;

ALTER TABLE audit_pending
  ADD COLUMN tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '归属租户(D-t112):暂存行按 0 占位(暂存面不可见,D-t63 #1),flush 随 exec_date 一并物化真实租户;无 FK(0004 锁面判据);0=平台租户' AFTER execution_id;

-- -----------------------------------------------------------------------------
-- 3. 键租户前缀化:旧全局键退役(防「租户撞名闸缺失」静默降级),新键
--    携 tenant_id 前缀;语义注释继承 0001 对应键,不重复论证。
-- -----------------------------------------------------------------------------
ALTER TABLE workflow_definition
  DROP INDEX uk_def_name_version,
  DROP INDEX idx_def_name_status_version,
  DROP INDEX idx_def_status_updated,
  ADD UNIQUE KEY uk_def_tenant_name_version (tenant_id, name, version) COMMENT '租户内版本定位 + 同名同号并发发布闸(0001 uk_def_name_version 语义 + 租户前缀);NULL 草稿不占号(唯一索引允许多 NULL)不变',
  ADD KEY idx_def_tenant_name_status_version (tenant_id, name, status, version) COMMENT '租户内默认触发:该工作流最新 PUBLISHED 版本一次后向索引扫描(0001 idx_def_name_status_version 语义 + 租户前缀)',
  ADD KEY idx_def_tenant_status_updated (tenant_id, status, updated_at) COMMENT '租户内草稿箱/发布列表按近序分页(0001 idx_def_status_updated 语义 + 租户前缀)';

ALTER TABLE workflow_execution
  DROP INDEX idx_exec_name_created,
  ADD KEY idx_exec_tenant_name_created (tenant_id, definition_name, created_at) COMMENT '租户内按工作流(跨版本)列执行,T5.5 列表(0001 idx_exec_name_created 语义 + 租户前缀;c2 起 api 查询恒携租户谓词)';

-- -----------------------------------------------------------------------------
-- 4. 归属 FK(RESTRICT:租户删除 = 显式运维决策,防孤儿归属)。至此
--    definition/execution 的 tenant 前缀索引已在场(③ 实建),两表 FK
--    不触发隐式索引;users 无 tenant 前缀索引查询面(uk_user_username
--    点查即登录路径,无按租户列 users 的读面)——fk_users_tenant 会
--    隐式建 users.tenant_id 单列索引(如实登记的隐式物;down DROP
--    COLUMN 随列消亡,无 1553 面)。audit 两表无 FK(0004 锁面判据,
--    镜像既有判)。
-- -----------------------------------------------------------------------------
ALTER TABLE users
  ADD CONSTRAINT fk_users_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT ON UPDATE RESTRICT;

ALTER TABLE workflow_definition
  ADD CONSTRAINT fk_def_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT ON UPDATE RESTRICT;

ALTER TABLE workflow_execution
  ADD CONSTRAINT fk_exec_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT ON UPDATE RESTRICT;
