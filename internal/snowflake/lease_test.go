package snowflake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"braid/internal/db/sqltest"
)

// 本文件 = 租约面集成测试(t13 栈 MySQL;栈不可达自动 skip,CI
// BRAID_TEST_REQUIRE_STACK=1 下不可达即红)。口径真源 = D-t151 决策 2。

// testTTL 亚秒 TTL 让续期/lost 路径在测试时限内可观测(续期间隔 = TTL/3)。
const testTTL = 300 * time.Millisecond

func openLeaseDB(t *testing.T) *sql.DB {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_lease_test")
	clearLeases(t, dbh)
	return dbh
}

func clearLeases(t *testing.T, dbh *sql.DB) {
	t.Helper()
	if _, err := dbh.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
		t.Fatalf("clear leases: %v", err)
	}
}

// insertLease 直插一行测试脚手架(绕过 acquire 语义;expires 偏移可负)。
func insertLease(t *testing.T, dbh *sql.DB, node uint16, holder string, expiresIn time.Duration) {
	t.Helper()
	if _, err := dbh.Exec(
		`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at)
		 VALUES (?, ?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND))`,
		node, KindWorker, holder, expiresIn.Microseconds()); err != nil {
		t.Fatalf("insert lease fixture: %v", err)
	}
}

func leaseRow(t *testing.T, dbh *sql.DB, node uint16) (kind, holder string, expires time.Time, capabilities any, ok bool) {
	t.Helper()
	err := dbh.QueryRow(
		`SELECT kind, holder_id, expires_at, capabilities FROM snowflake_node_lease WHERE node_id = ?`, node,
	).Scan(&kind, &holder, &expires, &capabilities)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", time.Time{}, nil, false
	}
	if err != nil {
		t.Fatalf("read lease row: %v", err)
	}
	return kind, holder, expires, capabilities, true
}

func leaseCfg(kind LeaseKind, holder string, spec NodeSpec) LeaseConfig {
	return LeaseConfig{Kind: kind, Holder: holder, Spec: spec, TTL: testTTL}
}

// --- 显式 claim(含内建默认 1/2 视同显式) ---------------------------------

// TestLeaseExplicitFresh:显式号空位直达 INSERT,行落库(capabilities 恒
// NULL = 未宣告,T15.6 消费面)。
func TestLeaseExplicitFresh(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()
	l, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	defer func() { _ = l.Release(context.Background()) }()
	if l.Node() != 5 {
		t.Fatalf("node = %d, want 5", l.Node())
	}
	kind, holder, _, caps, ok := leaseRow(t, dbh, 5)
	if !ok || kind != "api" || holder != "api:h1" {
		t.Fatalf("row = (%q,%q,%v), want (api,api:h1,present)", kind, holder, ok)
	}
	if caps != nil {
		t.Fatalf("capabilities = %v, want NULL(未宣告 fail-closed)", caps)
	}
}

// TestLeaseExplicitReclaim:同 holder 复占即时(正常重启复航)——行在场
// 未过期,同 kind:holder 直达 UPDATE 接管,不等待。
func TestLeaseExplicitReclaim(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()
	first, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
	if err != nil {
		t.Fatalf("first StartLease: %v", err)
	}
	defer func() { _ = first.Release(context.Background()) }()
	second, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
	if err != nil {
		t.Fatalf("reclaim StartLease: %v", err)
	}
	defer func() { _ = second.Release(context.Background()) }()
	if second.Node() != 5 {
		t.Fatalf("reclaimed node = %d, want 5", second.Node())
	}
}

// TestLeaseExplicitHeldByOther:被他持 = 拒启;错误带持有者与到期详情 +
// runbook 提示(清行应急)。
func TestLeaseExplicitHeldByOther(t *testing.T) {
	dbh := openLeaseDB(t)
	insertLease(t, dbh, 5, "worker:other", testTTL*20)
	_, err := StartLease(context.Background(), dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld", err)
	}
	for _, want := range []string{"worker:other", "UPGRADE"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q missing %q(诊断详情/runbook 提示)", err, want)
		}
	}
}

