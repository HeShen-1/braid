package relay

// T2.2 验收②「判别性故障测试」:relay 标记 DISPATCHED 前 kill -9,重启后同事件
// 重投递且下游幂等。
//
// 机制(TestMain helper-process 惯例):子进程 = 重执行测试二进制(BRAID_RELAY_CHILD=1),
// 跑**真实** relay.Run + 真实 Asynq 客户端,仅在 Enqueuer 缝外包一层包装器:
// 真实入队成功后向 stdout 写 READY 行、然后阻塞读 stdin。阻塞点是「Redis 已收、
// 标记 UPDATE 必未执行」的构造性证明(非概率时序):enqueue 返回 nil 之后代码才
// 可能走到 MarkDispatched,而包装器就卡在返回之前。
//
// 父进程读到 READY 即 kill -9(SIGKILL,不走 SIGTERM)——此刻命中崩溃矩阵的
// 窗口 3(enqueue 成功返回后、标记前;窗口 2/4 同收敛)。超时未 READY = 缺陷
// 不是 flake,fatal 并附子进程 stderr。
//
// 变异自查(实现错 → 哪条红):
//   - 标记移到 enqueue 前 / 无事务保护 → A1(行仍 PENDING、attempts=0)红;
//   - 冲突被当普通失败 → B1(重启后行滞留 PENDING+backoff)红;
//   - attempts 语义漂移(pre-enqueue 自增)→ A1 的 attempts=0 红;
//   - relay 越权写业务表 → A4 红;
//   - asynq option 错(TaskID 丢/MaxRetry 丢)→ A3/B3 的 GetTaskInfo 现场红
//     (stub 不可内省 asynq.Option,真实 Redis 是唯一取证点)。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"braid/internal/db"
	"braid/internal/db/sqltest"
	"braid/internal/redistest"
)

const childEnv = "BRAID_RELAY_CHILD=1"

// relayRedisBase:relay 包的 Redis 基号(AGENTS.md 槽位表:relay=0、
// worker=1、reaper=2;绝对 DB = worktree 槽位×4 + 基号)。
const relayRedisBase = 0

func TestMain(m *testing.M) {
	if os.Getenv("BRAID_RELAY_CHILD") == "1" {
		// kill 子进程:env 里 BRAID_TEST_REDIS_DB 已是父进程组合好的绝对号,
		// 直接用;不重跑组合(绝对号会被再当槽位解析而越界)。
		killChildMain()
		return
	}
	// T2.7:BRAID_TEST_REDIS_DB 语义 = worktree 槽位,TestMain 组合为绝对
	// DB 号(槽位×4 + relay 基号)写回同名 env——redistest 零改动,读到的
	// 即绝对号;非法槽位响亮退出(静默回退 DB 0 会并行互踩)。
	redisDB, err := db.TestRedisDB(relayRedisBase)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.Setenv("BRAID_TEST_REDIS_DB", strconv.Itoa(redisDB)); err != nil {
		fmt.Fprintf(os.Stderr, "set BRAID_TEST_REDIS_DB: %v\n", err)
		os.Exit(2)
	}
	// 父进程专属清扫:relay 轮询与 kill 子进程都是全 schema 扫描(无归属
	// 过滤),上一轮被中断留下的 PENDING 行(id 更小,ORDER BY id 先被捡走)
	// 会污染本轮的归属/计数断言——READY 甚至可能由残留行触发。本 schema 归
	// relay 包独占,启动时清空是安全的。栈不可达则留给各测试自行 skip。
	if d, err := sqltest.OpenAt(relaySchema); err == nil {
		_, _ = d.Exec(`DELETE FROM outbox`)
		_ = d.Close()
	}
	os.Exit(m.Run())
}

