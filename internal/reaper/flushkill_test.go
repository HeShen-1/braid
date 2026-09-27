package reaper

// T8.1 reaper 于 flush 中途 kill:子进程跑真 reaper.Run(flush 循环毫秒级
// 压缩),父进程批量暂存带双 trace 列的审计行 → 观测排水中 SIGKILL →
// 重启搬运(FlushAll)→ 断言恰一次、两 trace 列保真、暂存排空。
//
// kill 落点语义:flush 的 {INSERT, DELETE} 同事务(D-t61)使落点(事务前/
// 中/后)对暂存一致性不可区分——中窗回滚 = 行留暂存,提交后 = 行已落账且
// 暂存已删;恢复侧预过滤 + 裸 INSERT 使两态都收敛到恰一次。与 audit 包
// TestFlushCrashRedoConverges 互补:那臂确定性构造「已落账但暂存未删」
// 残局(重放流派),本臂真杀进程(残局落点由 MySQL 原子性决定,断言
// 对任意落点成立)。
//
// trace 连续性(T6.2 增补):暂存行×audit_log 两 trace 列全管道搬运,kill
// 与重放不得丢列/换值——NULL/带 trace/带 linked 三形态全覆盖逐行对账。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"braid/internal/audit"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
)

// flushChildEnv:T8.1 flush-kill 子进程分支标记。
const flushChildEnv = "BRAID_REAPER_FLUSH_CHILD"

