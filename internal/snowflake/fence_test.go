package snowflake

// fence_test.go = 租约 fencing 面集成测试(t13 栈 MySQL;栈不可达自动
// skip)。口径真源 = D-2026-10-06-t201-lease-fencing:等值加冕语义、
// 代际递增、判等 utf8mb4_bin 统一(D-t204 :778 交接缝)。

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// leaseEpoch 读行上当前代际(直读真源,测试脚手架)。
func leaseEpoch(t *testing.T, dbh *sql.DB, node uint16) (epoch uint64, ok bool) {
	t.Helper()
	err := dbh.QueryRow(
		`SELECT fencing_epoch FROM snowflake_node_lease WHERE node_id = ?`, node,
	).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read lease epoch: %v", err)
	}
	return epoch, true
}

// setLeaseExpiry 直接改行到期时刻(可负 = 过期;脚手架,绕过续期语义)。
func setLeaseExpiry(t *testing.T, dbh *sql.DB, node uint16, expiresIn time.Duration) {
	t.Helper()
	if _, err := dbh.Exec(
		`UPDATE snowflake_node_lease SET expires_at = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND) WHERE node_id = ?`,
		expiresIn.Microseconds(), node); err != nil {
		t.Fatalf("set lease expiry: %v", err)
	}
}

