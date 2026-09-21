package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// 错误分类语义(调用方只依赖 errors.Is / errors.As,不字符串匹配):
//   - ErrIllegalTransition:调用方 bug(进程内矩阵拒绝,零 DB 往返)→ 告警,不重试;
//   - ErrConflict:正常竞争(败方/已终态/version 过期)→ 重读行按真实状态重新决策,
//     不告警不自动重试(T2.5 重决策、T2.3 ErrTaskIDConflict 收敛的消费口径);
//   - ErrNotFound:行不存在,数据缺陷 → 告警,不重试;
//   - ErrInvalidSnapshot:快照非法 JSON,调用方 bug → 不触 DB;
//   - Transient(err)==true:MySQL 1213(死锁)/1205(锁等待超时)→ 唯一允许
//     抖动退避重试的一类,≤3 次。
var (
	ErrNotFound          = errors.New("engine: row not found")
	ErrConflict          = errors.New("engine: cas conflict")
	ErrIllegalTransition = errors.New("engine: illegal transition")
	ErrInvalidSnapshot   = errors.New("engine: invalid input snapshot")
)

// ConflictError 是 CAS 败方的结构化归因:期望(谓词)vs 实际(0 行后同 tx 以
// FOR UPDATE 当前读复查到的最新已提交行——普通 SELECT 在 RR 下会被本事务
// read view 污染,见 checkNodeCAS 注释)。Observed 供 T2.5 reconciler 直接
// 重决策,不必再查一次。多来源谓词(Cancel/Finish)的 Expect 为空串——期望是
// 集合不是单值,不捏造。workflow 侧无 version 列,ObservedVer 恒 0。
type ConflictError struct {
	ID          uint64
	Expect      NodeState
	ExpectVer   uint32
	Observed    NodeState
	ObservedVer uint32
}

func (e *ConflictError) Error() string {
	if e.Expect == "" { // 多来源谓词(Cancel/Finish):期望是集合,不捏造单值
		return fmt.Sprintf("engine: cas conflict on %d: observed %s(v%d)",
			e.ID, e.Observed, e.ObservedVer)
	}
	return fmt.Sprintf("engine: cas conflict on %d: expect %s(v%d), observed %s(v%d)",
		e.ID, e.Expect, e.ExpectVer, e.Observed, e.ObservedVer)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// Transient 报告 err 是否为可重试的瞬时 DB 故障(死锁 1213 / 锁等待超时 1205)。
// 仅此一类允许抖动退避重试;其余错误重试只会重复失败。
func Transient(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1213 || me.Number == 1205
	}
	return false
}

// nullTime 把应用侧零值时间规范为 SQL NULL(deadline 语义:零值 = 无截止)。
func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}
