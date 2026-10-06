-- ============================================================================
-- braid 0015_snowflake_lease_fencing.up.sql — T20.1 租约 fencing token
-- (D-2026-10-06-t201-lease-fencing;D-t151 决策 8-①「fencing token 彻底解
-- 登记后续不随 T15.2 实施」的后续在此落裁,0015 号自 D-t207 起预留给本窗):
--   ① fencing_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0:租约代际计数器,
--      任何持有变化同语句 +1——INSERT 新占 = 1;显式 CAS 复占/过期接管
--      = epoch+1(同 holder 复占也 bump:D-t204 边界③ 复占双活双签发面
--      由此关死,重启复占使前身进程 token 即刻失效);续期不 bump(同
--      持有同代);Release(DELETE)后行消失 = 一切 fence 拒。
--   ② 语义 = 等值加冕(非跨行单调比较):消费端(fence 锁定读)只认
--      行上当前 (holder_id, fencing_epoch) 精确等值——DB 计数器在异步
--      failover 尾吞下自身会回退,等值语义把参照系与加冕合一,回退窗
--      下 fail-closed,正确性恒单写者;谓词不含 expires_at(夺写只因
--      易主,不因墙钟——懒回收设计下过期未被接管即无第二持有者)。
--      详见 DECISIONS.md D-2026-10-06-t201 决策 2/3。
--   ③ DEFAULT 0 = 存量行与滚动窗 legacy 值(D-t151 决策 6 铺满窗先例):
--      旧二进制不写 epoch 不查 fence;新二进制首次持有变化即 bump 起代;
--      混跑窗新旧互 fence 不成立,如实登记不声称闭合。
--   ④ 判等口径:本列数值比较无 collation 面;holder_id 各比较点统一
--      显式 COLLATE utf8mb4_bin 属查询层(D-t204 :778 交接缝收口),
--      本迁移不动表缺省 collation(0012 既有行零重排)。
--
-- 授权面(铁律):零新增——三账号 INSERT/UPDATE/SELECT/DELETE 四权
-- 既有(grants.sh / UPGRADE §3 / README §5 / internal/preflight 断言
-- 台账四面零改;fence 锁定读 = SELECT,复用既有面)。
--
-- ADD COLUMN ... DEFAULT = metadata-only(ALGORITHM=INSTANT,存量表零
-- 重建,0013 同款);风格契约继承 0001:InnoDB / utf8mb4 /
-- utf8mb4_0900_ai_ci / ROW_FORMAT=DYNAMIC;时间 DATETIME(3) UTC;密钥
-- 不入 SQL。BIGINT UNSIGNED 上界 2^64-1:每次持有变化 +1,物理耗尽
-- 不可能(每秒一次易主也需 5849 亿年)。
-- ============================================================================

SET NAMES utf8mb4;

ALTER TABLE snowflake_node_lease
  ADD COLUMN fencing_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0
    COMMENT '租约代际(D-2026-10-06-t201):任何持有变化(新占/接管/同 holder 复占)同语句 +1,续期不动;消费端等值加冕语义——fence 锁定读只认行上当前 (holder_id, fencing_epoch) 精确等值;DEFAULT 0 = 存量/滚动窗 legacy(首次持有变化起代)'
    AFTER capabilities;
