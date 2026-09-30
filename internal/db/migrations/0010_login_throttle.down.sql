-- braid 0010_login_throttle.down.sql — 0010 回退:防御状态表整表 DROP
-- (簿记面无审计事实,回退不损证据链;回退后应用侧 DB 换装代码必须同步
-- 回退,否则登录面启动即败——防御不可用 = fail-closed,与「DB 故障下
-- 登录本就不可用」同判)。

DROP TABLE IF EXISTS login_throttle;
