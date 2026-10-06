package ldap

// T20.5 时域残差垫平测试面(D-2026-10-06-t205-ldap-timing-pad):
// unknown_user(零命中,无用户 bind)与 wrong_password(多一次用户
// bind 往返 + 目录侧验证)的响应时域差 = 未认证探测者可聚合的账号
// 枚举 oracle(D-t176 边界⑤登记)。垫平机制 = 零命中分支纯客户端
// 等待 levelPad(启动期实测),零新增目录操作 ⇒ 服务账号自锁风险
// 结构性消亡——本文件的断言族各钉一头:
//
//   - 对齐断言(pairwise):注入对称 op 延迟下两臂中位时耗差 ≤ 容差
//   - 存在性锚(下限-only):删掉垫平即红(auth 包时序测试同纪律,
//     不设上限防 flake)
//   - 结构锚:记账证明零命中路径零用户 DN bind、恰一次服务 bind
//   - 判定保真:ctx 取消打断垫平仍返凭据族判定,不升 500 族
//   - clamp 接线锚(对抗审计 MEDIUM① 修):实测近零必须钳到下界,
//     裸中位变异 = 快目录上垫平静默失效
//   - rebind fail-loud 锚(对抗审计 MEDIUM② 修):目录拒 rebind =
//     构造响亮败,吞错降级变异即红
//
// 全部零栈零 docker(berfake_test.go 基建);BRAID_TEST_REDIS_DB
// 不涉(零 MySQL/Redis 面,判定先于 AnchorLoginUser 返回)。

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"braid/internal/auth"
)

const (
	// timingOpDelay 模拟单次 bind 的目录侧成本(往返 + 口令验证),
	// 服务 bind 与用户 bind 同值 = 「目录成本均匀」假设的 fake 形态
	// (levelPad 实测服务 bind,对齐用户 bind 依赖该假设,D- 条目
	// 声明)。60ms 量级:≫ 回环抖动(亚毫秒),≪ 5s 连接绝对界。
	timingOpDelay = 60 * time.Millisecond
	// timingSearchDelay 模拟 search 成本(两臂公共前缀,不影响差值)。
	timingSearchDelay = 10 * time.Millisecond
	// timingAlignTol 对齐容差 = 注入量一半。flake 论证:断言对象是
	// 两臂中位数的差,慢机/高载的公共模膨胀在差里相消;单臂尖刺
	// (GC/调度)由 5 采样中位数吸收;回环抖动 ≪ 1ms。全仓首个
	// pairwise 时域断言,容差取半注入量留足单边噪声余量。
	timingAlignTol = 30 * time.Millisecond
	// timingSamples 每臂采样数(中位数统计)。
	timingSamples = 5
)

// newTimingProvider 起一个 fake 目录并走真构造路径拿 provider——
// levelPad 是构造期实测产物,不能字面量拼(拼了就没有被测物)。
func newTimingProvider(t *testing.T, opt fakeDirOpt) (LDAPBindProvider, *fakeDirServer) {
	t.Helper()
	s := newFakeDirServer(t, opt)
	cfg := LDAPConfig{
		Addr: s.Addr(), BindDN: opt.svcDN, BindPassword: "svc-secret",
		BaseDN: "dc=example,dc=org", Filter: DefaultLDAPFilter,
	}
	p, err := NewLDAPBindProvider(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("startup probe against fake directory: %v", err)
	}
	return p, s
}

// mustCredsReason 断言错误是凭据族且 reason 精确——时序断言的前置
// 守卫:两臂若被判到同一错误分支,时耗自然同形,对齐断言即失义。
func mustCredsReason(t *testing.T, err error, want string) {
	t.Helper()
	var ce *auth.CredentialsError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want CredentialsError (%s)", err, want)
	}
	if ce.Reason != want {
		t.Fatalf("reason = %q, want %q", ce.Reason, want)
	}
}

// medianDuration 就地排序取中位。
func medianDuration(xs []time.Duration) time.Duration {
	slices.Sort(xs)
	return xs[len(xs)/2]
}

