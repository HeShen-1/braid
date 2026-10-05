package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"braid/internal/db"
	"braid/internal/db/sqltest"
)

// auditReadySchema 是就绪面用例的 schema 基名;auditReadyNotReadySchema
// 是未就绪/等待族用例的独立基名——后者会把水位 DROP 回 ≤ today,独立
// schema 隔离防污染共租 cliSchema 的其它用例。自愈口径如实:DROP 后的
// 前滚只发生在跨进程首开(sqltest 的 migrated 缓存在进程内跳过
// initSchema,ensureAuditPartitions 不重跑),同进程内不自愈。
const (
	auditReadySchema         = "braid_cli_auditready"
	auditReadyNotReadySchema = "braid_cli_auditready_nr"
)

// TestAuditReadyArgvLayer 覆盖 argv 层用法错误与缺 DSN(退出码契约:
// 2 用法 / 1 运行失败;无 DB 依赖)。
func TestAuditReadyArgvLayer(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    int
		wantErr string
	}{
		{name: "unknown flag", args: []string{"-nope"}, want: ExitUsage},
		{name: "bad duration", args: []string{"-wait", "abc"}, want: ExitUsage},
		{name: "negative wait", args: []string{"-wait", "-1s"}, want: ExitUsage},
		{name: "positional junk", args: []string{"extra"}, want: ExitUsage},
		{
			name:    "missing dsn",
			args:    nil,
			want:    ExitError,
			wantErr: "BRAID_MYSQL_DSN is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			env := func(k string) string { return tt.env[k] }
			got := Run(context.Background(), append([]string{"audit-ready"}, tt.args...), env, nil, &out, &errBuf)
			if got != tt.want {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got, tt.want, errBuf.String())
			}
			if tt.wantErr != "" && !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Fatalf("stderr %q missing %q", errBuf.String(), tt.wantErr)
			}
		})
	}
}

// TestWaitReadyReadyNow 覆盖纯等待环:探针即绿时零等待与带等待均立即通过。
func TestWaitReadyReadyNow(t *testing.T) {
	probe := func() error { return nil }
	if err := waitReady(context.Background(), 0, time.Millisecond, probe); err != nil {
		t.Fatalf("wait 0: %v", err)
	}
	if err := waitReady(context.Background(), 5*time.Second, time.Millisecond, probe); err != nil {
		t.Fatalf("wait 5s: %v", err)
	}
}

// TestWaitReadyConverges 探针两败一绿,等待窗内收敛 → nil(间隔注入 1ms,
// 不吃默认 2s)。
func TestWaitReadyConverges(t *testing.T) {
	calls := 0
	probe := func() error {
		calls++
		if calls < 3 {
			return errors.New("not yet")
		}
		return nil
	}
	if err := waitReady(context.Background(), 2*time.Second, time.Millisecond, probe); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if calls != 3 {
		t.Fatalf("probe calls = %d, want 3", calls)
	}
}

// TestWaitReadyTimeout 恒败探针 + 超时 → 返回最后一次探针错误。
func TestWaitReadyTimeout(t *testing.T) {
	sentinel := errors.New("watermark low")
	probe := func() error { return sentinel }
	err := waitReady(context.Background(), 30*time.Millisecond, 5*time.Millisecond, probe)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// TestWaitReadyCtxCancel 等待期取消 → 错误返回(不挂死),且 canceled
// 因经 errors.Is 可追溯(双 %w),与最后一次探针错误并存。
func TestWaitReadyCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	probeErr := errors.New("not ready")
	probe := func() error { cancel(); return probeErr }
	done := make(chan error, 1)
	go func() { done <- waitReady(ctx, 10*time.Second, 5*time.Millisecond, probe) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want errors.Is(context.Canceled)", err)
		}
		if !errors.Is(err, probeErr) {
			t.Fatalf("err = %v, want last probe error wrapped", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady hung after ctx cancel")
	}
}

// TestAuditReadyIntegrationReady 真库就绪面:sqltest 前滚后水位 ≥ today+7,
// 全链 Run → 0,stdout 带就绪叙事。
func TestAuditReadyIntegrationReady(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, auditReadySchema)
	_ = dbh // MustOpenAt 即前滚;探测走 auditReady 自开的连接
	dsn := db.TestServerDSN() + db.TestSchema(auditReadySchema)
	var out, errBuf bytes.Buffer
	env := func(k string) string {
		if k == "BRAID_MYSQL_DSN" {
			return dsn
		}
		return ""
	}
	if got := Run(context.Background(), []string{"audit-ready"}, env, nil, &out, &errBuf); got != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got, errBuf.String())
	}
	if !strings.Contains(out.String(), "watermark covers today") {
		t.Fatalf("stdout %q missing ready narrative", out.String())
	}
}