// killChildMain:子进程入口。MySQL/Redis 不可达 → 非零退出(父进程将看到
// READY 超时并 dump stderr,响亮失败)。
func killChildMain() {
	d, err := sqltest.OpenAt(relaySchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: mysql: %v\n", err)
		os.Exit(3)
	}
	defer d.Close() //nolint:errcheck // 子进程被 kill 时到不了这里

	eq := &killGate{inner: asynq.NewClient(redistest.Opt()), stdin: os.Stdin}
	defer eq.inner.Close() //nolint:errcheck // 同上

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // stdin EOF(父进程先死)→ 收摊退出,不留孤儿
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	cfg := DefaultConfig()
	cfg.IdleInterval = 10 * time.Millisecond
	if err := Run(ctx, Deps{DB: d, Enqueuer: eq}, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "child: relay: %v\n", err)
		os.Exit(4)
	}
}

// killGate:真实入队成功 → READY 哨兵行 → 阻塞读 stdin 一个字节 → 才返回成功。
type killGate struct {
	inner *asynq.Client
	stdin *os.File
}

func (g *killGate) EnqueueContext(ctx context.Context, t *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	info, err := g.inner.EnqueueContext(ctx, t, opts...)
	if err != nil {
		return nil, err // 失败不阻塞(父进程超时会揭示)
	}
	// READY 携带行身份(payload 内嵌 outbox_id):父进程校验停车行就是目标行,
	// 防止残留脏行触发 READY 造成「目标行全程未被碰」的假绿。
	var v struct {
		OutboxID uint64 `json:"outbox_id"`
	}
	_ = json.Unmarshal(t.Payload(), &v)
	fmt.Printf("READY %d\n", v.OutboxID) // os.Stdout 无缓冲,直写管道
	buf := make([]byte, 1)
	_, _ = g.stdin.Read(buf) // 阻塞至被 kill -9(或父进程死后 EOF)
	return info, nil
}

func (g *killGate) Close() error { return g.inner.Close() }

