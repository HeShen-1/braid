-- 0004 down:WAL 暂存表整表 DROP。语义边界:down 时已暂存未搬运的审计行
-- 随表消亡(异步面回退即放弃该窗事件);audit_log 终行不受影响(无 FK、
-- 无触发器/视图依赖)。
SET NAMES utf8mb4;
DROP TABLE IF EXISTS audit_pending;
