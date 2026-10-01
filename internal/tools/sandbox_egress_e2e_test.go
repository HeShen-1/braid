package tools

// t15.4 egress 多副本分配面 e2e(真 docker):同基段双 worker → 各持
// 实例段 --internal 网(独立子网独立网关 IP)→ 同端口 9210 双代理共存
// 双活。D-t151 决策 5 验收锚:「同网多 worker 不撞端口拒启」
// (D-t102④ 登记解除);同 worker 二启 = 同网关同端口 bind 拒启(显式
// 端口语义;PORT=0 例外见 TestSandboxEgressStartup 同族登记边界)。
//
// 零沙箱镜像面(无容器执行)——daemon 网络分配是唯一被测对象;槽位
// 隔离 = 基段名后缀(与 http/dbquery e2e 同公约,BRAID_TEST_REDIS_DB
// 语义即槽位号);网络所有权 = 本进程新建才注册 rm 清理(复用非自建
// 残留 = 前轮崩溃泄漏或同槽位公约违例,登记不动,sandboxEgressE2ENetwork
// 同款纪律)。

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestSandboxE2EEgressMultiReplica 双 worker 同基段同端口起 egress(真
// daemon):网名实例段互异、两网 --internal、网关 IP 互异、两代理同端口
// 双活(默认拒绝双探测);同 worker 复起 = listen 拒启。
func TestSandboxE2EEgressMultiReplica(t *testing.T) {
	sandboxDockerGate(t)
	slot := sandboxSlotNumber(t)
	base := "braid-sbx-egress-e2e-t" + strconv.Itoa(slot)

	workerA, workerB := "e2e-worker-a", "e2e-worker-b"
	netA, err := egressNetworkInstance(base, workerA)
	if err != nil {
		t.Fatalf("instance name A: %v", err)
	}
	netB, err := egressNetworkInstance(base, workerB)
	if err != nil {
		t.Fatalf("instance name B: %v", err)
	}

	// 所有权登记(just-in-time:各启动调用前逐网预检,在场即前轮残留,
	// 登记复用不清理,新建者带走)。残余竞态窗仅同槽位并发可触发(并行
	// 公约已禁;预检与 create 之间他者建网会误记所有权,误伤面 = 公约
	// 违例本身,沙箱镜像 tag 槽位隔离同款取舍)。
	owned := map[string]bool{}
	own := func(name string) {
		if err := exec.Command(sandboxDockerBin, "network", "inspect", name).Run(); err != nil {
			owned[name] = true
		} else {
			t.Logf("egress network %s already present (leaked from a crashed run or same-slot sibling); reusing without cleanup", name)
		}
	}
	t.Cleanup(func() {
		for name, mine := range owned {
			if !mine {
				continue
			}
			if err := exec.Command(sandboxDockerBin, "network", "rm", name).Run(); err != nil {
				t.Logf("cleanup: rm egress network %s: %v", name, err)
			}
		}
	})

	env := func() func(string) string {
		return envOf(map[string]string{
			"BRAID_TOOL_SANDBOX":                "docker",
			"BRAID_TOOL_SANDBOX_EGRESS_NETWORK": base,
			"BRAID_TOOL_SANDBOX_EGRESS_PORT":    "9210", // 验收口径的撞端口位
			"BRAID_HTTP_ENDPOINTS":              `{"t":"https://127.0.0.1:1"}`,
		})
	}

	own(netA)
	egA, err := SandboxEgressStartup(context.Background(), env(), workerA)
	if err != nil {
		t.Fatalf("worker A startup: %v", err)
	}
	own(netB)
	egB, err := SandboxEgressStartup(context.Background(), env(), workerB)
	if err != nil {
		t.Fatalf("worker B startup (same base+port must not collide): %v", err)
	}

	if egA.Network != netA || egB.Network != netB {
		t.Fatalf("networks = %q/%q, want %q/%q (instance derivation single-source)", egA.Network, egB.Network, netA, netB)
	}
	if egA.Network == egB.Network {
		t.Fatalf("distinct workers share network %q", egA.Network)
	}
	if egA.ProxyURL.Port() != "9210" || egB.ProxyURL.Port() != "9210" {
		t.Fatalf("ports = %q/%q, want both 9210", egA.ProxyURL.Port(), egB.ProxyURL.Port())
	}
	gwA, gwB := egA.ProxyURL.Hostname(), egB.ProxyURL.Hostname()
	if gwA == gwB {
		t.Fatalf("gateways collide: %q (distinct bridges must get distinct subnets)", gwA)
	}

	// 两网都须 --internal(默认拒绝姿态的拓扑前提,真 daemon 复核)。
	for _, name := range []string{netA, netB} {
		out, ierr := exec.Command(sandboxDockerBin, "network", "inspect", "--format", "{{.Internal}}", name).Output()
		if ierr != nil || strings.TrimSpace(string(out)) != "true" {
			t.Fatalf("network %s internal = %q (err %v), want true", name, strings.TrimSpace(string(out)), ierr)
		}
	}

	// 双代理同端口双活,同策略(默认拒绝:非白名 host 403)。
	for _, addr := range []string{egA.ProxyURL.Host, egB.ProxyURL.Host} {
		code, reason := egressProbe(t, addr, "evil.example:443")
		if code != 403 || reason != egressReasonHostBlocked {
			t.Fatalf("probe %s = %d %q, want 403 %s", addr, code, reason, egressReasonHostBlocked)
		}
	}

	// 同 worker 二启:幂等复用同一实例网 + 同网关同端口 bind 拒启
	// (owner 唯一部署契约的执法形态;活体同 owner 冲突 = 决策 8-③ 面)。
	if _, err := SandboxEgressStartup(context.Background(), env(), workerA); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("same-worker restart err = %v, want listen refusal", err)
	}
}
