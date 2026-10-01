package tools

// db_query 沙箱 e2e(T10.4,真 docker + t13 栈):生产装配构造器全链路——
// 容器内 helper 重建 db_query(DSN/代理词形经 stdin 信封)→ MySQL 拨号经
// egress 代理 CONNECT 明文隧道(容器唯一可达面 = 网关代理,T10.2 拓扑)→
// 钉死条目豁免环回禁段(D-2026-09-30-t104-dbquery-egress)→ t13 MySQL →
// 结果跨信封回传。负向:未钉死宿主被代理 403 host_blocked,哨兵经信封
// 保真(errors.Is 命中 runner 既有映射,D-t101 #6 同判)。
//
// 前置:docker 在场(缺即 skip,CI BRAID_TEST_REQUIRE_STACK=1 下红)+
// t13 栈(缺即 skip)。镜像 tag/网络名按 BRAID_TEST_REDIS_DB 槽位隔离
// (并行公约;worktree 必须 export 槽位,缺省 0 = 主仓 lane)。

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestSandboxE2EDBQuery(t *testing.T) {
	sandboxDockerGate(t)
	slot := sandboxSlotNumber(t)
	dsn, _ := dbqStackFixture(t) // t13 门内嵌:栈不可达 skip(REQUIRE_STACK=1 红);admin 侧 e2e 不用

	e2eTag := "braid-tool-sandbox:e2e-t" + strconv.Itoa(slot)
	// db_query 无 TLS 面(MySQL 协议;链路加密留运营方 DSN tls=),但镜像
	// 构建契约要求 CA 材料在场——生成不承载真实目标的 ephemeral CA
	// (SAN 占位 127.0.0.1,该 CA 永不参与任何 TLS 配对)。
	m := generateSandboxCA(t, []net.IP{net.ParseIP("127.0.0.1")})
	sandboxBuildE2EImage(t, e2eTag, m.CACertPEM)
	t.Cleanup(func() { _ = exec.Command(sandboxDockerBin, "rmi", e2eTag).Run() })

	netName := "braid-sbx-egress-t" + strconv.Itoa(slot)
	gwIP := sandboxEgressE2ENetwork(t, netName)

	// 钉死条目单源派生(与生产 SandboxEgressStartup 同门);代理绑网关,
	// allow 集 = 零 http 条目 + 单 DSN 派生条目(默认拒绝姿态的可观测最小面)。
	h, p, err := EgressAllowFromDSN(dsn)
	if err != nil {
		t.Fatalf("EgressAllowFromDSN: %v", err)
	}
	proxyLn, err := net.Listen("tcp", net.JoinHostPort(gwIP, "0"))
	if err != nil {
		t.Fatalf("bind egress proxy on gateway %s: %v", gwIP, err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	proxy := NewEgressProxy(EgressConfig{
		AllowInternal: map[string]map[int]struct{}{h: {p: {}}},
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- proxy.Serve(proxyLn) }()
	proxyURL := &url.URL{Scheme: "http", Host: proxyLn.Addr().String()}

	cfg := sandboxConfig{
		Docker:         true,
		Image:          e2eTag,
		Memory:         "256m",
		CPUs:           "0.5",
		PidsLimit:      64,
		MaxConcurrency: 8,
		EgressNetwork:  netName,
		EgressAddr:     proxyURL,
		Owner:          sandboxTestOwner(),
	}
	carrier, err := newSandboxCarrierDBQuery(cfg, dsn, newSandboxLimiter(8))
	if err != nil {
		t.Fatalf("newSandboxCarrierDBQuery: %v", err)
	}

	t.Run("select through container tunnel egress proxy mysql", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res, err := carrier.Run(ctx, Call{
			Inputs: map[string]any{
				"sql":  "SELECT id, name FROM t WHERE id = ?",
				"args": []any{json.Number("2")},
			},
			Token:          "scoped-token",
			IdempotencyKey: "att:t104:1",
		})
		if err != nil {
			t.Fatalf("db_query through sandbox: %v", err)
		}
		dec := json.NewDecoder(strings.NewReader(string(res.Output)))
		dec.UseNumber()
		var out map[string]any
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("unmarshal output %s: %v", res.Output, err)
		}
		if out["row_count"].(json.Number).String() != "1" {
			t.Fatalf("row_count = %v, want 1 (output %s)", out["row_count"], res.Output)
		}
		row := out["rows"].([]any)[0].([]any)
		if row[0].(json.Number).String() != "2" || row[1] != "O'Brien" {
			t.Fatalf("row = %v, want [2 O'Brien]", row)
		}
	})

	t.Run("aggregate select through container chain", func(t *testing.T) {
		// 无参调用同链(COUNT 聚合回传精确数值);行上限的报错语义在
		// integ 已按同源构造器覆盖,此处证容器链路的第二条路径。
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res, err := carrier.Run(ctx, Call{
			Inputs: map[string]any{"sql": "SELECT COUNT(*) AS n FROM t"},
		})
		if err != nil {
			t.Fatalf("count through sandbox: %v", err)
		}
		dec := json.NewDecoder(strings.NewReader(string(res.Output)))
		dec.UseNumber()
		var out map[string]any
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("unmarshal output %s: %v", res.Output, err)
		}
		if got := out["rows"].([]any)[0].([]any)[0].(json.Number).String(); got != "5" {
			t.Fatalf("count = %s, want 5", got)
		}
	})

	t.Run("non-pinned host denied with sentinel across envelope", func(t *testing.T) {
		// 未钉死宿主(127.0.0.2,代理侧策略门 host 分桶)→ 明文隧道拨号器
		// egressDenialError → helper 信封 host_blocked → 宿主重抛真哨兵。
		bad, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("parse dsn variant: %v", err)
		}
		if _, port, serr := net.SplitHostPort(bad.Addr); serr == nil {
			bad.Addr = net.JoinHostPort("127.0.0.2", port)
		} else {
			bad.Addr = "127.0.0.2:3306"
		}
		deniedCarrier, err := newSandboxCarrierDBQuery(cfg, bad.FormatDSN(), newSandboxLimiter(8))
		if err != nil {
			t.Fatalf("newSandboxCarrierDBQuery (denied): %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_, err = deniedCarrier.Run(ctx, Call{Inputs: map[string]any{"sql": "SELECT id FROM t"}})
		if !errors.Is(err, ErrHostBlocked) {
			t.Fatalf("err = %v, want ErrHostBlocked (pinned-entry default deny, sentinel crossing envelope)", err)
		}
	})
}
