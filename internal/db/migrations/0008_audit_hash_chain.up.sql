-- ============================================================================
-- braid 0008_audit_hash_chain.up.sql — T9.3 审计哈希链(D-2026-09-29-t93):
-- ① audit_pending 补 prev_hash/row_hash 两列(镜像 audit_log 0001 建列语义)
--    ——逐行链「写入即算」:Enqueue 落暂存行前在 chain_heads 锁下算好 hash,
--    暂存行**带着 hash 运输**,flush 纯搬运不重算(串链与搬运解耦,flush 不取
--    head 锁)。存量在途行按缺省落位(''/NULL = 未启链期,链校验跳过)。
-- ② chain_heads 新表 = per-execution 链锚:链写者(Append/Enqueue)仅对
--    本 execution 的 head 行 FOR UPDATE——审计写入器锁面自持于专表,不进
--    全局锁序(workflow_execution 零触,D-t26 lockfree-read 哲学延续)。
--    链序 = prev_hash 链接序(链首 prev=''),**不是 id 序**:snowflake 同毫秒
--    跨进程可倒置(snowflake.go 免责条款钦定「id 序只是公平调度序非因果序」),
--    id 序仅作展示序与轮换触发的启发式。
--
-- 授权契约边界:chain_heads 是可 UPDATE 的**簿记面**(链尾推进/校验轮换/
-- 断链闩),不是 audit_log——append-only 铁律与 D-t73 preflight
-- CheckAuditAppendOnly 的断言表集 {audit_log, login_audit} 不含本表。
-- 授权矩阵真源 = deploy/grants.sh(api/worker SELECT+INSERT+UPDATE;
-- reaper SELECT+INSERT+UPDATE+DELETE——reaper 自身是 attempt.reaped 等
-- 同步 Append 发射面(head 懒建可达故有 INSERT),DELETE 专属 janitor
-- 分区回收级联)。
--
-- 断链执法语义(D-t93 #5):broken_at 非 NULL ⇒ Append/Enqueue 读 head 即拒
-- (ErrChainBroken,该执行业务停摆待人工,其余执行不受累);闩复位 = 人工。
-- 无 FK(镜像 audit 两表同判:FK 父行检查会把锁面加回写热事务)。
--
-- 方言与 0001/0004 同规:MySQL 8.0.16+、InnoDB、utf8mb4/utf8mb4_0900_ai_ci、
-- DATETIME(3) UTC(连接契约)、Snowflake 关联。不分区:行数 = 执行数,
-- 量级与留存均无分区诉求(janitor 分区回收级联按保留期解除链义务)。
-- ============================================================================

SET NAMES utf8mb4;

ALTER TABLE audit_pending
  ADD COLUMN prev_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' COMMENT '哈希链前驱行 row_hash(逐行链写入即算,D-t93):''=未启链期在途残行;flush 原样搬运不重算' AFTER payload,
  ADD COLUMN row_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '本行规范化哈希(SHA-256,19 应用侧已知列长度前缀编码):与后行 prev_hash 对拼验链;NULL=未启链期在途残行(链校验跳过);flush 原样搬运' AFTER prev_hash;

CREATE TABLE IF NOT EXISTS chain_heads (
  execution_id     BIGINT UNSIGNED NOT NULL COMMENT '链锚 = workflow_execution.id(1:1,PK 钉死);首条链行写入时懒建,存量未启链执行无行(校验跳过)',
  last_id          BIGINT UNSIGNED NOT NULL COMMENT '链尾行 id(audit_log 终行身份;可能在 audit_pending 在途)。⚠ id 序 ≠ 链序(同毫秒跨进程倒置),本列仅作链尾指认与轮换触发启发式',
  last_row_hash    CHAR(64)        CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '链尾 row_hash:下一行 prev_hash 的唯一真源(同步行经它拿到在途暂存行 hash——逐行链对批内链的结构性胜因);行存在即有值',
  verified_last_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近一次全绿校验时的 last_id 快照:与 last_id 不等 = 有新活动(轮换触发启发式,非正确性锚——正确性锚 = 全链 linkage 复走)',
  verified_at      DATETIME(3)     NULL COMMENT '最近一次全绿校验时刻:校验轮换(到期即全链复走,历史行篡改不因已验过而漏检)',
  broken_at        DATETIME(3)     NULL COMMENT '断链闩:非 NULL = 链校验红(改行/删行/叉链/丢行),Append/Enqueue 即拒 ErrChainBroken;人工复位',
  broken_reason    VARCHAR(255)    NULL COMMENT '断链归因(首见原因;响亮日志与告警字段,不自动覆盖)',
  updated_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT 'head 推进/校验/落闩的观测时刻(非事件时刻)',
  PRIMARY KEY (execution_id) COMMENT 'per-execution 单锚;链写者行锁的唯一落点'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='审计哈希链 per-execution 锚(T9.3,D-2026-09-29-t93):可 UPDATE 簿记面(非 audit_log,不触 append-only 授权契约);链序=prev_hash 链接序(非 id 序),链尾=串行化点,轮换校验=全链复走锚,断链闩=fail-closed 执法位';