// TestLeaseEpochGenerations:代际 = 任何持有变化同语句 +1——INSERT 新占
// = 1;同 holder 复占也 bump(D-t204 边界③ 复占双活:重启复占使前身
// token 即刻失效);过期接管 = +1(legacy 0 行起代 1);续期不动。
// Lease.Epoch() 必与行上真值一致(fence 等值比较的随身基准)。
func TestLeaseEpochGenerations(t *testing.T) {
	t.Run("fresh insert starts at 1", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(context.Background(), dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if l.Epoch() != 1 {
			t.Fatalf("Epoch() = %d, want 1(INSERT 新占起代)", l.Epoch())
		}
		if e, ok := leaseEpoch(t, dbh, 5); !ok || e != 1 {
			t.Fatalf("row epoch = (%d,%v), want 1", e, ok)
		}
	})
	t.Run("same-holder reclaim bumps and invalidates predecessor", func(t *testing.T) {
		dbh := openLeaseDB(t)
		ctx := context.Background()
		first, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
		if err != nil {
			t.Fatalf("first StartLease: %v", err)
		}
		defer func() { _ = first.Release(context.Background()) }()
		if first.Epoch() != 1 {
			t.Fatalf("first Epoch() = %d, want 1", first.Epoch())
		}
		second, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
		if err != nil {
			t.Fatalf("reclaim StartLease: %v", err)
		}
		defer func() { _ = second.Release(context.Background()) }()
		if second.Epoch() != 2 {
			t.Fatalf("second Epoch() = %d, want 2(复占 bump)", second.Epoch())
		}
		if e, _ := leaseEpoch(t, dbh, 5); e != 2 {
			t.Fatalf("row epoch = %d, want 2", e)
		}
		// 前身令牌即刻失效:旧 Lease 的 fence 必拒(等值语义的核心兑现)。
		if err := first.Fence(ctx, dbh); !errors.Is(err, ErrFenced) {
			t.Fatalf("predecessor Fence: %v, want ErrFenced(复占双活收口)", err)
		}
		if err := second.Fence(ctx, dbh); err != nil {
			t.Fatalf("successor Fence: %v, want nil", err)
		}
	})
	t.Run("expired takeover bumps from legacy zero", func(t *testing.T) {
		dbh := openLeaseDB(t)
		// insertLease 不写 epoch(DEFAULT 0)= 滚动窗存量行形态。
		insertLease(t, dbh, 7, "worker:dead", -time.Second)
		l, err := StartLease(context.Background(), dbh, leaseCfg(KindReaper, "r1", Explicit(7)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if l.Epoch() != 1 {
			t.Fatalf("Epoch() = %d, want 1(legacy 0 行接管起代)", l.Epoch())
		}
	})
	t.Run("auto claim bumps too", func(t *testing.T) {
		dbh := openLeaseDB(t)
		// auto 池取最小空闲号:0/1/2 垫 live,3 = 过期行 ⇒ 接管路径(非 INSERT)。
		insertLease(t, dbh, 0, "worker:w0", testTTL*20)
		insertLease(t, dbh, 1, "worker:w1", testTTL*20)
		insertLease(t, dbh, 2, "worker:w2", testTTL*20)
		insertLease(t, dbh, 3, "worker:dead", -time.Second)
		l, err := StartLease(context.Background(), dbh, leaseCfg(KindAPI, "a1", Auto()))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if l.Node() != 3 || l.Epoch() != 1 {
			t.Fatalf("(node,epoch) = (%d,%d), want (3,1)(auto 过期接管起代)", l.Node(), l.Epoch())
		}
	})
	t.Run("renewal preserves epoch", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(context.Background(), dbh, leaseCfg(KindWorker, "w1", Explicit(6)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		time.Sleep(testTTL * 2) // ≥2 个续期 tick
		if e, _ := leaseEpoch(t, dbh, 6); e != 1 {
			t.Fatalf("row epoch = %d after renewals, want 1(续期同持有同代)", e)
		}
		if l.Epoch() != 1 {
			t.Fatalf("Epoch() = %d after renewals, want 1", l.Epoch())
		}
	})
}

// TestLeaseFenceCrownedSemantics:等值加冕语义真源——夺写只因易主,不因
// 墙钟(谓词不含 expires_at):过期未易主 = 无人可撞,fence 放行;易主
// (holder 变或 epoch 变)= 拒;行消失(Release)= 拒。case 变体 holder
// = 异持有者(bin 判等),拒。
func TestLeaseFenceCrownedSemantics(t *testing.T) {
	ctx := context.Background()
	t.Run("crowned holder passes", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if err := l.Fence(ctx, dbh); err != nil {
			t.Fatalf("Fence: %v, want nil(行加冕本代)", err)
		}
	})
	t.Run("expired but uncrowned still passes", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		setLeaseExpiry(t, dbh, 4, -time.Second) // 过期未易主:懒回收下行未被接管
		if err := l.Fence(ctx, dbh); err != nil {
			t.Fatalf("Fence after expiry-without-takeover: %v, want nil(加冕语义:夺写只因易主)", err)
		}
	})
	t.Run("superseded epoch rejected", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		// 接管者形态:bump 代际(直改真源脚手架;holder 未变 = 纯 epoch 失配面)。
		if _, err := dbh.Exec(
			`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = 4`); err != nil {
			t.Fatalf("bump epoch: %v", err)
		}
		if err := l.Fence(ctx, dbh); !errors.Is(err, ErrFenced) {
			t.Fatalf("Fence after epoch bump: %v, want ErrFenced", err)
		}
	})
	t.Run("superseded holder rejected", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if _, err := dbh.Exec(
			`UPDATE snowflake_node_lease SET holder_id = 'worker:thief' WHERE node_id = 4`); err != nil {
			t.Fatalf("steal lease: %v", err)
		}
		if err := l.Fence(ctx, dbh); !errors.Is(err, ErrFenced) {
			t.Fatalf("Fence after theft: %v, want ErrFenced", err)
		}
	})
	t.Run("case-variant holder is a different crown", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if _, err := dbh.Exec(
			`UPDATE snowflake_node_lease SET holder_id = 'worker:W1' WHERE node_id = 4`); err != nil {
			t.Fatalf("case-variant steal: %v", err)
		}
		if err := l.Fence(ctx, dbh); !errors.Is(err, ErrFenced) {
			t.Fatalf("Fence after case-variant theft: %v, want ErrFenced(bin 判等)", err)
		}
	})
	t.Run("released row rejected", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		if err := l.Release(context.Background()); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if err := l.Fence(ctx, dbh); !errors.Is(err, ErrFenced) {
			t.Fatalf("Fence after Release: %v, want ErrFenced(行消失即一切 fence 拒)", err)
		}
	})
	t.Run("nil lease is legacy no-fence", func(t *testing.T) {
		var l *Lease
		if err := l.Fence(context.Background(), nil); err != nil {
			t.Fatalf("nil Lease Fence: %v, want nil(滚动窗 legacy 装配形态)", err)
		}
	})
}

