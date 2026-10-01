package tools

// 资源超限 e2e(T10.1 T11,真 docker):GATE 1 切片 = 「2×alloc→kill 失败
// 关闭 + Inspect 硬化落盘断言」,行为面覆盖基线 §2 六行——
//   - #4 内存(零 swap):alloc 2×256m 探针 → 容器被杀,失败关闭(非成功、
//     非截断成功),kill 后载体正常服务(宿主存活 + 后续调用正常的 e2e 代理);
//   - #5 CPU:spin 载荷 + cgroup cpu.stat 双采样计量(内核记账、窗外观察,
//     零时序敏感断言:速率窗 [0.15, 0.75] 分离配额执法 0.5 与漂移形态 ~1.0,
//     nr_throttled 增长为 CFS 节流的二进制佐证;v2 主路径 + v1 回退,两代
//     布局都不可读 = 环境门控 skip);
//   - #6 pids:forkchain 自再执行链,全部世代同时在场 → cgroup 预算内收口
//     (stop=budget;EAGAIN 真触顶兜底),断言 2 ≤ depth ≤ 64(pids-limit
//     结构上界;「宿主线程数无漂移」以链自终止 + 零残留差分覆盖——宿主侧
//     进程计数在并行环境下噪声主导,是假红面不是证明面);
//   - #7 FD:fds 循环 open 至 EMFILE(软失败正常退出),断言 512 ≤ n ≤
//     1024(下限分离「ulimit 漂移缺失 + daemon 缺省同值」的贴界形态;
//     旗标在场性由 inspect 矩阵独立钉死);
//   - #14 daemon 日志封顶:52MiB(非 5m 整倍数)stderr 载荷,docker logs
//     双流合并(容器 stderr 走 CLI stderr,单收 stdout 是假绿)截留
//     ≤ 5m+slack + 尾部哨兵行在场(轮转删头留尾,哨兵必驻留 = 通道承载
//     证明;宿主日志文件路径需 root,非测试可达面);
//   - #15 tini:zombies 探针双 fork 孤儿 → 沉降扫描 Z=0;负向对照(剥
//     --init,PID 1 = helper 自身不收割)断言僵尸滞留 >0——漂移即红的
//     红路径 + 探针检测力自证(zombies>0 可达,探针致盲变异在此即红),
//     并兑现威胁列的复合形态半边(孤儿僵尸 × PID 1 不收割);
//   - §6 OOM 归因:keep 容器(post-mortem 未删)经 docker inspect 单字段
//     证 OOMKilled=true + exit 137——正是载体 inspectOOMKilled 消费的机器
//     可读面(生产路径 --rm 下 daemon 先删容器,归因降级 unknown、exit 码
//     仍是主信号,该半边由 carrier 单测锚)。
//
// 镜像走 sandboxEnsureImage(prod 形态;探针族不消费 e2e CA 注入镜像)。
// argv 手术沿 T10 先例:builder 全硬化旗标原样,尾参(builder 尾部契约
// [entrypoint,image,tool] 的 tool 位)换 selftest 探针;变形——keep(剥
// --rm:容器退出后留驻,供 inspect 归因/矩阵断言与 docker logs 计量;子
// 用例尾即 rm)、detach(加 -d:spin 运行窗内采样)、剥 --init(负向对照)。
// carrier 永不发 selftest argv(构造器单测钉死),探针载荷经 argv 词表
// 到达与 dump-env/serve 同一论证面(基线 §3:无新能力面)。
//
// 零残留差分按 ancestor=<槽位镜像 tag> 圈定(label 无 lane 维度是全局面,
// 并行 lane 互踩是 T9 已登记的假红形态;镜像 tag 槽位隔离是既有公约,
// 差分借同一键收窄到本 lane 容器——本文件全部容器(手术与载体)都跑
// 本槽位 tag)。

import (
	"bytes"
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
)