// TestAuthenticateTimingAlignment:核心对齐断言。fake 目录注入对称
// bind 成本(timingOpDelay),服务与用户 bind 同价 ⇒ levelPad 实测值
// ≈ 用户 bind 真实成本 ⇒ 两臂总时耗的期望差为 0,残差只剩测量噪声。
// 判据 = 中位差 ≤ timingAlignTol。删掉垫平实现即红:残差回到整整
// 一次 bind 注入量(60ms)> 容差。
func TestAuthenticateTimingAlignment(t *testing.T) {
	base := fakeDirOpt{
		svcDN:          "cn=svc,dc=example,dc=org",
		svcBindDelay:   timingOpDelay,
		searchDelay:    timingSearchDelay,
		userBindDelay:  timingOpDelay,
		userBindResult: 49, // wrong_password 臂:真 49 形状
	}
	armCOpt := base // 零命中 → unknown_user
	armDOpt := base // 命中 alice → bind 49
	armDOpt.entries = []string{"uid=alice,ou=people,dc=example,dc=org"}
	armC, _ := newTimingProvider(t, armCOpt)
	armD, _ := newTimingProvider(t, armDOpt)

	cTimes := make([]time.Duration, timingSamples)
	dTimes := make([]time.Duration, timingSamples)
	for i := range timingSamples {
		t0 := time.Now()
		_, err := armC.Authenticate(context.Background(), "ghost", "pw")
		cTimes[i] = time.Since(t0)
		mustCredsReason(t, err, auth.ReasonUnknownUser)

		t0 = time.Now()
		_, err = armD.Authenticate(context.Background(), "alice", "wrong-pw")
		dTimes[i] = time.Since(t0)
		mustCredsReason(t, err, auth.ReasonWrongPassword)
	}

	medC, medD := medianDuration(cTimes), medianDuration(dTimes)
	diff := medC - medD
	if diff < 0 {
		diff = -diff
	}
	t.Logf("unknown_user median=%v wrong_password median=%v diff=%v", medC, medD, diff)
	if diff > timingAlignTol {
		t.Fatalf("timing residual between unknown_user (%v) and wrong_password (%v) = %v, want ≤ %v — pad semantics broken OR asymmetric load spike on this run (medians of %d samples each; rerun once before diagnosing semantics, 对抗审计 LOW 修:负载假红与语义破坏同文案会误导诊断)", medC, medD, diff, timingAlignTol, timingSamples)
	}
}

// TestZeroHitLevelPadFloor:垫平存在性锚(下限-only,auth 包
// TestAuthenticateUnknownUserTimingFloor 同纪律)。fake 服务 bind 注入
// 80ms ⇒ levelPad 实测 ≈ 80ms ⇒ 零命中臂总时耗 ≈ 80(bind)+ 80(pad)
// = 160ms;断言 ≥ 120ms:删掉垫平(零命中即返)总时耗 ≈ 80ms 即红;
// 慢机只会更慢,不构成假红来源。白盒半边:构造期实测值落在
// [60ms, 3×bindDelay]——上界防的是「测量语义坏了」(测错对象/钳制
// bug)而非时序精度,3 倍单侧余量吸收重载 CI 的采样超调(3 采样中位
// 容忍 1 个尖刺;review LOW 修:2 倍是全文件唯一缺 flake 论证的负载
// 敏感断言,本仓 semaphore_bounds 负载 flake 前科)。
func TestZeroHitLevelPadFloor(t *testing.T) {
	const bindDelay = 80 * time.Millisecond
	p, _ := newTimingProvider(t, fakeDirOpt{
		svcDN:        "cn=svc,dc=example,dc=org",
		svcBindDelay: bindDelay,
	})

	if pad := p.levelPad; pad < 60*time.Millisecond || pad > 3*bindDelay {
		t.Fatalf("levelPad = %v, want within [%v, %v] (startup-measured from %v service binds)", pad, 60*time.Millisecond, 3*bindDelay, bindDelay)
	}

	t0 := time.Now()
	_, err := p.Authenticate(context.Background(), "ghost", "pw")
	elapsed := time.Since(t0)
	mustCredsReason(t, err, auth.ReasonUnknownUser)
	if elapsed < 120*time.Millisecond {
		t.Fatalf("zero-hit authenticate took %v, want ≥ 120ms (level pad absent → ~%v, enumeration oracle reopens)", elapsed, bindDelay)
	}
}