// TestLeaseFenceLocksRowInTx:fence 锁定读在事务内持行锁——并发接管被
// 阻塞至事务提交(锁序契约:租约行恒为 fence 事务首锁,加冕校验与易主
// 在行锁上串行)。
func TestLeaseFenceLocksRowInTx(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()
	l, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	defer func() { _ = l.Release(context.Background()) }()

	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := l.Fence(ctx, tx); err != nil {
		t.Fatalf("Fence in tx: %v", err)
	}

	// 并发易主(另一连接 bump)必须被行锁挡住:短超时上下文内不可完成。
	bumpCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_, berr := dbh.ExecContext(bumpCtx,
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = 4`)
	if berr == nil {
		t.Fatalf("concurrent bump succeeded while fence tx holds row lock")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// 提交后 bump 可完成(锁释放);此后本代 fence 拒。
	if _, err := dbh.ExecContext(ctx,
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE node_id = 4`); err != nil {
		t.Fatalf("bump after commit: %v", err)
	}
	if err := l.Fence(ctx, dbh); !errors.Is(err, ErrFenced) {
		t.Fatalf("Fence after post-commit bump: %v, want ErrFenced", err)
	}
}

// TestLeaseAcquireBinaryCollation:acquire 显式 CAS 判等 utf8mb4_bin
// (D-t204 :778 交接缝收口)——case 变体在持行 = 异持有者,拒启(表缺省
// ai_ci 下旧口径互认复占,是复占双活的入口洞)。
func TestLeaseAcquireBinaryCollation(t *testing.T) {
	dbh := openLeaseDB(t)
	insertLease(t, dbh, 5, "api:H1", testTTL*20) // 大写变体在持
	if _, err := StartLease(context.Background(), dbh, leaseCfg(KindAPI, "h1", Explicit(5))); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld(case 变体 = 异持有者,bin 判等)", err)
	}
}