// TestLeaseExplicitExpiredTakeover:过期行 = 懒回收,显式 claim CAS 接管。
func TestLeaseExplicitExpiredTakeover(t *testing.T) {
	dbh := openLeaseDB(t)
	insertLease(t, dbh, 7, "worker:dead", -time.Second)
	l, err := StartLease(context.Background(), dbh, leaseCfg(KindReaper, "r1", Explicit(7)))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	defer func() { _ = l.Release(context.Background()) }()
	if _, holder, _, _, ok := leaseRow(t, dbh, 7); !ok || holder != "reaper:r1" {
		t.Fatalf("row holder = %q ok=%v, want reaper:r1", holder, ok)
	}
}

// --- auto 池 ----------------------------------------------------------------

// TestLeaseAutoLinear:空池 → 0;live 跳过、过期行接管、线性取最小空闲号。
func TestLeaseAutoLinear(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()

	l0, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "a0", Auto()))
	if err != nil {
		t.Fatalf("empty pool StartLease: %v", err)
	}
	defer func() { _ = l0.Release(context.Background()) }()
	if l0.Node() != 0 {
		t.Fatalf("empty pool node = %d, want 0", l0.Node())
	}

	// 复位后:0/1 live(他持)、2 过期(dead)、3+ 缺行 → 期望接管 2。
	clearLeases(t, dbh)
	insertLease(t, dbh, 0, "worker:w0", testTTL*20)
	insertLease(t, dbh, 1, "worker:w1", testTTL*20)
	insertLease(t, dbh, 2, "worker:dead", -time.Second)
	l2, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "a2", Auto()))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	defer func() { _ = l2.Release(context.Background()) }()
	if l2.Node() != 2 {
		t.Fatalf("node = %d, want 2(线性:0/1 live 跳过,2 过期接管)", l2.Node())
	}
}