// TestRelayKill9BeforeMarkThenIdempotentRedelivery:验收②全链路。
func TestRelayKill9BeforeMarkThenIdempotentRedelivery(t *testing.T) {
	db := sqltest.MustOpenAt(t, relaySchema)
	opt := redistest.MustOpt(t) // Redis 不可达 → skip(栈惯例)

	f := &rfixture{t: t, db: db, execID: newID()}
	t.Cleanup(func() { mustExec(t, f.db, `DELETE FROM outbox WHERE execution_id=?`, f.execID) })
	queue := "q" + fmt.Sprint(newID())
	outboxID, taskID := f.row(queue)

	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { _ = insp.Close() })

	// ---- 阶段一:起子进程 relay,等它完成真实入队并停在标记前。----
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			ready <- fmt.Errorf("child stdout: %w (stderr: %s)", err, stderr.String())
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || fields[0] != "READY" {
			ready <- fmt.Errorf("unexpected child output %q (stderr: %s)", line, stderr.String())
			return
		}
		readyID, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || readyID != outboxID {
			ready <- fmt.Errorf("child parked on outbox row %s, want %d (stale row leaked?): %v (stderr: %s)",
				fields[1], outboxID, err, stderr.String())
			return
		}
		ready <- nil
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Fatalf("child never signaled READY: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = stdin.Close()
		t.Fatalf("READY timeout (child defect, not flake); stderr: %s", stderr.String())
	}

	// ---- kill -9 前的现场断言 ----
	// A3:任务确已在 Redis(与 READY 行构成「入队成功、标记未落」双证据),
	//     且 TaskID/Queue/MaxRetry=1 三个 option 经真实入队可取证。
	ti, err := insp.GetTaskInfo(queue, taskID)
	if err != nil {
		t.Fatalf("task not in redis before kill: %v", err)
	}
	if ti.State != asynq.TaskStatePending {
		t.Fatalf("task state = %v, want pending", ti.State)
	}
	if ti.MaxRetry != 1 {
		t.Fatalf("task MaxRetry = %d, want 1 (platform retry authority)", ti.MaxRetry)
	}

	// ---- kill -9(SIGKILL)----
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill -9: %v", err)
	}
	_ = stdin.Close()
	_ = cmd.Wait()

	// A1:行仍 PENDING,零已提交痕迹(kill ⇒ 标记事务中止)。
	status, attempts, lastErr, availReady := f.outboxRow(outboxID)
	if status != "PENDING" || attempts != 0 || lastErr.Valid || !availReady {
		t.Fatalf("A1 after kill -9: status=%s attempts=%d lastErr=%v availReady=%v", status, attempts, lastErr.Valid, availReady)
	}
	// A2:该执行域恰 1 条事件(kill 的尝试不产生新行)。
	var rowCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM outbox WHERE execution_id=?`, f.execID).Scan(&rowCount); err != nil {
		t.Fatalf("A2 count: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("A2: %d outbox rows, want 1", rowCount)
	}
	// A4:relay 未写任何业务表(只投不写业务表)。
	var bizRows int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM node_execution WHERE execution_id=?`, f.execID).Scan(&bizRows); err != nil {
		t.Fatalf("A4 count: %v", err)
	}
	if bizRows != 0 {
		t.Fatalf("A4: relay created %d business rows", bizRows)
	}

	// ---- 阶段二:重启(进程内新 relay 实例 + 真实 Asynq 客户端)----
	eq := NewAsynqEnqueuer(opt)
	t.Cleanup(func() { _ = eq.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, Deps{DB: db, Enqueuer: eq}, func() Config { c := DefaultConfig(); c.IdleInterval = 10 * time.Millisecond; return c }())
	}()

	// B1:重投撞同 TaskID 的 ErrTaskIDConflict ⇒ 收敛为 DISPATCHED(下游幂等证据)。
	//     若冲突被误当失败,行会滞留 PENDING+backoff,eventually 超时即红。
	eventually(t, 5*time.Second, "row converges to DISPATCHED after restart", func() bool {
		status, _, _, _ := f.outboxRow(outboxID)
		return status == "DISPATCHED"
	})
	// B2:attempts = 1(kill 掉的尝试无结论不计,重启标记的尝试计 1)。
	if _, attempts, _, _ = f.outboxRow(outboxID); attempts != 1 {
		t.Fatalf("B2: deliver_attempts = %d, want 1", attempts)
	}
	// B4:未被退避推到未来。
	if _, _, _, availReady = f.outboxRow(outboxID); !availReady {
		t.Fatalf("B4: available_at pushed to future on conflict convergence")
	}
	// B3:Redis 侧恰一个同 TaskID 任务(冲突收敛,无第二份)。
	ti2, err := insp.GetTaskInfo(queue, taskID)
	if err != nil {
		t.Fatalf("B3 GetTaskInfo: %v", err)
	}
	if ti2.State != asynq.TaskStatePending {
		t.Fatalf("B3: task state = %v, want pending (no worker in test)", ti2.State)
	}
	pending, err := insp.ListPendingTasks(queue)
	if err != nil {
		t.Fatalf("B3 ListPendingTasks: %v", err)
	}
	n := 0
	for _, p := range pending {
		if p.ID == taskID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("B3: %d pending tasks with id %s, want exactly 1", n, taskID)
	}
}

// TestConflictDetectionGate:冲突检测门——pinned 版本对同 TaskID 二次真实入队,
// errors.Is 必须命中 ErrTaskIDConflict。asynq 升级若改坏错误链(#1166 复发),
// 此测试先红,禁止静默升级。检测只许 errors.Is,禁止字符串匹配。
func TestConflictDetectionGate(t *testing.T) {
	opt := redistest.MustOpt(t)
	client := asynq.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })

	queue := "q" + fmt.Sprint(newID())
	taskID := fmt.Sprintf("att:%d:1", newID())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.EnqueueContext(ctx, asynq.NewTask("node.task_enqueue", []byte(`{}`)),
		asynq.TaskID(taskID), asynq.Queue(queue), asynq.MaxRetry(1))
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	_, err = client.EnqueueContext(ctx, asynq.NewTask("node.task_enqueue", []byte(`{}`)),
		asynq.TaskID(taskID), asynq.Queue(queue), asynq.MaxRetry(1))
	if !errors.Is(err, asynq.ErrTaskIDConflict) {
		t.Fatalf("second enqueue: want ErrTaskIDConflict, got %v", err)
	}
}