// TestZeroHitIssuesNoUserBind:自锁结构锚(T20.5「先解服务账号自锁
// 风险」的测试化)。fake 记账证明:①构造期探测的全部 bind 都落在
// 服务 DN 上(实测 rebind 亦然——成功 rebind 零锁定面),恰 1 次探测
// bind + 3 次实测 rebind;②零命中 Authenticate 全程恰一次服务 bind +
// 一次 search,零用户 DN bind、零第二次服务 bind——任何经额外 bind
// 的垫平形态(含合成 DN)都会在此红。对照臂:wrong_password 的用户
// bind 如常发生(真实目录交互只在真 DN 存在时进行)。
func TestZeroHitIssuesNoUserBind(t *testing.T) {
	const svcDN = "cn=svc,dc=example,dc=org"
	userDN := "uid=alice,ou=people,dc=example,dc=org"

	zeroHit, zeroHitSrv := newTimingProvider(t, fakeDirOpt{svcDN: svcDN})
	ops := zeroHitSrv.Ops()
	if len(ops) != 4 {
		t.Fatalf("startup probe ops = %d, want exactly 4 (1 probe bind + 3 measured rebinds): %+v", len(ops), ops)
	}
	for i, op := range ops {
		if op.kind != fakeOpBind || op.dn != svcDN {
			t.Fatalf("startup op[%d] = %+v, want service-DN bind (measured rebinds must never touch another DN)", i, op)
		}
	}

	_, err := zeroHit.Authenticate(context.Background(), "ghost", "pw")
	mustCredsReason(t, err, auth.ReasonUnknownUser)
	ops = zeroHitSrv.Ops()[4:] // 构造期 4 op 之后的新增交互
	if len(ops) != 2 || ops[0].kind != fakeOpBind || ops[0].dn != svcDN || ops[1].kind != fakeOpSearch {
		t.Fatalf("zero-hit authenticate ops = %+v, want exactly [bind(svc), search] — no user bind, no extra service bind (self-lock risk must stay dead)", ops)
	}

	wrongPw, wrongPwSrv := newTimingProvider(t, fakeDirOpt{
		svcDN:          svcDN,
		userBindResult: 49,
		entries:        []string{userDN},
	})
	_, err = wrongPw.Authenticate(context.Background(), "alice", "wrong-pw")
	mustCredsReason(t, err, auth.ReasonWrongPassword)
	ops = wrongPwSrv.Ops()[4:]
	if len(ops) != 3 || ops[0].kind != fakeOpBind || ops[0].dn != svcDN ||
		ops[1].kind != fakeOpSearch || ops[2].kind != fakeOpBind || ops[2].dn != userDN {
		t.Fatalf("wrong-password ops = %+v, want [bind(svc), search, bind(user)] for contrast arm", ops)
	}
}