// TestSandboxE2EResourceLimits 真 docker 资源超限 e2e(子用例共享镜像与
// 基线档位配置)。
func TestSandboxE2EResourceLimits(t *testing.T) {
	// 基线档位与断言界共源(builder argv、cfg、断言三处消费同值)。
	const (
		t11PidsTier    = 64
		t11NoFileLimit = 1024
		t11FDSFloor    = 512
		t11CPURateMin  = 0.15
		t11CPURateMax  = 0.75
	)
	sandboxDockerGate(t)
	slot := sandboxSlotNumber(t)
	tag := sandboxTestImageTag()
	sandboxEnsureImage(t, tag)
	t.Cleanup(func() {
		_ = exec.Command(sandboxDockerBin, "rmi", tag).Run()
	})

	// 零残留基线快照,按 ancestor=<本槽位 tag> 圈定(文件头注记:label
	// 是全局面,tag 是 lane 键)。keep 容器在各子用例尾已 rm;测试进程
	// 被 SIGKILL 泄漏的固定名 stopped 容器落入 before 集被差分扣除,但
	// 同名 docker run 撞名响亮红需手工 rm——「自愈」只覆盖差分半边。
	t11LabeledIDs := func(t *testing.T) map[string]bool {
		t.Helper()
		out, err := exec.Command(sandboxDockerBin, "ps", "-aq",
			"--filter", "label=braid.sandbox=1", "--filter", "ancestor="+tag).Output()
		if err != nil {
			t.Fatalf("docker ps (label + ancestor=%s): %v", tag, err)
		}
		ids := make(map[string]bool)
		for _, id := range strings.Fields(string(out)) {
			ids[id] = true
		}
		return ids
	}
	before := t11LabeledIDs(t)

	cfg := sandboxConfig{
		Docker:    true,
		Image:     tag,
		Memory:    "256m",
		CPUs:      "0.5",
		PidsLimit: t11PidsTier, MaxConcurrency: 8,
		Owner: sandboxTestOwner(),
	}

	// probeArgv argv 手术:builder 全硬化 argv(file_read 形态,零挂载)
	// + 尾参换探针 argv;keep 剥 --rm,detach 加 -d。尾参契约防漂移与
	// T10 同款:尾部不是 tool 位时先行响亮红(替换错位会让探针载荷静默
	// 变成工具会话,弱断言假绿)。
	probeArgv := func(t *testing.T, name string, detach, keep bool, probe ...string) []string {
		t.Helper()
		argv, err := buildSandboxRunArgv(cfg, name, "file_read", nil)
		if err != nil {
			t.Fatalf("build argv: %v", err)
		}
		if argv[len(argv)-1] != "file_read" {
			t.Fatalf("builder tail contract drifted: argv tail = %q, want %q (surgery target moved)", argv[len(argv)-1], "file_read")
		}
		argv = append(argv[:len(argv)-1], probe...)
		if keep {
			filtered := make([]string, 0, len(argv))
			for _, a := range argv {
				if a != "--rm" {
					filtered = append(filtered, a)
				}
			}
			argv = filtered
		}
		if detach {
			argv = append([]string{argv[0], "-d"}, argv[1:]...)
		}
		return argv
	}

	// dockerRun 附着执行(stdout/stderr 全捕获;stderr 随错误呈现,与
	// T10 e2e 同款诊断面)。90s 预算封顶:裸 docker 面无 ctx 兜底时
	// daemon 半死会挂到 go test 全局超时一锅端(各载荷秒级,90s 只拦
	// 悬挂)。返回 stdout 与执行错误(ExitError 由调用方按探针预期终态
	// 判定)。
	dockerRun := func(t *testing.T, argv []string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, sandboxDockerBin, argv...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			err = fmt.Errorf("%w\n%s", err, stderr.String())
		}
		return stdout.Bytes(), err
	}

	// rmKeep keep 容器收尾 rm -f(子用例尾即调,不等 t.Cleanup——零残留
	// 差分在后续子用例执行,stopped keep 容器必须先行退场)。失败只记
	// 日志:容器若已退场(--rm 语义边界)rm 非错误。
	rmKeep := func(t *testing.T, name string) {
		t.Helper()
		if out, err := exec.Command(sandboxDockerBin, "rm", "-f", name).CombinedOutput(); err != nil {
			t.Logf("cleanup: rm -f %s: %v\n%s", name, err, out)
		}
	}

	// inspectFormat docker inspect --format 单字段读取(载体 inspectOOMKilled
	// 同款通道)。
	inspectFormat := func(t *testing.T, name, goTemplate string) string {
		t.Helper()
		out, err := exec.Command(sandboxDockerBin, "inspect", "--format", goTemplate, name).Output()
		if err != nil {
			t.Fatalf("docker inspect --format %q %s: %v", goTemplate, name, err)
		}
		return strings.TrimSpace(string(out))
	}

	t.Run("inspect pins the hardening matrix on the daemon", func(t *testing.T) {
		// GATE 1「Inspect 硬化落盘断言」:builder 旗标全集在 daemon 侧的
		// 落盘形态逐项钉死(基线 §1-b:配置漂移被测试击穿,不默默生效)。
		// 任何旗标从 argv 消失 → 对应 HostConfig/Config 字段红。
		name := "t11-inspect-t" + strconv.Itoa(slot)
		argv := probeArgv(t, name, false, true, "selftest", "dump-env")
		defer rmKeep(t, name)
		out, err := dockerRun(t, argv)
		if err != nil {
			t.Fatalf("docker run (dump-env keep): %v", err)
		}
		var env map[string]any
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("dump-env not a JSON object: %v (%s)", err, out)
		}
		raw, err := exec.Command(sandboxDockerBin, "inspect", name).Output()
		if err != nil {
			t.Fatalf("docker inspect %s: %v", name, err)
		}
		var rows []struct {
			Config struct {
				User   string            `json:"User"`
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
			HostConfig struct {
				ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
				Memory         int64    `json:"Memory"`
				MemorySwap     int64    `json:"MemorySwap"`
				NanoCpus       int64    `json:"NanoCpus"`
				PidsLimit      int64    `json:"PidsLimit"`
				CapDrop        []string `json:"CapDrop"`
				SecurityOpt    []string `json:"SecurityOpt"`
				Init           bool     `json:"Init"`
				Ulimits        []struct {
					Name string `json:"Name"`
					Soft int64  `json:"Soft"`
					Hard int64  `json:"Hard"`
				} `json:"Ulimits"`
				LogConfig struct {
					Type   string            `json:"Type"`
					Config map[string]string `json:"Config"`
				} `json:"LogConfig"`
				TmpFs       map[string]string `json:"TmpFs"`
				NetworkMode string            `json:"NetworkMode"`
			} `json:"HostConfig"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 {
			t.Fatalf("docker inspect output not a single container record: %v (%s)", err, raw)
		}
		h := rows[0].HostConfig
		type check struct {
			what string
			bad  bool
		}
		checks := []check{
			{"ReadonlyRootfs", !h.ReadonlyRootfs},
			{"Memory=256m", h.Memory != 256<<20},
			{"MemorySwap=256m (zero swap headroom)", h.MemorySwap != 256<<20},
			{"NanoCpus=0.5", h.NanoCpus != 500000000},
			{"PidsLimit=64", h.PidsLimit != t11PidsTier},
			{"CapDrop=[ALL]", len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL"},
			{"SecurityOpt=[no-new-privileges]", len(h.SecurityOpt) != 1 || h.SecurityOpt[0] != "no-new-privileges"},
			{"Init (tini as PID 1)", !h.Init},
			{"Ulimits=[nofile 1024:1024]", len(h.Ulimits) != 1 || h.Ulimits[0].Name != "nofile" || h.Ulimits[0].Soft != t11NoFileLimit || h.Ulimits[0].Hard != t11NoFileLimit},
			{"LogConfig=json-file max-size=5m max-file=1", h.LogConfig.Type != "json-file" || h.LogConfig.Config["max-size"] != "5m" || h.LogConfig.Config["max-file"] != "1"},
			// daemon 对 TmpFs 选项串原样存储(不重排不改写),整串等值
			// 比对——开尾前缀匹配会放行尾部追加选项(exec/size 上调)的
			// 漂移形态。
			{"TmpFs=/tmp rw,noexec,nosuid,size=16m", h.TmpFs["/tmp"] != "rw,noexec,nosuid,size=16m"},
			{"NetworkMode=none (file_read posture)", h.NetworkMode != "none"},
			{"User=65532:65532", rows[0].Config.User != "65532:65532"},
			{"label braid.sandbox=1", rows[0].Config.Labels["braid.sandbox"] != "1"},
			{"label braid.tool=file_read", rows[0].Config.Labels["braid.tool"] != "file_read"},
			// T15.3:owner label 在真 daemon 落盘——清扫过滤键与 e2e 残留
			// 差分的枚举键本身被锚(argv golden 之外的第二层)。
			{"label braid.sandbox.owner=" + cfg.Owner, rows[0].Config.Labels["braid.sandbox.owner"] != cfg.Owner},
		}
		for _, c := range checks {
			if c.bad {
				t.Errorf("hardening drifted: want %s (HostConfig=%+v Config.User=%q Labels=%v)", c.what, h, rows[0].Config.User, rows[0].Config.Labels)
			}
		}
	})

	t.Run("memory overlimit: 2x alloc is killed and fails closed", func(t *testing.T) {
		// 基线 #4:2×memory 档位(512 MiB vs 256m)按 8MiB 步长写满 →
		// OOM 杀。失败关闭 = 非成功、非截断成功:退出码 137 + stdout 零
		// 字节(成功报告行不存在)。已知宿主形态登记:vm.overcommit_memory=2
		// 下 Go mmap 直接 ENOMEM → "runtime: out of memory" exit 2,本断言
		// 在此类宿主错红——本机/CI 均缺省启发式 overcommit,接受。
		argv := probeArgv(t, "t11-oom-t"+strconv.Itoa(slot), false, false, "selftest", "alloc", "512")
		out, err := dockerRun(t, argv)
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("err = %v, want exec.ExitError (container kill relays non-zero exit)", err)
		}
		if ee.ExitCode() != 137 {
			t.Fatalf("exit code = %d, want 137 (OOM kill)", ee.ExitCode())
		}
		if len(bytes.TrimSpace(out)) != 0 {
			t.Fatalf("stdout = %q, want empty (fail closed: no success or truncated-success report)", out)
		}

		// §6 OOM 归因的 daemon 侧事实:keep 容器查 OOMKilled 单字段
		// (载体 inspectOOMKilled 消费的同一机器可读面;生产 --rm 路径
		// daemon 先删容器 → unknown,故归因半边在此用 keep 形态证)。
		name := "t11-oomkeep-t" + strconv.Itoa(slot)
		argvKeep := probeArgv(t, name, false, true, "selftest", "alloc", "512")
		defer rmKeep(t, name)
		if _, err := dockerRun(t, argvKeep); err == nil {
			t.Fatal("second 2x alloc unexpectedly succeeded (memory limit not enforced?)")
		}
		if got := inspectFormat(t, name, "{{.State.OOMKilled}}"); got != "true" {
			t.Fatalf("State.OOMKilled = %q, want true", got)
		}
		if got := inspectFormat(t, name, "{{.State.ExitCode}}"); got != "137" {
			t.Fatalf("State.ExitCode = %q, want 137", got)
		}
	})

	t.Run("cpu limit throttles spin to the 0.5 quota", func(t *testing.T) {
		// 基线 #5:detach 跑 spin 载荷,窗外(docker exec)双采样容器自身
		// cgroup 的 cpu 记账(内核记账 = daemon 执法的同一计数器)。
		// 速率 = Δusage_usec/Δwall ∈ [0.15, 0.75]:0.5 配额执法落在窗内,
		// 旗标漂移(无配额)单线程自旋 ~1.0 即红;下限防「探针没跑/计量
		// 读零」假绿。nr_throttled 增长 = CFS 配额耗尽节流的二进制佐证。
		// 载荷 20s(非 8s):三次 docker RPC(Running 轮询 + 双采样 exec)
		// 在 daemon 高载下可达秒级,寿命预算放宽容错,断言窗保持 2s 不变
		// (零时序敏感)。记账读取 v2 主路径(cpu.stat 单文件)+ v1 回退
		// (cpuacct.usage ns + cpu/cpu.stat);两代都不可读 = 环境门控
		// skip(记账面缺失不是执法失败,伪装红会误导)——但仅在容器确认
		// Running 时才准 skip,容器早退仍响亮红。
		name := "t11-spin-t" + strconv.Itoa(slot)
		argv := probeArgv(t, name, true, false, "selftest", "spin", "20")
		defer rmKeep(t, name)
		if _, err := dockerRun(t, argv); err != nil {
			t.Fatalf("docker run -d (spin): %v", err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for inspectFormat(t, name, "{{.State.Running}}") != "true" {
			if time.Now().After(deadline) {
				t.Fatalf("spin container never reached Running state (state=%s)", inspectFormat(t, name, "{{.State.Status}}"))
			}
			time.Sleep(100 * time.Millisecond)
		}
		execCat := func(path string) (string, error) {
			out, ex := exec.Command(sandboxDockerBin, "exec", name, "cat", path).Output()
			if ex != nil {
				return "", ex
			}
			return string(out), nil
		}
		parseKV := func(raw string) map[string]int64 {
			fields := map[string]int64{}
			for _, line := range strings.Split(raw, "\n") {
				kv := strings.Fields(line)
				if len(kv) != 2 {
					continue
				}
				if v, perr := strconv.ParseInt(kv[1], 10, 64); perr == nil {
					fields[kv[0]] = v
				}
			}
			return fields
		}
		// readCPUStat 返回 (usage_usec, nr_throttled):v2 单文件;
		// v1 = cpuacct.usage(ns→µs)+ cpu/cpu.stat 的 nr_throttled。
		readCPUStat := func() (int64, int64, error) {
			if raw, err := execCat("/sys/fs/cgroup/cpu.stat"); err == nil {
				if m := parseKV(raw); m["usage_usec"] != 0 || strings.Contains(raw, "usage_usec") {
					return m["usage_usec"], m["nr_throttled"], nil
				}
			}
			for _, usagePath := range []string{"/sys/fs/cgroup/cpu,cpuacct/cpuacct.usage", "/sys/fs/cgroup/cpuacct/cpuacct.usage"} {
				raw, err := execCat(usagePath)
				if err != nil {
					continue
				}
				ns, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if perr != nil {
					continue
				}
				var throttled int64
				for _, statPath := range []string{"/sys/fs/cgroup/cpu,cpuacct/cpu.stat", "/sys/fs/cgroup/cpu/cpu.stat"} {
					if sraw, serr := execCat(statPath); serr == nil {
						throttled = parseKV(sraw)["nr_throttled"]
						break
					}
				}
				return ns / 1000, throttled, nil
			}
			return 0, 0, fmt.Errorf("cpu accounting not readable via cgroup v2 cpu.stat or v1 cpuacct layouts")
		}
		t0 := time.Now()
		u0, th0, err := readCPUStat()
		if err != nil {
			if inspectFormat(t, name, "{{.State.Running}}") == "true" {
				t.Skipf("cpu metering unavailable (host cgroup layout): %v", err)
			}
			t.Fatalf("sample 1 (container no longer running?): %v", err)
		}
		time.Sleep(2 * time.Second)
		u1, th1, err := readCPUStat()
		t1 := time.Now()
		if err != nil {
			t.Fatalf("sample 2: %v", err)
		}
		wallUsec := t1.Sub(t0).Microseconds()
		if wallUsec <= 0 {
			t.Fatal("wall window collapsed")
		}
		rate := float64(u1-u0) / float64(wallUsec)
		if rate < t11CPURateMin || rate > t11CPURateMax {
			t.Fatalf("cpu rate = %.3f over %dus window (usage %d→%d), want [%v, %v] (quota 0.5 enforced; unthrottled spin ≈ 1.0)", rate, wallUsec, u0, u1, t11CPURateMin, t11CPURateMax)
		}
		if th1-th0 < 1 {
			t.Fatalf("nr_throttled delta = %d→%d, want >= 1 (CFS quota exhaustion)", th0, th1)
		}
	})

	t.Run("pids limit caps the fork chain at the cgroup budget", func(t *testing.T) {
		// 基线 #6:自再执行链全世代在场 → pids cgroup 预算内收口(stop=
		// budget 首选,eagain 为真触顶兜底;cap = 走完 max=200,只在
		// pids-limit 漂移缺失时出现,即红)。depth ≤ 64 是结构上界
		// (pids-limit 计所有任务,每代 ≥1)。宿主无漂移 = 链自终止 +
		// 零残留差分(文件头注记:宿主进程计数噪声主导,不背书)。
		argv := probeArgv(t, "t11-fork-t"+strconv.Itoa(slot), false, false, "selftest", "forkchain", "0", "200")
		out, err := dockerRun(t, argv)
		if err != nil {
			t.Fatalf("docker run (forkchain): %v\n%s", err, out)
		}
		var depth int
		var stop string
		if _, perr := fmt.Sscanf(string(out), "forkchain depth=%d stop=%s", &depth, &stop); perr != nil {
			t.Fatalf("forkchain report = %q, want 'forkchain depth=N stop=…'", out)
		}
		if stop != "budget" && stop != "eagain" {
			t.Fatalf("stop = %q, want budget or eagain (cap would mean pids limit not enforced)", stop)
		}
		if depth < 2 || depth > t11PidsTier {
			t.Fatalf("depth = %d, want 2 <= depth <= %d (pids-limit structural bound)", depth, t11PidsTier)
		}
	})

	t.Run("fd limit caps the open loop with emfile", func(t *testing.T) {
		// 基线 #7:循环 open 至 EMFILE,断言 512 ≤ n ≤ 1024(nofile 硬
		// 上限;软失败 = exit 0 + 报告行)。下限分离「限额远低于预期」形
		// 态;「旗标漂移缺失 + daemon 缺省同值」的贴界形态由 inspect
		// 矩阵的 Ulimits 全值等值比对独立闭合(分层依赖,单用例不背书)。
		argv := probeArgv(t, "t11-fds-t"+strconv.Itoa(slot), false, false, "selftest", "fds", "4096")
		out, err := dockerRun(t, argv)
		if err != nil {
			t.Fatalf("docker run (fds): %v\n%s", err, out)
		}
		var n int
		var stop string
		if _, perr := fmt.Sscanf(string(out), "fds=%d stop=%s", &n, &stop); perr != nil {
			t.Fatalf("fds report = %q, want 'fds=N stop=…'", out)
		}
		if stop != "emfile" {
			t.Fatalf("stop = %q, want emfile (ulimit enforcement)", stop)
		}
		if n < t11FDSFloor || n > t11NoFileLimit {
			t.Fatalf("open count = %d, want %d <= n <= %d (nofile=%d:%d)", n, t11FDSFloor, t11NoFileLimit, t11NoFileLimit, t11NoFileLimit)
		}
	})

	t.Run("daemon log cap rotates stderr spam", func(t *testing.T) {
		// 基线 #14:52MiB(非 5m 整倍数——整倍数轮转可清空驻留)stderr
		// 载荷 → json-file max-size=5m max-file=1 截留。docker logs 必须双流
		// 合并捕获:容器的 stderr 流走 CLI 自己的 stderr,单收 stdout(.Output)
		// 只剩报告行,断言退化成假绿(实测发现)。驻留总量非确定(轮转时点
		// 依赖 daemon 写侧计数),内容半边由尾部哨兵行锚定:轮转删头留尾,
		// last-write 哨兵必驻留 = stderr→json-file→logs 通道承载证明。上限
		// slack 128KiB = daemon stdio copier 单次读块粒度(非探针 4KiB 写块)。
		// LogConfig 配置半边已由 inspect 矩阵子用例钉死。
		name := "t11-log-t" + strconv.Itoa(slot)
		argv := probeArgv(t, name, false, true, "selftest", "spam-stderr", "52")
		defer rmKeep(t, name)
		var stdout bytes.Buffer
		cmd := exec.Command(sandboxDockerBin, argv...)
		cmd.Stdout = &stdout
		cmd.Stderr = io.Discard // 52MiB 附着流直线排空,不进测试进程内存
		if err := cmd.Run(); err != nil {
			t.Fatalf("docker run (spam-stderr): %v\nstdout: %s", err, stdout.String())
		}
		if !strings.Contains(stdout.String(), "spammed 52 MiB to stderr") {
			t.Fatalf("stdout = %q, want spam report", stdout.String())
		}
		logsCmd := exec.Command(sandboxDockerBin, "logs", name)
		var logs bytes.Buffer
		logsCmd.Stdout = &logs
		logsCmd.Stderr = &logs // 容器 stderr 流走 CLI stderr:合并才是完整截留面
		if err := logsCmd.Run(); err != nil {
			t.Fatalf("docker logs %s: %v", name, err)
		}
		if cap := 5<<20 + 128<<10; logs.Len() > cap {
			t.Fatalf("docker logs retained %d bytes, want <= %d (json-file max-size=5m max-file=1 rotation)", logs.Len(), cap)
		}
		if !strings.Contains(logs.String(), "braid-e2e-stderr-spam-done 52") {
			t.Fatalf("tail sentinel absent from docker logs (stderr→json-file→logs channel not carrying the payload?): %d bytes retained", logs.Len())
		}
	})

	t.Run("init reaps orphaned zombie children", func(t *testing.T) {
		// 基线 #15:zombies 探针双 fork 孤儿(转交 PID 1)→ tini 收割,
		// 沉降扫描 zombies=0;探针聚合为「轮询至归零」语义(瞬态收割窗
		// 容忍)。HostConfig.Init 配置半边已由 inspect 矩阵子用例钉死。
		argv := probeArgv(t, "t11-zomb-t"+strconv.Itoa(slot), false, false, "selftest", "zombies", "8")
		out, err := dockerRun(t, argv)
		if err != nil {
			t.Fatalf("docker run (zombies): %v\n%s", err, out)
		}
		var z int
		if _, perr := fmt.Sscanf(string(out), "zombies=%d", &z); perr != nil || z != 0 {
			t.Fatalf("zombies report = %q, want zombies=0 (tini reaps orphans; err=%v)", out, perr)
		}
	})

	t.Run("init drift control: orphans persist without tini", func(t *testing.T) {
		// 负向对照(剥 --init 漂移注入):PID 1 = helper 自身,Go runtime
		// 不 wait 转交孤儿 → 僵尸永续,zombies > 0。三重作用:(1) #15 的
		// 红路径证明(--init 漂移即红);(2) 探针检测力自证——zombies>0
		// 可达,探针致盲变异(如 zombie-child 改等孙进程)在此绿不了;
		// (3) 威胁列复合形态半边(孤儿僵尸 × PID 1 不收割;堆积顶满
		// pids 的完整形态由 #6 链自终止 + 零残留差分分解覆盖,基线 §2
		// #15 证明列已按此修订)。
		argv := probeArgv(t, "t11-noinit-t"+strconv.Itoa(slot), false, false, "selftest", "zombies", "8")
		noInit := make([]string, 0, len(argv))
		for _, a := range argv {
			if a != "--init" {
				noInit = append(noInit, a)
			}
		}
		out, err := dockerRun(t, noInit)
		if err != nil {
			t.Fatalf("docker run (zombies, --init stripped): %v\n%s", err, out)
		}
		var z int
		if _, perr := fmt.Sscanf(string(out), "zombies=%d", &z); perr != nil || z <= 0 {
			t.Fatalf("zombies report = %q, want > 0 without --init (orphan zombies must persist when PID 1 does not reap — probe blind?; err=%v)", out, perr)
		}
	})

	t.Run("carrier serves a normal call after overlimit kills", func(t *testing.T) {
		// 基线 #4 宿主断言的 e2e 代理:超限 kill 之后载体/daemon 仍健康,
		// 正常调用成功(kill 不拖垮后续执行面)。
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("chmod whitelist dir: %v", err)
		}
		file := dir + "/alive.txt"
		if err := os.WriteFile(file, []byte("post-kill\n"), 0o644); err != nil {
			t.Fatalf("write whitelist file: %v", err)
		}
		carrier, err := newSandboxCarrierFileRead(cfg, []string{dir}, newSandboxLimiter(8))
		if err != nil {
			t.Fatalf("newSandboxCarrierFileRead: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := carrier.Run(ctx, Call{
			Inputs:         map[string]any{"path": file},
			Token:          "scoped-token",
			IdempotencyKey: "att:t11:1",
		})
		if err != nil {
			t.Fatalf("post-kill normal call: %v", err)
		}
		if !strings.Contains(string(res.Output), "post-kill") {
			t.Fatalf("output = %s, want content of alive.txt", res.Output)
		}
	})

	t.Run("zero residue", func(t *testing.T) {
		// 差分按 ancestor=<本槽位 tag> 圈定(文件头注记):并行 lane 的
		// 容器结构性排除;仍命中即本 lane 真泄漏。红时附容器名辅助排障
		// (固定名残留 = 前轮 SIGKILL,差分半边自愈但同名撞名需手工 rm)。
		after := t11LabeledIDs(t)
		var leaked []string
		for id := range after {
			if !before[id] {
				leaked = append(leaked, id)
			}
		}
		if len(leaked) > 0 {
			names, _ := exec.Command(sandboxDockerBin, "ps", "-a",
				"--filter", "label=braid.sandbox=1", "--filter", "ancestor="+tag,
				"--format", "{{.Names}} {{.ID}}").CombinedOutput()
			t.Fatalf("sandbox containers leaked past --rm: %v\n(labeled containers on this tag: %s)", leaked, names)
		}
	})
}
