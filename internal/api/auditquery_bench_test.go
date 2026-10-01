package api

// T15.5 审计查询基准(D-2026-10-01-t155-bench-baseline;D-t63#3
// audit_trace_map 旁路表的裁定输入):trace/actor 维度无分区谓词 → 逐分区
// 本地索引探测(0001 DDL 钦定取舍),本基准量化该成本随体量/分区数的曲线。
// 复跑:
//
//	go test ./internal/api/ -bench BenchmarkAuditQuery -benchtime 10x -run '^$' -timeout 1h
//
// 口径注记:
//   - 种子 = 构造式 bulk INSERT(哈希链列不参与查询面,占位值;行数/分区
//     分布/索引列真值才是延迟的变量——与 audit.Append 真写路径的写吞吐
//     不可比,本基准只测读面);
//   - 分区 = sqltest 前滚后的现有窗口(引导分区 + 今日+7),日期散布即
//     分区散布,零 DDL;
//   - 形状:每行 20 行/执行、1 trace/执行、200 actor 均摊;hit/miss 与
//     深页游标走页(50 页/页大小 20)四形态。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/snowflake"
)

// auditBenchDates 返回种子可用的 exec_date 序列(sqltest 已前滚的分区窗
// 内,按日散布 = 按分区散布)。窗口 = 今日-7 .. 今日+5(0001 引导分区 +
// ensureAuditPartitions 前滚),静态钳制防时钟漂移越过窗口边。
func auditBenchDates(now time.Time) []string {
	var out []string
	for i := -7; i <= 5; i++ {
		out = append(out, now.AddDate(0, 0, i).Format("2006-01-02"))
	}
	return out
}

// seedAuditBulk 构造式 bulk INSERT rows 行审计:event_at/id 严格同序
// (keyset 全序前提),每 20 行一执行(一 trace),200 actor 均摊,
// exec_date 按 dates 轮转散布。返回 hit 锚点(trace 与 actor 各一)。
func seedAuditBulk(tb testing.TB, dbh *sql.DB, ids *snowflake.Generator, rows int, dates []string) (hitTrace, hitActor string) {
	tb.Helper()
	const (
		rowsPerExec = 20
		actors      = 200
		batch       = 5000
	)
	// 硬杀残留清扫:种子键确定性,旧行不清会撞 PK 或污染命中测量。
	if _, err := dbh.Exec(`DELETE FROM audit_log WHERE actor_id LIKE 'bench-a%'`); err != nil {
		tb.Fatalf("sweep bench audit rows: %v", err)
	}
	base := time.Now().Add(-48 * time.Hour)
	execBase := int64(9_000_000_000_000_000) // 审计行 execution_id 无 FK;独立段位防与真执行行纠缠
	var idBase uint64
	if v, err := ids.Next(); err != nil {
		tb.Fatalf("gen id: %v", err)
	} else {
		idBase = v
	}
	stmt := `INSERT INTO audit_log
	  (id, exec_date, event_at, event_type, execution_id, actor_type, actor_id, trace_id, payload)
	  VALUES `
	var vals string
	args := make([]any, 0, batch*9)
	flush := func() {
		if len(args) == 0 {
			return
		}
		if _, err := dbh.Exec(stmt+vals[:len(vals)-1], args...); err != nil {
			tb.Fatalf("bulk insert audit: %v", err)
		}
		vals = ""
		args = args[:0]
	}
	for i := 0; i < rows; i++ {
		id := idBase + uint64(i)
		evAt := base.Add(time.Duration(i) * time.Millisecond)
		date := dates[(i/rowsPerExec)%len(dates)]
		actor := fmt.Sprintf("bench-a%03d", (i/rowsPerExec)%actors)
		trace := fmt.Sprintf("%032x", 0x900000+i/rowsPerExec)
		if hitTrace == "" {
			hitTrace, hitActor = trace, actor
		}
		vals += "(?,?,?,?,?,?,?,?,?),"
		args = append(args, id, date, evAt.Format("2006-01-02 15:04:05.000"),
			"llm.call.completed", execBase+int64(i/rowsPerExec), "worker", actor, trace, "{}")
		if len(args) >= batch*9 {
			flush()
		}
	}
	flush()
	tb.Cleanup(func() {
		_, _ = dbh.Exec(`DELETE FROM audit_log WHERE actor_id LIKE 'bench-a%'`)
	})
	return hitTrace, hitActor
}

// BenchmarkAuditQuery:trace/actor 维度查询延迟曲线(D-t63#3 裁定输入)。
// v= 行体量(各档独立子基准,行集在本档结束即清——上档残留会污染下档
// 体量);case= hit/miss/deepwalk(50 页 × 20 行/页)。
func BenchmarkAuditQuery(b *testing.B) {
	for _, v := range []int{200_000, 1_000_000} {
		b.Run(fmt.Sprintf("v=%d", v), func(b *testing.B) {
			dbh := sqltest.MustOpenAt(b, "braid_api_test")
			ids, err := snowflake.New(3)
			if err != nil {
				b.Fatalf("snowflake: %v", err)
			}
			st := NewStore(ids)
			hitTrace, hitActor := seedAuditBulk(b, dbh, ids, v, auditBenchDates(time.Now()))
			ctx := context.Background()
			missTrace := fmt.Sprintf("%032x", 0xABCDEF)
			missActor := "bench-zz-none"

			runCase := func(name string, fn func(b *testing.B, dbh *sql.DB)) {
				b.Run("case="+name, func(b *testing.B) {
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						fn(b, dbh)
					}
				})
			}
			runCase("trace-hit", func(b *testing.B, dbh *sql.DB) {
				if _, _, err := st.ListAuditEvents(ctx, dbh, AuditQuery{TraceID: hitTrace}, nil, 20); err != nil {
					b.Fatal(err)
				}
			})
			runCase("trace-miss", func(b *testing.B, dbh *sql.DB) {
				if _, _, err := st.ListAuditEvents(ctx, dbh, AuditQuery{TraceID: missTrace}, nil, 20); err != nil {
					b.Fatal(err)
				}
			})
			runCase("actor-miss", func(b *testing.B, dbh *sql.DB) {
				if _, _, err := st.ListAuditEvents(ctx, dbh, AuditQuery{ActorID: missActor}, nil, 20); err != nil {
					b.Fatal(err)
				}
			})
			runCase("actor-hit", func(b *testing.B, dbh *sql.DB) {
				if _, _, err := st.ListAuditEvents(ctx, dbh, AuditQuery{ActorID: hitActor}, nil, 20); err != nil {
					b.Fatal(err)
				}
			})
			runCase("actor-deepwalk", func(b *testing.B, dbh *sql.DB) {
				var cur *AuditCursor
				pages := 0
				for pages < 50 {
					res, next, err := st.ListAuditEvents(ctx, dbh, AuditQuery{ActorID: hitActor}, cur, 20)
					if err != nil {
						b.Fatal(err)
					}
					_ = res
					if next == nil {
						break
					}
					cur = next
					pages++
				}
				b.ReportMetric(float64(pages), "pages/op")
			})
		})
	}
}