// TestPadPreservesVerdictOnCanceledContext:垫平的 ctx 语义 = 可被
// 取消打断,但判定已由目录应答决定,打断后仍返凭据族——绝不把
// ctx.Canceled 原样上抛升 500 族(login.go default 分支会把它当身份源
// 故障:不计数不审计,判定语义漂移)。时耗上界(对抗审计 LOW 修)
// 钉死「打断」半边:删掉 ctx.Done 臂(变异)则垫平整段睡满,耗时翻
// 倍即红——只验判定不验时耗的话,该变异零噪声存活。
func TestPadPreservesVerdictOnCanceledContext(t *testing.T) {
	const bindDelay = 50 * time.Millisecond // ⇒ levelPad ≈ 50ms
	p, _ := newTimingProvider(t, fakeDirOpt{
		svcDN:        "cn=svc,dc=example,dc=org",
		svcBindDelay: bindDelay,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进入垫平前即取消:select 走 ctx.Done 臂,判定仍须返回

	t0 := time.Now()
	_, err := p.Authenticate(ctx, "ghost", "pw")
	elapsed := time.Since(t0)
	mustCredsReason(t, err, auth.ReasonUnknownUser)
	// 单侧余量论证(与 TestZeroHitLevelPadFloor 白盒上界同纪律):带
	// ctx.Done 臂的路径 = 拨号 + 服务 bind 50ms 睡眠 + search + 立即
	// 返回 ≈ 51ms;变异路径 ≈ 101ms。断言 < 80ms = 路径 + 30ms 余量,
	// 睡眠不欠睡,假红需要非睡眠开销 60% 膨胀。
	if elapsed >= 80*time.Millisecond {
		t.Fatalf("canceled-ctx authenticate took %v, want < 80ms (ctx.Done arm missing → pad sleeps in full → cancel-timing distinguishable)", elapsed)
	}
}

// TestClampLevelPad:实测值钳制边界(纯函数,零 fake)。下界防退化
// 测量(0/亚毫秒),上界防病态目录把每次零命中登录拖满秒级。
func TestClampLevelPad(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero floors to min", 0, levelPadMin},
		{"sub-millisecond floors to min", 500 * time.Microsecond, levelPadMin},
		{"boundary min passes through", levelPadMin, levelPadMin},
		{"in-range passes through", 5 * time.Millisecond, 5 * time.Millisecond},
		{"boundary max passes through", levelPadMax, levelPadMax},
		{"pathological directory ceilings", 2 * time.Second, levelPadMax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampLevelPad(tt.in); got != tt.want {
				t.Fatalf("clampLevelPad(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestLevelPadClampWiring:clamp 接线锚(对抗审计 MEDIUM① 修)——
// TestClampLevelPad 只钉纯函数,不钉 measureLevelPad 确实调用了它;
// 裸中位变异(ldap.go 去掉 clampLevelPad 调用)曾令四族断言全绿存活,
// 而 min 钳恰是快目录(实测 < 1ms、真实用户 bind 成本不近零)上唯一
// 保证非零垫平的要素。本测试用零延迟 fake(回环 RTT 数十 µs,≪ 1ms
// 下界 20 倍余量)断言构造期实测值恰等于 levelPadMin;裸中位 ≈ 0 即
// 红。max 侧接线经纯函数表驱动传递覆盖(不做 1s+ 慢测试)。
func TestLevelPadClampWiring(t *testing.T) {
	p, _ := newTimingProvider(t, fakeDirOpt{svcDN: "cn=svc,dc=example,dc=org"}) // 全零延迟
	if p.levelPad != levelPadMin {
		t.Fatalf("levelPad = %v, want exactly %v (sub-millisecond loopback measurement must clamp to the floor — bare-median mutation silently disables the pad on fast directories)", p.levelPad, levelPadMin)
	}
}

// TestNewLDAPBindProviderRebindFailLoud:rebind 失败 fail-loud 路径锚
// (对抗审计 MEDIUM② 修;D-t205 决策 2 封套②「个别实现/策略拒
// rebind = 启动失败即回退路径,响亮不静默」)。fake 自第 2 次服务
// bind 起回 49(探测 bind 过、首次 rebind 拒)⇒ 构造必须报错且错误
// 链两层可见(startup probe 外层 wrap + measure rebind 因由);吞错
// 降级变异(rebind 失败则 continue、pad 兜底 0)= 垫平静默消失即红。
func TestNewLDAPBindProviderRebindFailLoud(t *testing.T) {
	s := newFakeDirServer(t, fakeDirOpt{
		svcDN:           "cn=svc,dc=example,dc=org",
		svcBindFailFrom: 2,
	})
	cfg := LDAPConfig{
		Addr: s.Addr(), BindDN: "cn=svc,dc=example,dc=org", BindPassword: "svc-secret",
		BaseDN: "dc=example,dc=org", Filter: DefaultLDAPFilter,
	}
	_, err := NewLDAPBindProvider(context.Background(), cfg, nil)
	if err == nil {
		t.Fatal("rebind-rejecting directory must fail the constructor loudly (silent degradation = timing pad off, enumeration oracle reopens)")
	}
	if !strings.Contains(err.Error(), "startup probe") || !strings.Contains(err.Error(), "measure rebind") {
		t.Fatalf("err = %v, want startup-probe wrap carrying a measure-rebind cause", err)
	}
	if ops := s.Ops(); len(ops) != 2 {
		t.Fatalf("fake ops = %+v, want exactly 2 service binds (probe ok + first rebind rejected)", ops)
	}
}
