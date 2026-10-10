// Package snowflake 提供 64 位雪花 ID 生成器:41bit 毫秒时间戳 + 10bit 节点号 +
// 12bit 序列。epoch 取 2026-01-01T00:00:00Z(41bit ≈ 69.7 年)。
//
// outbox.id / audit_log.id / 各业务主键均应用侧生成(MySQL 分区表唯一键必须含
// 分区列的硬约束),「Snowflake 近似时序」被 outbox relay 的 FIFO 扫描(ORDER BY id)
// 与审计链行序白拿。但 id 序只是公平调度序,不是因果序:跨进程仅毫秒粒度近似,
// 时钟偏差可致先提交者 id 更大。因果正确性完全由同事务状态迁移承载;未来任何
// 「依赖 id 序的正确性」的需求(P1 哈希链行序)必须重新论证。
//
// 时钟回拨(见 D-2026-09-21-snowflake-rollback-bounded,对 0001_init.up.sql 头注释
// 「阻塞等待」的修订):回拨 ≤ maxBackwardWait(默认 2s,覆盖 NTP 微调的常见形态)
// 时阻塞等待时钟追平;超过则快速失败返回 ErrClockBackward。理由:outbox.Append
// 发生在状态迁移事务内,无限阻塞 = 该进程全部引擎写入停摆;大幅回拨属 NTP 偏移
// 监控的受监控条件,不应由写入路径无限吸收。
package snowflake

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// epochMs 是自定义纪元(2026-01-01T00:00:00Z)的 Unix 毫秒数。
	epochMs int64 = 1767225600000

	nodeBits = 10
	seqBits  = 12

	// MaxNodeID 是节点号上界(10bit,也是租约池容量)。解析三层(D-t151
	// 决策 2):显式 env 数字 > 内建默认(api=1、worker=2、reaper=3,视同
	// 显式 claim;reaper 缺省由 auto 改 3 = T20.9/D-2026-10-10-t209——PID
	// 派生已随 T15.2 退役,容器 PID 恒 1 撞 api 默认 1)> 字面 "auto" 哨兵
	// (池分配,多副本用)。节点号仲裁 = MySQL 租约表(migration 0012),
	// relay 无生成器不参与。跨进程重号的唯一症状是 outbox/业务表 PK 1062
	// (写入事务中止 + 告警)。
	MaxNodeID = 1<<nodeBits - 1 // 1023

	maxSeq         = 1<<seqBits - 1 // 序列上界;同 ms 可产 4096 个(seq 0..4095)
	maxBackwardDef = 2 * time.Second

	// waitTick 是回拨/序列耗尽等待的重探间隔;纳秒级临界区外唯一的睡眠粒度。
	waitTick = 200 * time.Microsecond
)

// ErrClockBackward:时钟回拨超过 maxBackwardWait,或时钟早于纪元。
// 调用方不应重试(重试只会再次失败),应触发 NTP 偏移告警。
var ErrClockBackward = errors.New("snowflake: clock moved backward beyond tolerance")

// ErrLeaseLost:节点租约丢失后的停发闸(D-t151 决策 2:续期失败 = 停止
// 签发)。Block 置位后 Next 恒返回本错误——租约已失,继续签发 = 与接管
// 者撞号(PK 1062 面);复航 = 进程退出后重新 acquire,无解除面。
var ErrLeaseLost = errors.New("snowflake: node lease lost; issuance blocked")

// Generator 是单进程雪花 ID 生成器。Next 可多 goroutine 并发调用(内部互斥,
// 临界区仅本地算术,无 IO)。
type Generator struct {
	mu       sync.Mutex
	node     uint16
	lastMS   int64 // 距纪元的毫秒数;0 = 尚未生成(纪元首毫秒合法,用 seq 区分)
	seq      uint16
	now      func() time.Time
	maxBackw time.Duration
	blocked  atomic.Bool // 停发闸(单向;Block 置位,Next 首查)
}

// New 按节点号构造生成器(nodeID > MaxNodeID 报错)。同一部署内每个进程一个
// 生成器,节点号由部署层静态分配且不得重号。
func New(nodeID uint16) (*Generator, error) {
	return newGenerator(nodeID, time.Now, maxBackwardDef)
}

func newGenerator(nodeID uint16, now func() time.Time, maxBackward time.Duration) (*Generator, error) {
	if nodeID > MaxNodeID {
		return nil, fmt.Errorf("snowflake: node id %d > %d", nodeID, MaxNodeID)
	}
	if now == nil {
		return nil, errors.New("snowflake: nil clock")
	}
	return &Generator{node: nodeID, now: now, maxBackw: maxBackward}, nil
}

// Block 置停发闸(单向,幂等):租约丢失后由租约守护方调用。此后 Next
// 恒返回 ErrLeaseLost——见 ErrLeaseLost 注释。
func (g *Generator) Block() { g.blocked.Store(true) }

// Next 生成下一个 ID。同毫秒内序列严格递增,跨毫秒时间戳递增 ⇒ 单调。
// 时钟回拨在容忍界内阻塞等待追平;超界返回 ErrClockBackward;停发闸
// 置位后返回 ErrLeaseLost(闸优先)。
func (g *Generator) Next() (uint64, error) {
	if g.blocked.Load() {
		return 0, ErrLeaseLost
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		ms := g.now().UnixMilli() - epochMs
		switch {
		case ms < 0:
			return 0, fmt.Errorf("%w: clock before epoch (%dms)", ErrClockBackward, ms)
		case ms < g.lastMS:
			// 回拨:有界等待追平;等待期间继续恶化(差值超界)同样快速失败。
			if time.Duration(g.lastMS-ms)*time.Millisecond > g.maxBackw {
				return 0, fmt.Errorf("%w: %dms behind last, tolerance %s",
					ErrClockBackward, g.lastMS-ms, g.maxBackw)
			}
			g.sleep(waitTick)
		case ms == g.lastMS:
			if g.seq >= maxSeq {
				// 本毫秒序列耗尽:等下一毫秒(标准雪花行为,不借 neighboring ms)。
				g.sleep(waitTick)
				continue
			}
			g.seq++
			return g.compose(ms), nil
		default:
			g.lastMS = ms
			g.seq = 0
			return g.compose(ms), nil
		}
	}
}

func (g *Generator) compose(ms int64) uint64 {
	return uint64(ms)<<(nodeBits+seqBits) | uint64(g.node)<<seqBits | uint64(g.seq)
}

// sleep 在持锁状态下小睡后重试(now() 每轮循环重读,可注入假时钟推进)。
func (g *Generator) sleep(d time.Duration) {
	g.mu.Unlock()
	time.Sleep(d)
	g.mu.Lock()
}