// TestLeaseRenewalBinaryCollationAndEpoch:续期 UPDATE 判等 bin + 携代际
// ——case 变体抢注者不能被旧持有者续期(ai_ci 旧口径会替 thief 续命);
// 同 holder 复占后的前身续期必须落空(代际失配 → lost,复占双活的
// 停摆收口)。
func TestLeaseRenewalBinaryCollationAndEpoch(t *testing.T) {
	t.Run("case-variant theft stops renewal", func(t *testing.T) {
		dbh := openLeaseDB(t)
		l, err := StartLease(context.Background(), dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("StartLease: %v", err)
		}
		defer func() { _ = l.Release(context.Background()) }()
		if _, err := dbh.Exec(
			`UPDATE snowflake_node_lease SET holder_id = 'worker:W1' WHERE node_id = 4`); err != nil {
			t.Fatalf("case-variant steal: %v", err)
		}
		select {
		case _, ok := <-l.Lost():
			if !ok {
				t.Fatalf("lost channel closed, want error signal")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("lost not signalled after case-variant theft(bin 判等下续期必须落空)")
		}
	})
	t.Run("predecessor renewal dies after reclaim", func(t *testing.T) {
		dbh := openLeaseDB(t)
		ctx := context.Background()
		first, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("first StartLease: %v", err)
		}
		defer func() { _ = first.Release(context.Background()) }()
		second, err := StartLease(ctx, dbh, leaseCfg(KindWorker, "w1", Explicit(4)))
		if err != nil {
			t.Fatalf("reclaim StartLease: %v", err)
		}
		defer func() { _ = second.Release(context.Background()) }()
		select {
		case _, ok := <-first.Lost():
			if !ok {
				t.Fatalf("lost channel closed, want error signal")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("predecessor lost not signalled after reclaim(复占后前身续期必须落空)")
		}
		// 后继者续期照常:到期持续前推。
		_, _, exp0, _, _ := leaseRow(t, dbh, 4)
		time.Sleep(testTTL * 2)
		_, _, exp1, _, _ := leaseRow(t, dbh, 4)
		if !exp1.After(exp0) {
			t.Fatalf("successor renewal stalled: %v -> %v", exp0, exp1)
		}
	})
}

// TestLeaseHolderIDAccessor:HolderID() 返回完整 holder_id(fence 语义的
// 随身身份,调用方对账面)。
func TestLeaseHolderIDAccessor(t *testing.T) {
	dbh := openLeaseDB(t)
	l, err := StartLease(context.Background(), dbh, leaseCfg(KindWorker, "w1", Auto()))
	if err != nil {
		t.Fatalf("StartLease: %v", err)
	}
	defer func() { _ = l.Release(context.Background()) }()
	if got := l.HolderID(); got != "worker:w1" {
		t.Fatalf("HolderID() = %q, want worker:w1(kind:identity 拼装单源)", got)
	}
}

// TestLeaseReleaseGuardsSuccessorRow:对抗审计 MEDIUM 锚——Release 的
// 代际限定 + bin 判等守卫:被复占废黜的前身(first,代 1)优雅退出时
// 不得误删后继者(second,代 2)的在活租约行(误删 = 后继下一 tick 续期
// 落空被误杀)。
func TestLeaseReleaseGuardsSuccessorRow(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()
	first, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
	if err != nil {
		t.Fatalf("first StartLease: %v", err)
	}
	second, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
	if err != nil {
		t.Fatalf("second StartLease: %v", err)
	}
	defer func() { _ = second.Release(context.Background()) }()
	// 废黜前身退出:DELETE 带 (holder bin 等值 + 代际) 谓词 ⇒ 0 行。
	if err := first.Release(ctx); err != nil {
		t.Fatalf("predecessor Release: %v", err)
	}
	e, ok := leaseEpoch(t, dbh, 5)
	if !ok || e != 2 {
		t.Fatalf("row after predecessor Release = (epoch %d, ok %v), want (2, true)(后继行存活)", e, ok)
	}
	if _, holder, _, _, _ := leaseRow(t, dbh, 5); holder != "api:h1" {
		t.Fatalf("holder = %q, want api:h1(后继持有不变)", holder)
	}
	// 后继照常加冕。
	if err := second.Fence(ctx, dbh); err != nil {
		t.Fatalf("successor Fence after predecessor Release: %v", err)
	}
}

// TestLeaseConcurrentSameHolderStartsDistinctEpochs:对抗审计 HIGH① 回归锚
// (claim→代际回读单事务化)。同 holder N 路并发 StartLease——CAS 恒匹配
// (同 holder 复占),每路各自 bump;回读被行锁锚定在本持 bump 上 ⇒ 任意
// 两路代际互异(行加冕唯一持有者,先启者续期落空 lost)。修复前回读是
// claim 临界区外的无锁独立语句,交错下两路可共持同代际(双活盲)。
func TestLeaseConcurrentSameHolderStartsDistinctEpochs(t *testing.T) {
	dbh := openLeaseDB(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	type res struct {
		epoch uint64
		err   error
		lease *Lease
	}
	out := make([]res, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := StartLease(ctx, dbh, leaseCfg(KindAPI, "h1", Explicit(5)))
			out[i] = res{err: err, lease: l}
			if err == nil {
				out[i].epoch = l.Epoch()
			}
		}(i)
	}
	wg.Wait()
	seen := map[uint64]bool{}
	for i, r := range out {
		if r.err != nil {
			t.Fatalf("concurrent start %d: %v(同 holder 复占必成功)", i, r.err)
		}
		defer func(l *Lease) { _ = l.Release(context.Background()) }(r.lease)
		if r.epoch == 0 || seen[r.epoch] {
			t.Fatalf("start %d epoch = %d, duplicate/zero(代际必须互异:行加冕唯一持有者)", i, r.epoch)
		}
		seen[r.epoch] = true
	}
}
