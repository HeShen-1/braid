package snowflake

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock:可编程时钟。advance 供测试推进(模拟跨毫秒/回拨),读侧即 now()。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestLayoutInvariants(t *testing.T) {
	g, err := New(7)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seen := make(map[uint64]struct{}, 100)
	for range 100 {
		id, err := g.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got := id >> (nodeBits + seqBits); got == 0 {
			t.Fatalf("id %d: timestamp bits empty", id)
		}
		if got := (id >> seqBits) & MaxNodeID; got != 7 {
			t.Fatalf("id %d: node bits = %d, want 7", id, got)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d within generator", id)
		}
		seen[id] = struct{}{}
	}
}

func TestMonotonicSameGenerator(t *testing.T) {
	g, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ids := make([]uint64, 10000)
	for i := range ids {
		id, err := g.Next()
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		ids[i] = id
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		t.Fatalf("ids not strictly increasing")
	}
}

func TestNewRejectsBadNode(t *testing.T) {
	if _, err := New(MaxNodeID + 1); err == nil {
		t.Fatalf("node %d accepted, want reject", MaxNodeID+1)
	}
	if _, err := New(MaxNodeID); err != nil {
		t.Fatalf("node %d rejected: %v", MaxNodeID, err)
	}
}

// TestClockBackwardBoundedWait:回拨在容忍界内 → 阻塞等待追平(期间不返回、
// 不产出回退 ID),追平后继续单调。咬住变异:删阻塞等待(直接用回拨时刻生成
// ⇒ id 回退/重复毫秒)。
func TestClockBackwardBoundedWait(t *testing.T) {
	clk := &fakeClock{now: time.UnixMilli(epochMs + 10000)}
	g, err := newGenerator(1, clk.Now, 2*time.Second)
	if err != nil {
		t.Fatalf("newGenerator: %v", err)
	}
	first, err := g.Next()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	clk.advance(-500 * time.Millisecond) // 容忍界内的回拨

	done := make(chan uint64, 1)
	go func() {
		id, err := g.Next()
		if err != nil {
			t.Errorf("Next after rollback: %v", err)
			done <- 0
			return
		}
		done <- id
	}()

	// 阻塞证明:时钟未追平前 50ms 内不得返回(不是概率等待,是确定性不返回)。
	select {
	case id := <-done:
		t.Fatalf("Next returned %d while clock still behind", id)
	case <-time.After(50 * time.Millisecond):
	}
	clk.advance(600 * time.Millisecond) // 追平并超过原 lastMS
	var second uint64
	select {
	case second = <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Next did not return after clock caught up")
	}
	if second <= first {
		t.Fatalf("id regressed after bounded wait: %d then %d", first, second)
	}
}

// TestClockBackwardBeyondTolerance:回拨超过容忍界 → 快速失败 ErrClockBackward。
// 咬住变异:丢掉上界检查(无限阻塞 ⇒ 测试挂死而非快速失败)。
func TestClockBackwardBeyondTolerance(t *testing.T) {
	clk := &fakeClock{now: time.UnixMilli(epochMs + 10000)}
	g, err := newGenerator(1, clk.Now, 2*time.Second)
	if err != nil {
		t.Fatalf("newGenerator: %v", err)
	}
	if _, err := g.Next(); err != nil {
		t.Fatalf("first: %v", err)
	}
	clk.advance(-5 * time.Second) // 超界
	if _, err := g.Next(); !errors.Is(err, ErrClockBackward) {
		t.Fatalf("want ErrClockBackward, got %v", err)
	}
}

// TestSeqExhaustionWaitsNextMs:同毫秒序列耗尽(4096 个)→ 等待下一毫秒,
// 不借用相邻毫秒、不回绕。咬住变异:丢序列上界(掩码回绕 ⇒ 同 ms 内 id 重复)。
func TestSeqExhaustionWaitsNextMs(t *testing.T) {
	clk := &fakeClock{now: time.UnixMilli(epochMs + 20000)}
	g, err := newGenerator(1, clk.Now, 2*time.Second)
	if err != nil {
		t.Fatalf("newGenerator: %v", err)
	}
	seen := make(map[uint64]struct{}, maxSeq+2)
	for range maxSeq + 1 { // 本 ms 的 4096 个
		id, err := g.Next()
		if err != nil {
			t.Fatalf("within-ms Next: %v", err)
		}
		seen[id] = struct{}{}
	}
	done := make(chan uint64, 1)
	go func() {
		id, err := g.Next() // 第 4097 个:必须等下一 ms
		if err != nil {
			t.Errorf("exhausted Next: %v", err)
			done <- 0
			return
		}
		done <- id
	}()
	select {
	case id := <-done:
		t.Fatalf("Next returned %d within exhausted millisecond", id)
	case <-time.After(50 * time.Millisecond):
	}
	clk.advance(time.Millisecond)
	next := <-done
	if _, dup := seen[next]; dup {
		t.Fatalf("id %d duplicates an earlier id", next)
	}
	if uint64(next>>(nodeBits+seqBits)) <= uint64(clk.Now().UnixMilli()-epochMs-1) {
		t.Fatalf("id %d did not advance to next millisecond", next)
	}
}

// TestConcurrentUniqueness:-race 下多 goroutine 齐发,全部 ID 唯一。
func TestConcurrentUniqueness(t *testing.T) {
	g, err := New(3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const workers, per = 8, 5000
	var wg sync.WaitGroup
	start := make(chan struct{})
	var total atomic.Int64
	ids := make(chan uint64, workers*per)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range per {
				id, err := g.Next()
				if err != nil {
					t.Errorf("Next: %v", err)
					return
				}
				ids <- id
			}
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	uniq := make(map[uint64]struct{}, workers*per)
	for id := range ids {
		if _, dup := uniq[id]; dup {
			t.Fatalf("duplicate id %d", id)
		}
		uniq[id] = struct{}{}
		total.Add(1)
	}
	if int(total.Load()) != workers*per {
		t.Fatalf("collected %d ids, want %d", total.Load(), workers*per)
	}
}
