package api

// T15.6 mode=all 混跑窗硬闸契约面(D-t151 决策 6 + D-t142①):api 在
// 发布/触发含 mode=all 的工作流前查「在场实例(api/worker/reaper 三类)
// 全部宣告 approval_mode_all」,在场未宣告即拒(fail-closed,误放 < 误拒);
// 查询错误同拒;过期行不在场;某 kind 零在场 = vacuous 放行(停摆 ≠ 破坏,
// 发布序由 UPGRADE 人工纪律承载)。拒绝痕迹 = 响亮日志 + 计数器
//(audit_log 结构性执行锚定——writer validate 钉死 ExecutionID 非零,
// 物化前拒绝落不进执行事实域,零迁移禁新表,见 D-t142① 口径)。

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/scheduler"
)

// gateAllDoc 是含会签 mode=all 审批节点的合法 DSL(rollback_test 的
// approval-only 最小形态 + mode=all)。
func gateAllDoc(name string) []byte {
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"nodes":[{"id":"sign","type":"approval","approvers":["carol","dave"],"mode":"all","timeout_hours":24}],
"outputs":{}}`, name)
}

// seedGateLease 直插租约行(宣告面可空 = 未宣告;expires 偏移可负)。
func seedGateLease(t *testing.T, dbh *sql.DB, node int, kind, holder string, expiresIn time.Duration, declared bool) {
	t.Helper()
	var caps any
	if declared {
		caps = `["approval_mode_all"]`
	}
	if _, err := dbh.Exec(
		`INSERT INTO snowflake_node_lease (node_id, kind, holder_id, expires_at, capabilities)
		 VALUES (?, ?, ?, DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), ?)`,
		node, kind, holder, expiresIn.Microseconds(), caps); err != nil {
		t.Fatalf("seed gate lease: %v", err)
	}
}

func clearGateLeases(t *testing.T, dbh *sql.DB) {
	t.Helper()
	if _, err := dbh.Exec(`DELETE FROM snowflake_node_lease`); err != nil {
		t.Fatalf("clear gate leases: %v", err)
	}
}

// TestFleetGatePublishModeAll:发布面闸——表驱动覆盖 vacuous 放行/全宣告
// 放行/未宣告拒(503 + 诊断 body)/过期行不在场/非 mode=all 不触发闸。
func TestFleetGatePublishModeAll(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(t *testing.T, dbh *sql.DB)
		want    int
		wantErr string // 空 = 期望成功
	}{
		{
			name: "empty fleet vacuous pass",
			seed: func(*testing.T, *sql.DB) {},
			want: http.StatusOK,
		},
		{
			name: "all three kinds declared pass",
			seed: func(t *testing.T, dbh *sql.DB) {
				seedGateLease(t, dbh, 1, "api", "api:a1", time.Minute, true)
				seedGateLease(t, dbh, 2, "worker", "worker:w1", time.Minute, true)
				seedGateLease(t, dbh, 3, "reaper", "reaper:r1", time.Minute, true)
			},
			want: http.StatusOK,
		},
		{
			name: "undeclared in-fleet worker rejected",
			seed: func(t *testing.T, dbh *sql.DB) {
				seedGateLease(t, dbh, 1, "api", "api:a1", time.Minute, true)
				seedGateLease(t, dbh, 2, "worker", "worker:old-bin", time.Minute, false)
			},
			want:    http.StatusServiceUnavailable,
			wantErr: "fleet_capability_gate",
		},
		{
			name: "expired undeclared row not in fleet",
			seed: func(t *testing.T, dbh *sql.DB) {
				seedGateLease(t, dbh, 2, "worker", "worker:dead", -time.Second, false)
			},
			want: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, dbh, sign := newTestServer(t)
			clearGateLeases(t, dbh)
			defer clearGateLeases(t, dbh)
			tc.seed(t, dbh)
			author := sign(auth.RoleAuthor)
			created := createViaAPI(t, r, author, gateAllDoc(uniqName("gate")))
			id := created["id"].(string)
			code, body := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil)
			if code != tc.want {
				t.Fatalf("publish = %d %v, want %d", code, body, tc.want)
			}
			if tc.wantErr != "" {
				if body["error"] != tc.wantErr {
					t.Fatalf("error = %v, want %q", body["error"], tc.wantErr)
				}
				und, ok := body["undeclared"].([]any)
				if !ok || len(und) != 1 {
					t.Fatalf("undeclared = %v, want 1 行诊断列表", body["undeclared"])
				}
				row, _ := und[0].(map[string]any)
				if row["kind"] != "worker" || row["holder"] != "worker:old-bin" {
					t.Fatalf("undeclared[0] = %v, want worker:old-bin", und[0])
				}
			}
		})
	}
}

// TestFleetGatePublishNotEngagedWithoutModeAll:不含 mode=all 的发布在
// 未宣告在场行下照常放行——闸只在 DSL 含会签节点时查询判定(零开销路径)。
func TestFleetGatePublishNotEngagedWithoutModeAll(t *testing.T) {
	r, dbh, sign := newTestServer(t)
	clearGateLeases(t, dbh)
	defer clearGateLeases(t, dbh)
	seedGateLease(t, dbh, 2, "worker", "worker:old-bin", time.Minute, false)
	author := sign(auth.RoleAuthor)
	created := createViaAPI(t, r, author, dslDoc(uniqName("nogate")))
	id := created["id"].(string)
	if code, body := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil); code != http.StatusOK {
		t.Fatalf("publish without mode=all = %d %v, want 200(闸不触发)", code, body)
	}
}

// gateCountingTrigger 记录 Instantiate 调用数(闸拒绝必须发生在物化之前)。
type gateCountingTrigger struct {
	fakeTrigger
	instantiated int
}

func (g *gateCountingTrigger) Instantiate(ctx context.Context, p scheduler.InstantiateParams) (uint64, error) {
	g.instantiated++
	return g.fakeTrigger.Instantiate(ctx, p)
}

// TestFleetGateTriggerModeAll:触发面闸(triggerCore 单点 = REST + MCP 共享)
// ——未宣告在场行 → 503 且零物化;补宣告后 → 201 正常物化。
func TestFleetGateTriggerModeAll(t *testing.T) {
	trig := &gateCountingTrigger{fakeTrigger: fakeTrigger{id: 1}}
	r, dbh, sign := newTestServerWithTrigger(t, trig)
	clearGateLeases(t, dbh)
	defer clearGateLeases(t, dbh)
	author := sign(auth.RoleAuthor)
	name := uniqName("gatetrig")
	created := createViaAPI(t, r, author, gateAllDoc(name))
	id := created["id"].(string)
	if code, body := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil); code != http.StatusOK {
		t.Fatalf("publish under empty fleet = %d %v, want 200", code, body)
	}

	seedGateLease(t, dbh, 2, "worker", "worker:old-bin", time.Minute, false)
	code, body := doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(`{"definition_name":"`+name+`"}`))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("trigger with undeclared worker = %d %v, want 503", code, body)
	}
	if body["error"] != "fleet_capability_gate" {
		t.Fatalf("error = %v, want fleet_capability_gate", body["error"])
	}
	if trig.instantiated != 0 {
		t.Fatalf("Instantiate called %d times, want 0(闸先于物化)", trig.instantiated)
	}

	// 补宣告(滚动升级完成形态):同一行 UPDATE 后放行。
	if _, err := dbh.Exec(`UPDATE snowflake_node_lease SET capabilities = ? WHERE node_id = 2`,
		`["approval_mode_all"]`); err != nil {
		t.Fatalf("declare worker: %v", err)
	}
	code, body = doJSON(t, r, http.MethodPost, "/api/v1/executions", author,
		[]byte(`{"definition_name":"`+name+`"}`))
	if code != http.StatusCreated {
		t.Fatalf("trigger after declare = %d %v, want 201", code, body)
	}
	if trig.instantiated != 1 {
		t.Fatalf("Instantiate called %d times, want 1", trig.instantiated)
	}
}

// TestFleetGateScanErrorFailsClosed:在场查询错误 → 拒(误拒方向安全)。
// 白盒直调(HTTP 面走不到:def 解析先于闸,坏 DB 在更早处 500)。
func TestFleetGateScanErrorFailsClosed(t *testing.T) {
	bad := sqltest.MustOpenAt(t, "braid_api_test_gate")
	if err := bad.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	s := &Server{dbh: bad}
	rej := s.gateFleetApprovalAll(context.Background())
	if rej == nil {
		t.Fatalf("gate = nil, want fail-closed rejection on scan error")
	}
	if rej.ScanErr == nil {
		t.Fatalf("ScanErr = nil, want wrapped scan error")
	}
	if len(rej.Undeclared) != 0 {
		t.Fatalf("Undeclared = %v, want empty on scan error", rej.Undeclared)
	}
}

// TestWorkflowUsesApprovalAll:判定 helper 纯函数面(遍历 wf.Nodes 的
// Approval.Mode;any/缺省不命中;坏 JSON 响亮)。
func TestWorkflowUsesApprovalAll(t *testing.T) {
	allDoc := `{"dsl":"1.0","name":"gate-probe","nodes":[{"id":"a","type":"approval","approvers":["c"],"mode":"all"}],"outputs":{}}`
	anyDoc := `{"dsl":"1.0","name":"gate-probe","nodes":[{"id":"a","type":"approval","approvers":["c"],"mode":"any"}],"outputs":{}}`
	defDoc := `{"dsl":"1.0","name":"gate-probe","nodes":[{"id":"a","type":"approval","approvers":["c"]}],"outputs":{}}`
	for doc, want := range map[string]bool{allDoc: true, anyDoc: false, defDoc: false} {
		got, err := dslUsesApprovalAll([]byte(doc))
		if err != nil {
			t.Fatalf("dslUsesApprovalAll(%s): %v", doc, err)
		}
		if got != want {
			t.Fatalf("dslUsesApprovalAll(%s) = %v, want %v", doc, got, want)
		}
	}
	if _, err := dslUsesApprovalAll([]byte(strings.Repeat("{", 3))); err == nil {
		t.Fatalf("dslUsesApprovalAll(bad json) = nil error, want parse error")
	}
}
