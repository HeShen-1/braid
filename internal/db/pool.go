package db

import (
	"database/sql"
	"time"
)

// ConfigurePool 对业务入口的 *sql.DB 施加保守默认池上限。
//
// 背景(D-t63 移交,2026-09-26 回收):四入口(api/relay/worker/reaper)
// 此前 sql.Open 后不设上限,而 T6.3 上线了无分区谓词的 trace/actor 15s
// 重查询,并发下无上界连接池可被耗尽。本函数是临时保守默认;T7.1 配置
// 体系将三值 env 化(BRAID_DB_MAX_OPEN / MAX_IDLE / CONN_MAX_LIFETIME),
// 换装点即四处调用位。
//
// 取值口径:MaxOpen=25 与 dev compose 单 MySQL 实例的默认 max_connections
// (151)错峰容纳四入口(4×25=100 < 151,留运维余量);Lifetime=5min 消化
// MySQL wait_timeout 与 LB 轮转下的陈旧连接;Idle=MaxOpen 使突发后不缩池
// 抖动(低流量部署可由 T7.1 调低)。
const (
	maxIdleConns    = 25
	connMaxLifetime = 5 * time.Minute
)

func ConfigurePool(dbh *sql.DB) {
	dbh.SetMaxOpenConns(25)
	dbh.SetMaxIdleConns(maxIdleConns)
	dbh.SetConnMaxLifetime(connMaxLifetime)
}