// TestLeaseAutoExhausted:池满(1024 行全 live)→ 拒启,不烧重试预算。
func TestLeaseAutoExhausted(t *testing.T) {
	dbh := openLeaseDB(t)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at) VALUES `)
	for n := 0; n <= int(MaxNodeID); n++ {
		if n > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "(%d,'worker','fill-%d',DATE_ADD(NOW(3), INTERVAL 60000000 MICROSECOND))", n, n)
	}
	if _, err := dbh.Exec(sb.String()); err != nil {
		t.Fatalf("fill pool: %v", err)
	}
	if _, err := StartLease(context.Background(), dbh, leaseCfg(KindAPI, "a", Auto())); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("err = %v, want ErrPoolExhausted", err)
	}
}

// --- 能力宣告(T15.6 mode=all 硬闸面;D-t151 决策 2/6) -----------------------

// declaredCfg 在基础配置上叠能力宣告(宣告时点 = acquire 启动期,无旋钮)。
func declaredCfg(kind LeaseKind, holder string, spec NodeSpec) LeaseConfig {
	cfg := leaseCfg(kind, holder, spec)
	cfg.Capabilities = []string{CapApprovalModeAll}
	return cfg
}

// TestLeaseCapabilitiesWrite:宣告随 acquire 三写路径落库(INSERT 抢注 /
// 同 holder 复占 / 过期接管),行内容始终反映本次 acquire 的二进制——空宣告
// 复占也回写 NULL(自愈:宣告面不残留前身二进制的陈旧值);续期只推
// expires_at,宣告保值。
func TestLeaseCapabilitiesWrite(t *testing.T) {
	t.Run("fresh insert declares", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(context.Background(), dbh, declaredCfg(KindAPI, "h1", Explicit(5)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		_, _, _, caps, ok := leaseRow(t, dbh, 5)
		if !ok {
			t.Fatalf("lease row missing")
		}
		if b, ok := caps.([]byte); !ok || string(b) != `["approval_mode_all"]` {
			t.Fatalf("capabilities = %#v, want JSON [\"approval_mode_all\"]", caps)
		}
	})
	t.Run("reclaim redeclares and empty reclaims reset to NULL", func(t *testing.T) {
		dbh := openLeaseDB(t)
		ctx := context.Background()
		first, err := StartLease(ctx, dbh, declaredCfg(KindAPI, "h1", Explicit(5)))
		if err != nil {
			t.Fatalf("declared StartLease: %v", err)
		}
		defer func() { _ = first.Release(ctx) }()
		// 空宣告复占(旧二进制回滚形态的镜像)必须命中 CAS 复占 UPDATE 路径
		//(行在场未过期 + 同 holder):不先 Release,二启即复占——行内容
		// 回写 NULL,不残留前身宣告(fail-closed 方向自愈)。
		second, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
		if err != nil {
			t.Fatalf("undeclared reclaim StartLease: %v", err)
		}
		defer func() { _ = second.Release(ctx) }()
		_, holder, _, caps, ok := leaseRow(t, dbh, 5)
		if !ok || holder != "api:h1" {
			t.Fatalf("row = (%q,%v), want api:h1 present(复占不换 holder)", holder, ok)
		}
		if caps != nil {
			t.Fatalf("capabilities = %#v after empty reclaim, want NULL", caps)
		}
		// 再以宣告配置复占:行回到已宣告。
		third, err := StartLease(ctx, dbh, declaredCfg(KindAPI, "h1", Explicit(5)))
		if err != nil {
			t.Fatalf("redeclared reclaim StartLease: %v", err)
		}
		defer func() { _ = third.Release(ctx) }()
		if _, _, _, caps, _ := leaseRow(t, dbh, 5); caps == nil {
			t.Fatalf("capabilities = NULL after redeclared reclaim, want declared")
		}
	})
	t.Run("expired takeover declares", func(t *testing.T) {
		dbh := openLeaseDB(t)
		insertLease(t, dbh, 7, "worker:dead", -time.Second)
		l, err := StartLease(context.Background(), dbh, declaredCfg(KindReaper, "r1", Explicit(7)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if _, _, _, caps, _ := leaseRow(t, dbh, 7); caps == nil {
			t.Fatalf("capabilities = NULL after takeover, want declared")
		}
	})
	t.Run("renewal preserves declaration", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(context.Background(), dbh, declaredCfg(KindWorker, "w1", Auto()))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		time.Sleep(testTTL * 2) // ≥2 个续期 tick
		if _, _, _, caps, _ := leaseRow(t, dbh, l.Node()); caps == nil {
			t.Fatalf("capabilities lost after renewal, want preserved")
		}
	})
	t.Run("auto claim declares", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(context.Background(), dbh, declaredCfg(KindWorker, "w2", Auto()))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if _, _, _, caps, _ := leaseRow(t, dbh, l.Node()); caps == nil {
			t.Fatalf("capabilities = NULL after auto claim, want declared")
		}
	})
}

// TestInFleet:在场查询只返回未过期行(kind 三类;NULL = 未宣告,解析为
// nil 切片),按 node_id 升序;过期行与缺行一律不在场。
func TestInFleet(t *testing.T) {
	dbh := openLeaseDB(t)
	// 行面:0 = 在场已宣告(worker)/ 1 = 在场未声明(api,NULL)/ 2 = 过期
	// 已宣告(不在场)/ 3 = 缺行。
	l0, err := StartLease(context.Background(), dbh, declaredCfg(KindWorker, "w0", Explicit(0)))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	defer func() { _ = l0.Release(context.Background()) }()
	l1, err := StartLease(context.Background(), dbh, leaseCfg(KindAPI, "a1", Explicit(1)))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	defer func() { _ = l1.Release(context.Background()) }()
	insertLease(t, dbh, 2, "reaper:dead", -time.Second)

	got, err := InFleet(context.Background(), dbh)
	if err != nil {
		t.Fatalf("InFleet: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("fleet size = %d, want 2(过期行不在场): %+v", len(got), got)
	}
	if got[0].Node != 0 || got[0].Kind != KindWorker || got[0].HolderID != "worker:w0" {
		t.Fatalf("got[0] = %+v, want node 0 worker:w0", got[0])
	}
	if len(got[0].Capabilities) != 1 || got[0].Capabilities[0] != CapApprovalModeAll {
		t.Fatalf("got[0].Capabilities = %v, want [%s]", got[0].Capabilities, CapApprovalModeAll)
	}
	if got[1].Node != 1 || got[1].Kind != KindAPI || got[1].Capabilities != nil {
		t.Fatalf("got[1] = %+v, want node 1 api 未宣告(nil)", got[1])
	}
	if got[1].ExpiresAt.IsZero() {
		t.Fatalf("got[1].ExpiresAt zero, want row expiry(诊断详情面)")
	}
}

// TestHolderLive(t20.4 清扫前活性复核读面):同 kind+identity 的在场行命中
// (判活全 DB 端 NOW(3),与 acquire/InFleet 同口径);无行/过期/异 kind/
// 异 identity ⇒ 无活体。holder_id = kind:identity 拼装单源在本包,同宿主
// 跨类(api/worker 同机)不互认。多行同持(部署契约违例形态)取最晚到期
// 一行供诊断。DB 错误原样上抛(调用侧 fail-safe:复核不可达 ⇒ 不清)。
func TestHolderLive(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()
	insertKind := func(node uint16, kind LeaseKind, identity string, expiresIn time.Duration) {
		t.Helper()
		if _, err := dbh.Exec(
			`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at)
			 VALUES (?, ?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND))`,
			node, kind, string(kind)+":"+identity, expiresIn.Microseconds()); err != nil {
			t.Fatalf("insert fixture: %v", err)
		}
	}
	// insertKindCaps 同上但带能力宣告(真 worker 行恒非 NULL——复核读面的
	// capabilities 解析分支锚)。
	insertKindCaps := func(node uint16, kind LeaseKind, identity string, expiresIn time.Duration, caps string) {
		t.Helper()
		if _, err := dbh.Exec(
			`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at, capabilities)
			 VALUES (?, ?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), ?)`,
			node, kind, string(kind)+":"+identity, expiresIn.Microseconds(), caps); err != nil {
			t.Fatalf("insert fixture: %v", err)
		}
	}

	t.Run("live peer matched", func(t *testing.T) {
		clearLeases(t, dbh)
		insertKindCaps(7, KindWorker, "w1", 20*time.Second, `["`+CapApprovalModeAll+`"]`)
		got, err := HolderLive(ctx, dbh, KindWorker, "w1")
		if err != nil {
			t.Fatalf("HolderLive: %v", err)
		}
		if got == nil || got.Node != 7 || got.HolderID != "worker:w1" || got.Kind != KindWorker {
			t.Fatalf("got = %+v, want node 7 worker:w1", got)
		}
		if len(got.Capabilities) != 1 || got.Capabilities[0] != CapApprovalModeAll {
			t.Fatalf("Capabilities = %v, want [%s](宣告行解析,InFleet 同款)", got.Capabilities, CapApprovalModeAll)
		}
		if got.ExpiresAt.IsZero() {
			t.Fatalf("ExpiresAt zero, want row expiry(诊断详情面)")
		}
	})
	deadCases := []struct {
		name string
		seed func()
	}{
		{"expired row is dead", func() { insertKind(7, KindWorker, "w1", -time.Second) }},
		{"absent row is dead", func() {}},
		{"same host different kind is not matched", func() { insertKind(7, KindReaper, "w1", 20*time.Second) }},
		{"different identity is not matched", func() { insertKind(7, KindWorker, "w2", 20*time.Second) }},
		// 表缺省 ai_ci 大小写不敏感;判等显式 bin 字节精确——docker label
		// 过滤同语义,W1 与 w1 是互不相及的两个 owner,不互判活体。
		{"case-variant identity is a different owner", func() { insertKind(7, KindWorker, "W1", 20*time.Second) }},
	}
	for _, tc := range deadCases {
		t.Run(tc.name, func(t *testing.T) {
			clearLeases(t, dbh)
			tc.seed()
			got, err := HolderLive(ctx, dbh, KindWorker, "w1")
			if err != nil || got != nil {
				t.Fatalf("got = (%+v, %v), want (nil, nil)(死 owner:清扫照走)", got, err)
			}
		})
	}
	t.Run("two live rows return latest expiry", func(t *testing.T) {
		clearLeases(t, dbh)
		insertKind(7, KindWorker, "w1", 10*time.Second)
		insertKind(8, KindWorker, "w1", 30*time.Second) // 违例形态:双 worker 同 owner
		got, err := HolderLive(ctx, dbh, KindWorker, "w1")
		if err != nil {
			t.Fatalf("HolderLive: %v", err)
		}
		if got == nil || got.Node != 8 {
			t.Fatalf("got = %+v, want node 8(最晚到期,诊断详情)", got)
		}
	})
	t.Run("db error propagates", func(t *testing.T) {
		closed := sqltest.MustOpenAt(t, "braid_lease_test")
		if err := closed.Close(); err != nil {
			t.Fatalf("close fixture: %v", err)
		}
		if _, err := HolderLive(ctx, closed, KindWorker, "w1"); err == nil {
			t.Fatalf("err = nil, want query error propagation(调用侧 fail-safe)")
		}
	})
	// 写读配对锚(cmd/worker 装配契约):StartLease 的 Holder 实参与
	// HolderLive 的 identity 实参同串 ⇒ 必命中自身行(Holder 对齐
	// cfg.WorkerID 的 API 面契约,回退 hostname 即失配——锚见 cmd/worker)。
	t.Run("acquire holder identity pairing round-trip", func(t *testing.T) {
		clearLeases(t, dbh)
		l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Auto()))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		got, err := HolderLive(ctx, dbh, KindWorker, "w1")
		if err != nil || got == nil || got.Node != l.Node() || got.HolderID != "worker:w1" {
			t.Fatalf("got = (%+v, %v), want own row node %d worker:w1(Holder 实参 == identity 实参契约)", got, err, l.Node())
		}
	})
}

// --- 续期 / lost / 停发闸 ----------------------------------------------------

// TestLeaseRenewalAndLost:续期把 expires_at 向前推;行被他持/续期落空 →
// lost 触发,且守护生成器先停发(Lost 可见时 Block 已生效)。
func TestLeaseRenewalAndLost(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()
	l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Auto()))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	g, err := New(l.Node())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Guard(g)

	_, _, exp0, _, ok := leaseRow(t, dbh, l.Node())
	if !ok {
		t.Fatalf("lease row missing")
	}
	// 续期间隔 = TTL/3;睡 2+ 个续期 tick 后,到期时刻必须被推向前。
	time.Sleep(testTTL * 2)
	_, _, exp1, _, _ := leaseRow(t, dbh, l.Node())
	if !exp1.After(exp0) {
		t.Fatalf("expires_at not renewed: %v -> %v", exp0, exp1)
	}

	// 他持抢注(reaper 接管/runbook 清行后重占同构):续期 UPDATE 落空 → lost。
	if _, err := dbh.Exec(
		`UPDATE snowflake_node_lease SET holder_id = 'worker:thief' WHERE node_id = ?`, l.Node()); err != nil {
		t.Fatalf("steal lease: %v", err)
	}
	select {
	case err, ok := <-l.Lost():
		if !ok || err == nil {
			t.Fatalf("lost channel: err=%v ok=%v, want non-nil error", err, ok)
		}
		// 停发闸先于 lost 可见(D-t151 决策 2:停止签发 + 响亮)。
		if _, err := g.Next(); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("Next after lost: %v, want ErrLeaseLost", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("lost not signalled within 3s after theft")
	}
}

// TestLeaseRelease:SIGTERM 释放 = DELETE 本行 + 续期停(lost 不触发)。
func TestLeaseRelease(t *testing.T) {
	dbh := openLeaseDB(t)
	l, err := StartLease(context.Background(), dbh, leaseCfg(KindAPI, "h9", Explicit(9)))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	if err := l.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, _, _, _, ok := leaseRow(t, dbh, 9); ok {
		t.Fatalf("row survives Release, want deleted")
	}
	select {
	case err, ok := <-l.Lost():
		t.Fatalf("Release must not signal lost(got err=%v ok=%v)", err, ok)
	default:
	}
}

// --- 重扫环(纯单元,注入 scan/claim) ---------------------------------------

// fakeLeaseOps 记录 claim 调用序列,按脚本返回结果。
type fakeLeaseOps struct {
	live    map[uint16]bool
	expired map[uint16]bool
	claims  []uint16
	failN   int // 前 failN 次 claim 返回 ErrLeaseRace
}

func (f *fakeLeaseOps) scan(context.Context) (map[uint16]bool, map[uint16]bool, error) {
	return f.live, f.expired, nil
}

func (f *fakeLeaseOps) claim(_ context.Context, node uint16) error {
	f.claims = append(f.claims, node)
	if len(f.claims) <= f.failN {
		return ErrLeaseRace
	}
	return nil
}

// TestAutoRetryLoop:竞态败者 ≤3 次退避重扫后拒启;两败一成 = 成功。
func TestAutoRetryLoop(t *testing.T) {
	ctx := context.Background()
	t.Run("recovers within budget", func(t *testing.T) {
		ops := &fakeLeaseOps{live: map[uint16]bool{0: true}, expired: map[uint16]bool{1: true}, failN: 2}
		l := &Lease{}
		node, err := l.acquireAutoLoop(ctx, ops.scan, ops.claim, func(time.Duration) {})
		if err != nil || node != 1 {
			t.Fatalf("acquireAutoLoop = (%d,%v), want (1,nil)", node, err)
		}
		if len(ops.claims) != 3 {
			t.Fatalf("claims = %d, want 3(两败一成)", len(ops.claims))
		}
	})
	t.Run("refuses after budget", func(t *testing.T) {
		ops := &fakeLeaseOps{live: map[uint16]bool{0: true}, expired: map[uint16]bool{1: true}, failN: 99}
		l := &Lease{}
		_, err := l.acquireAutoLoop(ctx, ops.scan, ops.claim, func(time.Duration) {})
		if !errors.Is(err, ErrPoolContention) {
			t.Fatalf("err = %v, want ErrPoolContention", err)
		}
		if len(ops.claims) != autoClaimAttempts {
			t.Fatalf("claims = %d, want %d", len(ops.claims), autoClaimAttempts)
		}
	})
	t.Run("exhausted fails fast without claims", func(t *testing.T) {
		full := map[uint16]bool{}
		for n := uint16(0); n <= MaxNodeID; n++ {
			full[n] = true
		}
		ops := &fakeLeaseOps{live: full}
		l := &Lease{}
		if _, err := l.acquireAutoLoop(ctx, ops.scan, ops.claim, func(time.Duration) {}); !errors.Is(err, ErrPoolExhausted) {
			t.Fatalf("err = %v, want ErrPoolExhausted", err)
		}
		if len(ops.claims) != 0 {
			t.Fatalf("claims = %d, want 0(池满不烧预算)", len(ops.claims))
		}
	})
}

// TestRenewalSurvivesShutdownCtx:SIGTERM 取消根 ctx 后续期不停(排空窗
// 续约,见 renewLoop 注释)——到期时刻持续前推,lost 不触发;只有
// Release 才停。
func TestRenewalSurvivesShutdownCtx(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "drain", Explicit(42)))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	_, _, exp0, _, _ := leaseRow(t, dbh, 42)

	cancel() // SIGTERM 形态:根 ctx 取消(= asynq 排空窗开始)
	time.Sleep(testTTL * 2)
	_, _, exp1, _, _ := leaseRow(t, dbh, 42)
	if !exp1.After(exp0) {
		t.Fatalf("renewal stopped after ctx cancel: %v -> %v(排空窗续约缺失)", exp0, exp1)
	}
	select {
	case err, ok := <-l.Lost():
		t.Fatalf("ctx cancel must not signal lost(got err=%v ok=%v)", err, ok)
	default:
	}
	if err := l.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