// TestAuditReadyIntegrationNotReady 真库未就绪面:把未来分区 DROP 回
// 水位 ≤ today(0001 静态界在 2026-10-05 起恒 < today,天然确定性),
// 单探针 → 1 且 stderr 带 preflight 修复指引(start reaper);-wait 短窗
// 内无收敛同判超时,返回最后一次探针错误(接线锚:argv 的 -wait 真正
// 驱动轮询,单探针化变异在此红)。
func TestAuditReadyIntegrationNotReady(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, auditReadyNotReadySchema)
	dropFuturePartitions(t, dbh)
	dsn := db.TestServerDSN() + db.TestSchema(auditReadyNotReadySchema)
	env := func(k string) string {
		if k == "BRAID_MYSQL_DSN" {
			return dsn
		}
		return ""
	}
	var out, errBuf bytes.Buffer
	if got := Run(context.Background(), []string{"audit-ready"}, env, nil, &out, &errBuf); got != ExitError {
		t.Fatalf("single probe: exit = %d, want 1 (stderr: %s)", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "start reaper") {
		t.Fatalf("stderr %q missing fix guidance", errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	start := time.Now()
	if got := Run(context.Background(), []string{"audit-ready", "-wait", "50ms"}, env, nil, &out, &errBuf); got != ExitError {
		t.Fatalf("-wait timeout: exit = %d, want 1 (stderr: %s)", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "does not cover today") {
		t.Fatalf("-wait stderr %q missing last-probe watermark error", errBuf.String())
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("-wait returned after %v, want >= 50ms (polling budget honored)", elapsed)
	}
}

// TestAuditReadyIntegrationWaitConverges 全链收敛锚(对抗审计 M1/M4):
// 未就绪 schema 上起 `audit-ready -wait 5s`,窗内并发补一条覆盖明日的
// 分区(janitor 同式单条 ADD),断言退出 0——钉住 argv -wait →
// waitReady 轮询的整条接线;把 *wait 中性化的变异(单探针)在补分区
// 前即退 1,本用例红。
func TestAuditReadyIntegrationWaitConverges(t *testing.T) {
	dbh := sqltest.MustOpenAt(t, auditReadyNotReadySchema)
	dropFuturePartitions(t, dbh)
	dsn := db.TestServerDSN() + db.TestSchema(auditReadyNotReadySchema)
	env := func(k string) string {
		if k == "BRAID_MYSQL_DSN" {
			return dsn
		}
		return ""
	}
	// 窗内补水位:明日分区一条即覆盖今日(preflight 只要求 bound > today)。
	topUp := func() {
		time.Sleep(300 * time.Millisecond)
		var today time.Time
		if err := dbh.QueryRow(`SELECT CAST(NOW(3) AS DATE)`).Scan(&today); err != nil {
			t.Errorf("topup read db date: %v", err)
			return
		}
		next := today.AddDate(0, 0, 1)
		if _, err := dbh.Exec(fmt.Sprintf(
			"ALTER TABLE audit_log ADD PARTITION (PARTITION p%s VALUES LESS THAN ('%s'))",
			today.Format("20060102"), next.Format("2006-01-02"))); err != nil {
			t.Errorf("topup add partition: %v", err)
		}
	}
	go topUp()
	var out, errBuf bytes.Buffer
	if got := Run(context.Background(), []string{"audit-ready", "-wait", "5s"}, env, nil, &out, &errBuf); got != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got, errBuf.String())
	}
	if !strings.Contains(out.String(), "watermark covers today") {
		t.Fatalf("stdout %q missing ready narrative", out.String())
	}
}

// dropFuturePartitions 把 audit_log 水位 DROP 回 ≤ today:列 I_S 取全部
// 日期边界 > today 的分区逐个 DROP(janitor 的保留期回收面同款语句;
// 前滚自愈只在跨进程首开发生——进程内 sqltest migrated 缓存跳过
// initSchema,故用例自 DROP 自不清,靠独立 schema 隔离)。
func dropFuturePartitions(t *testing.T, dbh *sql.DB) {
	t.Helper()
	var today sql.NullTime // preflight/janitor 同口径:DB 时钟(NOW(3) 会话 UTC)
	if err := dbh.QueryRow(`SELECT CAST(NOW(3) AS DATE)`).Scan(&today); err != nil {
		t.Fatalf("read db date: %v", err)
	}
	rows, err := dbh.Query(`
SELECT PARTITION_NAME, PARTITION_DESCRIPTION
  FROM INFORMATION_SCHEMA.PARTITIONS
 WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='audit_log'`)
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name, desc sql.NullString
		if err := rows.Scan(&name, &desc); err != nil {
			t.Fatalf("scan partition row: %v", err)
		}
		bound, err := time.ParseInLocation("2006-01-02", strings.Trim(desc.String, "' \t"), time.UTC)
		if err != nil || !bound.After(today.Time) {
			continue // 非日期边界(p_min 等)或 ≤ today:保留
		}
		names = append(names, name.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("partition rows: %v", err)
	}
	// 空集合法:同 schema 前一用例已 DROP 光(未就绪态已成立);跨进程
	// 首开的前滚只会发生在本进程首次打开时,进程内不回填。
	for _, n := range names {
		if _, err := dbh.Exec("ALTER TABLE audit_log DROP PARTITION " + n); err != nil {
			t.Fatalf("drop partition %s: %v", n, err)
		}
	}
}
