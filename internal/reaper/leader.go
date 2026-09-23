package reaper

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// leaderKey 是选主锁键(全局唯一,reaper 域)。
const leaderKey = "braid:reaper:leader"

// Locker 是 Redis 选主缝(go-redis 实现;测试 fake 或 nil=leaderless)。
// 语义 = fail-open 效率件:正确性由处置事务的锁后复核 + CAS 结构性保证,
// 双 leader 短窗只是重复扫描,收敛为「先者胜、后者复核 0 行空提交跳过」。
type Locker interface {
	// AcquireOrRenew 每 tick 开始调用:true = 本实例持锁(新取或续期成功),
	// false = 他人持锁,本 tick skip。
	AcquireOrRenew(ctx context.Context, token string, ttl time.Duration) (bool, error)
	// Release 正常停机路径释放锁(仅删自己的 token,compare-and-del)。
	Release(ctx context.Context, token string) error
}

// redisLocker 是 Locker 的 go-redis v9 实现。单段 Lua 保证「读-判-写」原子:
//	GET == nil(锁不存在)→ SET NX PX,返回持锁;
//	GET == 本实例 token   → PEXPIRE 续期(v1 修复:不续期的裸 NX 会自锁到 TTL 耗尽);
//	GET == 他人 token     → 0,本 tick skip。
var acquireOrRenewLua = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == false then
	if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
		return 1
	end
	return 0
end
if v == ARGV[1] then
	redis.call('PEXPIRE', KEYS[1], ARGV[2])
	return 1
end
return 0`)

var releaseLua = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0`)

// RedisLocker 构造选主锁。rdb 由调用方注入(cmd/reaper 复用队列地址;
// 测试指向 t13 Redis)。
func RedisLocker(rdb *redis.Client) Locker { return &redisLocker{rdb: rdb} }

type redisLocker struct{ rdb *redis.Client }

func (l *redisLocker) AcquireOrRenew(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	return acquireOrRenewLua.Run(ctx, l.rdb, []string{leaderKey},
		token, ttl.Milliseconds()).Bool()
}

func (l *redisLocker) Release(ctx context.Context, token string) error {
	return releaseLua.Run(ctx, l.rdb, []string{leaderKey}, token).Err()
}
