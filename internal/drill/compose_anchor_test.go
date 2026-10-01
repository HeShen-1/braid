package drill

// 演练栈 compose 静态锚(零 docker 依赖,恒跑——防漂移门,TestSandboxDockerfile
// Anchors 先例):多副本形态的关键键一旦被无意改动(钉 container_name 回潮、
// AOF 丢失、宽限缩水、雪花钉常量回潮),在此响亮。语义断言由 drill_test.go
// 的真栈演练承载(BRAID_TEST_REQUIRE_DRILL=1)。

import (
	"os"
	"strings"
	"testing"
)

func drillComposeText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../deploy/drill.docker-compose.yml")
	if err != nil {
		t.Fatalf("read drill compose: %v", err)
	}
	return string(b)
}

func TestDrillComposeAnchors(t *testing.T) {
	yaml := drillComposeText(t)

	// 零 container_name 键:多副本面的存在前提(prod compose 钉名挡 --scale
	// 正是独立栈的立项理由,D-t151 决策 6)。键形态匹配,注释不算。
	if strings.Contains(yaml, "container_name:") {
		t.Fatalf("drill compose pins container_name (--scale 挡板回潮)")
	}
	// worker×2 / reaper×2 / api×1 / relay×1:多副本面 + api 路由钉死。
	for _, svc := range []string{"worker1:", "worker2:", "reaper1:", "reaper2:", "api:", "relay:"} {
		if !strings.Contains(yaml, "\n  "+svc) {
			t.Fatalf("drill compose missing service %q", strings.TrimSuffix(svc, ":"))
		}
	}
	// T15.3 部署契约:逐副本显式身份 + 停机宽限 ≥35s(缺省 10s < 30s 排水窗)。
	for _, w := range []string{"worker1", "worker2"} {
		if !strings.Contains(yaml, "hostname: braid-drill-"+w) ||
			!strings.Contains(yaml, "BRAID_WORKER_ID: braid-drill-"+w) {
			t.Fatalf("worker %s missing explicit hostname/BRAID_WORKER_ID(部署契约)", w)
		}
	}
	if !strings.Contains(yaml, "stop_grace_period: 35s") {
		t.Fatalf("drill compose missing stop_grace_period 35s(停机排水契约)")
	}
	// Redis 队列 = prod 契约(AOF everysec + 具名卷):Redis 重启臂的恢复
	// 真相源;t13 的无持久化形态在演练栈不可用。
	for _, want := range []string{"noeviction", "--appendonly", "everysec", "redis-queue-data:/data"} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("drill compose redis-queue missing %q(prod 契约)", want)
		}
	}
	if strings.Contains(yaml, "/var/lib/mysql") && strings.Contains(yaml, "tmpfs") == false {
		t.Fatalf("mysql persistence shape drifted")
	}
	// 雪花 auto:多副本语义(决策 2;钉常量回潮 = 双 worker 撞号拒启)。
	// YAML 锚合并下 auto 文面只在锚定义处——断言 = 锚定义 auto + 各副本
	// 正确继承锚,且全文件零数字钉。
	if !strings.Contains(yaml, "BRAID_API_SNOWFLAKE_NODE: auto") ||
		!strings.Contains(yaml, "BRAID_WORKER_SNOWFLAKE_NODE: auto") ||
		!strings.Contains(yaml, "BRAID_REAPER_SNOWFLAKE_NODE: auto") {
		t.Fatalf("drill compose missing SNOWFLAKE_NODE auto(多副本语义)")
	}
	for _, svc := range []string{"worker1", "worker2"} {
		if !strings.Contains(yaml, "<<: *worker-env") {
			t.Fatalf("worker %s not inheriting worker-env anchor", svc)
		}
	}
	for _, svc := range []string{"reaper1", "reaper2"} {
		if !strings.Contains(yaml, "<<: *reaper-env") {
			t.Fatalf("reaper %s not inheriting reaper-env anchor", svc)
		}
	}
	for _, pin := range []string{`SNOWFLAKE_NODE: "1"`, `SNOWFLAKE_NODE: "2"`, `SNOWFLAKE_NODE: "3"`} {
		if strings.Contains(yaml, pin) {
			t.Fatalf("drill compose pins %q(常量钉回潮)", pin)
		}
	}
	// 断言面端口:api/mysql/redis-queue 按槽位发布(MySQL/HTTP/docker CLI
	// 断言面 + host-netns worker 的消费面;零 t13 Redis 消费);其余组件
	// 不得有宿主端口。
	portsCount := strings.Count(yaml, "\"127.0.0.1:${BRAID_DRILL_")
	if portsCount != 3 {
		t.Fatalf("published loopback port count = %d, want 3(api+mysql+redis)", portsCount)
	}
	if !strings.Contains(yaml, "network_mode: host") {
		t.Fatalf("workers missing network_mode: host(egress 网关绑定硬要求)")
	}
	// docker.sock 只挂 worker 锚(沙箱清扫臂的实路径;api/relay/reaper 不给)
	//——文本一次在 x-drill-worker 锚,worker1/2 经 <<: *drill-worker 继承。
	if strings.Count(yaml, "/var/run/docker.sock:/var/run/docker.sock") != 1 {
		t.Fatalf("docker.sock mount count != 1 (worker anchor only)")
	}
	if strings.Count(yaml, "<<: *drill-worker") != 2 {
		t.Fatalf("worker anchor inherit count != 2")
	}
}