// flushChildMain:子进程 = 真 reaper.Run(READY = flush 循环已活,首轮立即
// 搬运);stdin EOF(父进程先死)→ 收摊退出。纯 MySQL 面,不触 Redis
// (Locker nil = leaderless,Run 照常)。
func flushChildMain() {
	d, err := sqltest.OpenAt(reaperSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: mysql: %v\n", err)
		os.Exit(3)
	}
	defer d.Close() //nolint:errcheck // kill 路径到不了
	ids, err := snowflake.New(uint16(os.Getpid() & 0x3ff))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: snowflake: %v\n", err)
		os.Exit(4)
	}
	sched, err := scheduler.New(scheduler.Deps{DB: d, IDs: ids, Queue: "flush-child"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: scheduler: %v\n", err)
		os.Exit(5)
	}
	r, err := New(Deps{DB: d, Sched: sched, IDs: ids}, Config{
		ScanInterval:       2 * time.Second, // 扫描面与本臂无关,放缓降噪
		DeadGrace:          time.Hour,
		ExecStale:          time.Hour,
		PendingTTL:         time.Hour,
		DispatchStale:      time.Hour,
		AuditRetention:     24 * time.Hour,
		AuditFlushInterval: 50 * time.Millisecond, // 压缩 tick
		AuditFlushBatch:    50,                    // n=300 ⇒ ~6 轮排水窗
		JanitorInterval:    time.Hour,
		OutboxBatch:        100,
		Queue:              "flush-child",
		ID:                 "flush-child",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: reaper: %v\n", err)
		os.Exit(6)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { // stdin EOF(父进程先死)→ 收摊退出,不留孤儿
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	fmt.Println("READY") // os.Stdout 无缓冲,直写管道
	if err := r.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "child: run: %v\n", err)
		os.Exit(7)
	}
}

// TestFlushKillMidDrainReplaysExactlyOnce:排水中 SIGKILL → 恢复搬运 →
// 恰一次 + 双 trace 列保真 + 暂存排空。
func TestFlushKillMidDrainReplaysExactlyOnce(t *testing.T) {
	d := sqltest.MustOpenAt(t, reaperSchema)
	sd := seedReap(t, d, 0, 0, 1) // 最小执行链(心跳新鲜,扫描不处置)
	// seedReap 的 Cleanup 已覆盖 audit_log/audit_pending 清障。

	// 批量暂存:NULL trace / 带 trace_id / 带 linked 三形态全覆盖。
	const n = 300
	type srow struct {
		id            uint64
		trace, linked string
	}
	want := make(map[uint64]srow, n)
	ctx := context.Background()
	if err := engine.WithTx(ctx, d, func(q engine.DBTX) error {
		for i := range n {
			id, err := mustTestIDs(t).Next()
			if err != nil {
				return fmt.Errorf("gen id: %w", err)
			}
			r := srow{id: id}
			eventType := audit.EventNodeStateChanged
			switch {
			case i%2 == 0:
				r.trace = fmt.Sprintf("%032x", id)
			case i%7 == 0:
				// 段首形:linked 仅 approval.decided/decision_denied 词表许可
				r.linked = fmt.Sprintf("%032x", id+1)
				eventType = audit.EventApprovalDecided
				r.trace = fmt.Sprintf("%032x", id) // 段首新链 Z + linked 指 root 形
			}
			want[id] = r
			if err := audit.Enqueue(ctx, q, audit.Event{
				ID: id, EventType: eventType, ExecutionID: sd.execID,
				NodeExecID: sd.nodeExecID, NodeID: "n1", AttemptID: sd.attID,
				ActorType: audit.ActorWorker, ActorID: "w1",
				Payload: []byte(`{}`),
				TraceID: r.trace, LinkedTraceID: r.linked,
			}); err != nil {
				return fmt.Errorf("stage %d: %w", id, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("stage rows: %v", err)
	}
	var stagedBefore int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, sd.execID).
		Scan(&stagedBefore); err != nil || stagedBefore != n {
		t.Fatalf("staged before = %d (err %v), want %d", stagedBefore, err, n)
	}

	// 子进程启动 → READY(flush 循环活)。
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), flushChildEnv+"=1")
	stdin, err := cmd.StdinPipe() // 不预关:EOF 会触发子进程优雅收摊,污染 kill 现场
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	_ = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	childDone := make(chan struct{})
	defer func() {
		_ = cmd.Process.Kill()
		<-childDone
	}()
	go func() {
		_, _ = cmd.Process.Wait()
		close(childDone)
	}()
	ready := make(chan string, 1)
	go func() {
		line, rerr := bufio.NewReader(stdout).ReadString('\n')
		if rerr != nil {
			ready <- fmt.Sprintf("ERR %v (stderr: %s)", rerr, stderr.String())
			return
		}
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			t.Fatalf("child boot: %s", line)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("child never ready (stderr: %s)", stderr.String())
	}

	// 观测排水中(count < n)→ 立即 SIGKILL(真·未收口,落点任意)。
	killed := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var c int
		if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, sd.execID).
			Scan(&c); err == nil && c < n {
			if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("kill: %v", err)
			}
			killed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !killed {
		t.Fatalf("排水未被观测到(10s 内 audit_pending 未下降);child stderr: %s", stderr.String())
	}
	<-childDone

	// 恢复:父进程 FlushAll 排空(重启 reaper 的 flush 等价物)。
	if _, err := audit.FlushAll(ctx, d, 50, 10000); err != nil {
		t.Fatalf("recovery FlushAll: %v", err)
	}

	// ①暂存排空。
	var stagedAfter int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_pending WHERE execution_id=?`, sd.execID).
		Scan(&stagedAfter); err != nil || stagedAfter != 0 {
		t.Fatalf("staged after = %d (err %v), want 0(恢复排空)", stagedAfter, err)
	}
	// ②恰一次:终行数 == n(id 唯一 ⇒ 无重复落账)。
	var final int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE execution_id=?`, sd.execID).
		Scan(&final); err != nil || final != n {
		t.Fatalf("audit_log rows = %d (err %v), want %d(恰一次)", final, err, n)
	}
	// ③双 trace 列逐行保真(NULL/trace/linked 三形态全对账)。
	q, err := d.Query(`
SELECT id, COALESCE(trace_id,''), COALESCE(linked_trace_id,'')
  FROM audit_log WHERE execution_id=?`, sd.execID)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	defer func() { _ = q.Close() }()
	seen := 0
	for q.Next() {
		var id uint64
		var trace, linked string
		if err := q.Scan(&id, &trace, &linked); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		w, ok := want[id]
		if !ok {
			t.Fatalf("audit_log 出现非种子行 id=%d", id)
		}
		if trace != w.trace || linked != w.linked {
			t.Fatalf("行 %d trace 搬运失真:got (%q,%q), want (%q,%q)",
				id, trace, linked, w.trace, w.linked)
		}
		seen++
	}
	if err := q.Err(); err != nil {
		t.Fatalf("audit rows iter: %v", err)
	}
	if seen != n {
		t.Fatalf("对账行数 = %d, want %d", seen, n)
	}
}
