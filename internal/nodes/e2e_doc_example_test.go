package nodes

// T8.2 端到端示例真链守护:docs/examples/doc-compliance-review.json 是用户
// 可见示例资产,本文件以同一条链的真跑证明它成立(单一真源:资产烂 → 测试
// 红,文档与实现不脱钩)。链 = 经 API 触发:file_read → LLM 解析 → LLM 合规
// 审查 → branch 分流 → 摘要 → 人工审批 → 报告。拓扑拍板(D-2026-09-24-t82-example-topology):
//   - branch 挂在合规结论上(issue #51「branch 合规审查(T3.4)」的正读),
//     pass → 摘要 → 审批 → 报告主线(= ROADMAP T8.2 原链);
//   - default → 修订建议独立尾,**不与主线汇聚**:advance 对任一 SKIPPED
//     前驱级联剪枝后继(sched.go:896),菱形汇聚点会被一并剪掉,形成
//     「SUCCEEDED 但报告为 null」的假成功——蓝本 testdata 的 report_revision
//     →report 汇聚边即此陷阱,示例资产不得采用。
// 三条路径:
//   - 合规 pass + 批准(主线):SUCCEEDED、report 产出、revision null;
//   - 合规 fail:branch default 边真实路由,主线级联 SKIPPED,产出 revision;
//   - 驳回:approval 节点终败(approval_rejected)+ fail-fast——引擎现义驳回
//     不进下游(resume.go failApprovalNode),示例文档同口径陈述。
// 审计验收(issue #51 降级口径):按 execution_id 串联全序断言完整事件链
// (ORDER BY id:雪花序=落行序);execution.triggered 行与首个节点事件的
// 写入方是两个进程(api 二段事务 vs worker),全序比较排除该行、单独断言
// 形状,避免跨进程竞态敏化。trace_id 恒 NULL 不伪造(T6.2 落地后补验)。
// 页面形态拍板 = API 模拟:审批走 Web 页同源 HTTP 序列(login → 待办 →
// 详情 → POST decision;webjourney_test.go 已证与内嵌页等价)。
// 定义提交与发布走真 API 面(POST /definitions + publish):发布点经
// registry 名单校验 file_read 白名单(T5.4 契约顺带真链复验)。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/api"
	"braid/internal/audit"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/engine"
	"braid/internal/llm"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
	"braid/internal/worker"
)

// 示例资产路径(相对本包目录)。测试直读用户可见资产而非内嵌副本。
const (
	docExampleDSLAsset    = "../../docs/examples/doc-compliance-review.json"
	docExampleSampleAsset = "../../docs/examples/sample-doc.txt"
	// docExampleSampleMarker:样例文档内标记,断言文件内容真实流到 LLM 桩。
	docExampleSampleMarker = "供应商答谢会"
	// docExampleStubText:桩全部 llm 节点 text 字段的回值(outputs 断言锚)。
	docExampleStubText = "t82:审查结论与报告正文占位"
)

// 主体(职责分离的示例语义):author 触发(tool RBAC 要求 author 角色真实
// 用户行,e2e_tool_test.go 拒绝链路即证),alice 审批(DSL approvers 名单 +
// approver 角色双门)。密码 = ensureE2EUser 固定值(真实 login 流程)。
const (
	docExampleAuthor   = "t82_author"
	docExampleApprover = "alice"
	docExamplePassword = "e2e-user-password-placeholder"
)

// docLLMStub:OpenAI 兼容 httptest 桩。单一超集 JSON 回值覆盖全部 llm 节点
// 的声明 outputs(extraction 按声明键抽取,超集无碍);compliance verdict
// 由构造参数钉死(分流方向的测试控制面)。
type docLLMStub struct {
	srv     *httptest.Server
	verdict string

	mu     sync.Mutex
	calls  int
	bodies []string
}

func newDocLLMStub(t *testing.T, complianceVerdict string) *docLLMStub {
	t.Helper()
	s := &docLLMStub{verdict: complianceVerdict}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.calls++
		s.bodies = append(s.bodies, string(b))
		s.mu.Unlock()
		content := fmt.Sprintf(
			`{"facts":["费用明细已核验","发票与申请单一致"],"kind":"报销申请",`+
				`"verdict":%q,"reason":"金额、附件与制度上限一致","text":%q}`,
			complianceVerdict, docExampleStubText)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-t82", "model": "glm-5.3",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": content},
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *docLLMStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *docLLMStub) sawSubstring(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bodies {
		if bytes.Contains([]byte(b), []byte(sub)) {
			return true
		}
	}
	return false
}

// docExampleEnv:一条示例链的全套真件装配。
type docExampleEnv struct {
	d          *sql.DB
	router     *gin.Engine
	stub       *docLLMStub
	samplePath string
	defName    string // 资产 $.name 单源(trigger 引用,资产改名不靠 404 兜底)
	defID      uint64
	execID     *uint64 // trigger 后物化;cleanup 读当前值
}

// newDocExampleEnv:装配 t13 真栈 + 全真件(worker Mux:FileReadTool 白名单
// tmpdir / LLMExecutor 桩供应商 / ApprovalExecutor)+ 真 gin api 面 + 定义
// 经 API 提交并发布。complianceVerdict 钉死桩的合规结论(分流控制面)。
func newDocExampleEnv(t *testing.T, complianceVerdict string) *docExampleEnv {
	t.Helper()
	d := sqltest.MustOpenAt(t, nodesSchema)
	ctx := context.Background()
	queue := fmt.Sprintf("t_n%d", atomic.AddUint32(&queueSeq, 1))

	// 单一真源:直读示例资产(缺失/烂 = 红)。
	dslBytes, err := os.ReadFile(docExampleDSLAsset)
	if err != nil {
		t.Fatalf("read example DSL asset: %v", err)
	}
	var dslName struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(dslBytes, &dslName); err != nil {
		t.Fatalf("example DSL asset not json: %v", err)
	}
	sampleBytes, err := os.ReadFile(docExampleSampleAsset)
	if err != nil {
		t.Fatalf("read sample asset: %v", err)
	}

	// file_read 白名单 = 本测试 tmpdir(真工具,真边界检查)。
	dir := t.TempDir()
	samplePath := filepath.Join(dir, "sample-doc.txt")
	if err := os.WriteFile(samplePath, sampleBytes, 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	fileRead, err := tools.NewFileReadTool(tools.FileReadConfig{Dirs: []string{dir}})
	if err != nil {
		t.Fatalf("file_read tool: %v", err)
	}
	reg, err := tools.NewRegistry(fileRead)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	// LLM 真件 + 桩供应商(生产适配器,方言翻译/幂等键面已被 T3.6 e2e 钉死)。
	stub := newDocLLMStub(t, complianceVerdict)
	llmCfg := llm.DefaultConfig()
	llmCfg.BaseURL = stub.srv.URL
	llmCfg.APIKey = "sk-t82"
	table, err := llm.ParsePriceTable(
		`{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`)
	if err != nil {
		t.Fatalf("price table: %v", err)
	}
	llmCfg.PriceTable = table
	client, err := llm.NewOpenAICompat(llmCfg, nil)
	if err != nil {
		t.Fatalf("llm client: %v", err)
	}

	// 主体:author 触发 / alice 审批。
	ensureE2EUser(t, d, docExampleAuthor, auth.RoleAuthor)
	ensureE2EUser(t, d, docExampleApprover, auth.RoleApprover)

	// worker 全真件(tool RBAC 真门)。
	toolExec := &ToolExecutor{
		Runner: tools.NewLocalRunner(reg, tools.NewTokenIssuer(nil, 0, nil), mustE2EAuthz(t, d)),
		DB:     d,
	}
	executor := NewMux(mustE2ESched(t, d, queue), &LLMExecutor{
		Client: client, Prices: table, DB: d,
	}, toolExec, ApprovalExecutor{})

	// api 面(真 gin 路由;registry 同款 → 发布白名单含 file_read)。
	apiIDs, err := snowflake.New(3)
	if err != nil {
		t.Fatalf("api snowflake: %v", err)
	}
	apiSched, err := scheduler.New(scheduler.Deps{DB: d, IDs: apiIDs, Queue: queue})
	if err != nil {
		t.Fatalf("api scheduler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	router := api.NewServer(api.Config{
		Addr: "127.0.0.1:0", JWTSecret: nodesE2ESecret, SnowflakeNode: 3, ResumeQueue: queue,
		// T6.4:登录防御旋钮放宽(三用例多轮 login 同 IP;防御行为由
		// api 包 loginlimit/loginaudit 用例锚定,本链不动)。
		LoginRateLimit: 100000, LoginRateWindow: time.Minute,
		LoginMaxFailures: 100000, LoginLockout: time.Minute,
	}, d, apiIDs, reg, apiSched, auth.BuiltinProvider{DB: d}).Router()

	startE2ERelay(t, d, ctx)
	workerCfg := worker.DefaultConfig()
	workerCfg.HeartbeatInterval = 40 * time.Millisecond
	startE2EWorker(t, d, executor, workerCfg, queue)

	// 定义走真 API 面:author 提交(原样 DSL 字节)→ 发布(白名单校验点)。
	tok := docLogin(t, router, docExampleAuthor)
	code, out := doDocJSON(t, router, http.MethodPost, "/api/v1/definitions", tok, dslBytes)
	if code != http.StatusCreated {
		t.Fatalf("create definition = %d %v, want 201", code, out)
	}
	draftID, _ := out["id"].(string)
	if draftID == "" {
		t.Fatalf("create definition response missing id: %v", out)
	}
	code, out = doDocJSON(t, router, http.MethodPost, "/api/v1/drafts/"+draftID+"/publish", tok, nil)
	if code != http.StatusOK || out["status"] != "PUBLISHED" {
		t.Fatalf("publish = %d %v, want 200 PUBLISHED", code, out)
	}
	defID, err := strconv.ParseUint(draftID, 10, 64)
	if err != nil {
		t.Fatalf("definition id %q: %v", draftID, err)
	}

	env := &docExampleEnv{
		d: d, router: router, stub: stub, samplePath: samplePath,
		defName: dslName.Name, defID: defID, execID: new(uint64),
	}
	t.Cleanup(func() { cleanupNodesE2E(t, env.d, *env.execID, env.defID) })
	return env
}

// trigger 经真 API 触发执行(input = 样例文档路径)。
func (e *docExampleEnv) trigger(t *testing.T) {
	t.Helper()
	tok := docLogin(t, e.router, docExampleAuthor)
	body := fmt.Sprintf(`{"definition_name":%q,"input":{"doc_path":%q}}`,
		e.defName, e.samplePath)
	code, out := doDocJSON(t, e.router, http.MethodPost, "/api/v1/executions", tok, []byte(body))
	if code != http.StatusCreated {
		t.Fatalf("trigger = %d %v, want 201", code, out)
	}
	if out["status"] != "PENDING" || out["trigger_type"] != "API" || out["triggered_by"] != docExampleAuthor {
		t.Fatalf("trigger ack = %v, want PENDING/API/%s", out, docExampleAuthor)
	}
	id, _ := out["id"].(string)
	execID, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		t.Fatalf("execution id %q: %v", id, err)
	}
	*e.execID = execID
}

// decideViaPageActions 以 Web 页同源 HTTP 序列驱动审批(页面动作面 =
// login → 待办首屏 → 详情 → POST 决定;webjourney_test.go 证的等价序)。
func (e *docExampleEnv) decideViaPageActions(t *testing.T, decision, comment string) {
	t.Helper()
	execID := *e.execID
	tok := docLogin(t, e.router, docExampleApprover)

	// ① 待办首屏:alice 恰见本执行。
	code, out := doDocJSON(t, e.router, http.MethodGet, "/api/v1/approvals/pending", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("pending = %d %v, want 200", code, out)
	}
	items, _ := out["items"].([]any)
	found := false
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["execution_id"] == strconv.FormatUint(execID, 10) {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending items = %v, want exec %d in list", items, execID)
	}

	// ② 详情读面。
	code, out = doDocJSON(t, e.router, http.MethodGet,
		fmt.Sprintf("/api/v1/executions/%d/approvals/human_review", execID), tok, nil)
	if code != http.StatusOK || out["node_status"] != "WAITING_APPROVAL" {
		t.Fatalf("approval detail = %d %v, want 200 WAITING_APPROVAL", code, out)
	}

	// ③ 决定。
	code, out = doDocJSON(t, e.router, http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/human_review/decision", execID), tok,
		[]byte(fmt.Sprintf(`{"decision":%q,"comment":%q}`, decision, comment)))
	if code != http.StatusCreated {
		t.Fatalf("decide(%s) = %d %v, want 201", decision, code, out)
	}
}

// fetchDetail:author 读执行详情(用户面读回的公共前缀:登录 + GET;调用方
// 断言 status/outputs/audit 指针)。
func (e *docExampleEnv) fetchDetail(t *testing.T) map[string]any {
	t.Helper()
	tok := docLogin(t, e.router, docExampleAuthor)
	code, out := doDocJSON(t, e.router, http.MethodGet,
		fmt.Sprintf("/api/v1/executions/%d", *e.execID), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("execution detail = %d %v, want 200", code, out)
	}
	return out
}

// TestE2EDocExampleApproveProducesReport:主线全链 —— 合规 pass + 批准 →
// SUCCEEDED;报告产出、修订尾 null(skip 级联);审计链按 execution 串联
// 全序完整;trace_id 恒 NULL(降级口径)。
func TestE2EDocExampleApproveProducesReport(t *testing.T) {
	env := newDocExampleEnv(t, "pass")
	env.trigger(t)
	execID := *env.execID

	waitE2EExecWaitingApproval(t, env.d, execID)
	env.decideViaPageActions(t, "approve", "同意,归档")
	waitE2EExecSucceeded(t, env.d, execID)

	// 节点终态:主线全 SUCCEEDED,独立尾 SKIPPED。
	for _, n := range []string{"read_doc", "parse", "compliance", "gate", "summary", "human_review", "report"} {
		assertE2ENodeState(t, env.d, execID, n, engine.NodeSucceeded)
	}
	assertE2ENodeState(t, env.d, execID, "report_revision", engine.NodeSkipped)

	// 用户面读回:报告 = 桩 text,修订 null,审计指针 trace 不伪造。
	out := env.fetchDetail(t)
	if out["status"] != "SUCCEEDED" {
		t.Fatalf("execution detail status = %v, want SUCCEEDED", out["status"])
	}
	outputs, _ := out["outputs"].(map[string]any)
	if outputs["report"] != docExampleStubText {
		t.Fatalf("outputs.report = %v, want stub text(报告物化)", outputs["report"])
	}
	if v, ok := outputs["revision"]; !ok || v != nil {
		t.Fatalf("outputs.revision = %v, want 显式 null(pass 路径独立尾被剪枝)", outputs["revision"])
	}
	auditPtr, _ := out["audit"].(map[string]any)
	if auditPtr["trace_id"] != nil {
		t.Fatalf("audit.trace_id = %v, want null(T6.2 前不伪造)", auditPtr["trace_id"])
	}
	if s, _ := auditPtr["exec_date"].(string); s == "" {
		t.Fatalf("audit.exec_date = %v, want 非空(分区定位键)", auditPtr["exec_date"])
	}

	// 审计链全序(execution 串联;排除跨进程竞态的 triggered 行)。
	assertDocAuditSequence(t, env.d, execID, []docAuditRow{
		{audit.EventNodeStateChanged, "read_doc"},
		{audit.EventToolCallCompleted, "read_doc"},
		{audit.EventNodeStateChanged, "read_doc"},
		{audit.EventNodeStateChanged, "parse"},
		{audit.EventLLMCallCompleted, "parse"},
		{audit.EventNodeStateChanged, "parse"},
		{audit.EventNodeStateChanged, "compliance"},
		{audit.EventLLMCallCompleted, "compliance"},
		{audit.EventNodeStateChanged, "compliance"},
		{audit.EventNodeStateChanged, "gate"},            // branch:PENDING→EXECUTING
		{audit.EventNodeStateChanged, "gate"},            // EXECUTING→SUCCEEDED(内联)
		{audit.EventBranchEvaluated, "gate"},             // next=summary
		{audit.EventNodeStateChanged, "report_revision"}, // 独立尾剪枝
		{audit.EventNodeStateChanged, "summary"},
		{audit.EventLLMCallCompleted, "summary"},
		{audit.EventNodeStateChanged, "summary"},
		{audit.EventNodeStateChanged, "human_review"}, // →EXECUTING
		{audit.EventNodeStateChanged, "human_review"}, // →WAITING_APPROVAL
		{audit.EventApprovalRequested, "human_review"},
		{audit.EventApprovalDecided, "human_review"},
		{audit.EventNodeStateChanged, "human_review"}, // resume:WAITING→EXECUTING
		{audit.EventNodeStateChanged, "human_review"}, // →SUCCEEDED
		{audit.EventNodeStateChanged, "report"},
		{audit.EventLLMCallCompleted, "report"},
		{audit.EventNodeStateChanged, "report"},
	})
	assertDocTriggeredRow(t, env.d, execID)
	assertDocTraceNeverForged(t, env.d, execID)

	// 数据面:文件内容真实流到 LLM(parse 的 user 块含样例标记);4 次 llm
	// 调用(parse/compliance/summary/report——revision 被剪枝零调用)。
	if !env.stub.sawSubstring(docExampleSampleMarker) {
		t.Fatal("sample doc content never reached the LLM stub(file_read → llm 数据面断裂)")
	}
	if got := env.stub.count(); got != 4 {
		t.Fatalf("llm stub calls = %d, want 4", got)
	}
}

// TestE2EDocExampleComplianceFailRoutesToRevision:合规不过 → branch
// default 边真实路由到修订尾,主线(摘要/审批/报告)级联 SKIPPED,SUCCEEDED
// 产出 revision、report null。两向分流 + 剪枝闭包的真链证明。
func TestE2EDocExampleComplianceFailRoutesToRevision(t *testing.T) {
	env := newDocExampleEnv(t, "fail")
	env.trigger(t)
	execID := *env.execID

	waitE2EExecSucceeded(t, env.d, execID)

	for _, n := range []string{"read_doc", "parse", "compliance", "gate", "report_revision"} {
		assertE2ENodeState(t, env.d, execID, n, engine.NodeSucceeded)
	}
	for _, n := range []string{"summary", "human_review", "report"} {
		assertE2ENodeState(t, env.d, execID, n, engine.NodeSkipped)
	}

	out := env.fetchDetail(t)
	if out["status"] != "SUCCEEDED" {
		t.Fatalf("execution detail status = %v, want SUCCEEDED", out["status"])
	}
	outputs, _ := out["outputs"].(map[string]any)
	if outputs["revision"] != docExampleStubText {
		t.Fatalf("outputs.revision = %v, want stub text(修订尾物化)", outputs["revision"])
	}
	if v, ok := outputs["report"]; !ok || v != nil {
		t.Fatalf("outputs.report = %v, want 显式 null(主线被剪枝)", outputs["report"])
	}

	assertDocAuditSequence(t, env.d, execID, []docAuditRow{
		{audit.EventNodeStateChanged, "read_doc"},
		{audit.EventToolCallCompleted, "read_doc"},
		{audit.EventNodeStateChanged, "read_doc"},
		{audit.EventNodeStateChanged, "parse"},
		{audit.EventLLMCallCompleted, "parse"},
		{audit.EventNodeStateChanged, "parse"},
		{audit.EventNodeStateChanged, "compliance"},
		{audit.EventLLMCallCompleted, "compliance"},
		{audit.EventNodeStateChanged, "compliance"},
		{audit.EventNodeStateChanged, "gate"},
		{audit.EventNodeStateChanged, "gate"},
		{audit.EventBranchEvaluated, "gate"},          // next=report_revision(default)
		{audit.EventNodeStateChanged, "summary"},      // 级联剪枝闭包
		{audit.EventNodeStateChanged, "human_review"}, //
		{audit.EventNodeStateChanged, "report"},       //
		{audit.EventNodeStateChanged, "report_revision"},
		{audit.EventLLMCallCompleted, "report_revision"},
		{audit.EventNodeStateChanged, "report_revision"},
	})
	assertDocTriggeredRow(t, env.d, execID)
	assertDocTraceNeverForged(t, env.d, execID)

	if got := env.stub.count(); got != 3 {
		t.Fatalf("llm stub calls = %d, want 3(parse/compliance/revision)", got)
	}
}

// TestE2EDocExampleApprovalRejectFailsFast:驳回 → approval 节点终败
// (approval_rejected)+ fail-fast:下游不推进(引擎现义,示例文档同口径)。
func TestE2EDocExampleApprovalRejectFailsFast(t *testing.T) {
	env := newDocExampleEnv(t, "pass")
	env.trigger(t)
	execID := *env.execID

	waitE2EExecWaitingApproval(t, env.d, execID)
	env.decideViaPageActions(t, "reject", "附件不全,退回")
	waitE2EExecFailed(t, env.d, execID)

	assertE2ENodeState(t, env.d, execID, "human_review", engine.NodeFailed)
	var codeCol sql.NullString
	if err := env.d.QueryRow(`SELECT error_code FROM node_execution WHERE execution_id=? AND node_id='human_review'`,
		execID).Scan(&codeCol); err != nil {
		t.Fatalf("read human_review error_code: %v", err)
	}
	if !codeCol.Valid || codeCol.String != "approval_rejected" {
		t.Fatalf("human_review error_code = %v, want approval_rejected", codeCol)
	}
	// 驳回时已推进到审批:上游全 SUCCEEDED(gate 在审批上游),独立尾已
	// 剪枝;仅审批下游的 report 留 PENDING。
	for _, n := range []string{"read_doc", "parse", "compliance", "gate", "summary"} {
		assertE2ENodeState(t, env.d, execID, n, engine.NodeSucceeded)
	}
	assertE2ENodeState(t, env.d, execID, "report_revision", engine.NodeSkipped)
	assertE2ENodeState(t, env.d, execID, "report", engine.NodePending)

	// 用户面:FAILED、无 outputs。
	out := env.fetchDetail(t)
	if out["status"] != "FAILED" {
		t.Fatalf("execution detail status = %v, want FAILED", out["status"])
	}
	if v, ok := out["outputs"]; !ok || v != nil {
		t.Fatalf("outputs = %v, want null(fail-fast 无产物)", out["outputs"])
	}

	assertDocAuditSequence(t, env.d, execID, []docAuditRow{
		{audit.EventNodeStateChanged, "read_doc"},
		{audit.EventToolCallCompleted, "read_doc"},
		{audit.EventNodeStateChanged, "read_doc"},
		{audit.EventNodeStateChanged, "parse"},
		{audit.EventLLMCallCompleted, "parse"},
		{audit.EventNodeStateChanged, "parse"},
		{audit.EventNodeStateChanged, "compliance"},
		{audit.EventLLMCallCompleted, "compliance"},
		{audit.EventNodeStateChanged, "compliance"},
		{audit.EventNodeStateChanged, "gate"},
		{audit.EventNodeStateChanged, "gate"},
		{audit.EventBranchEvaluated, "gate"},
		{audit.EventNodeStateChanged, "report_revision"},
		{audit.EventNodeStateChanged, "summary"},
		{audit.EventLLMCallCompleted, "summary"},
		{audit.EventNodeStateChanged, "summary"},
		{audit.EventNodeStateChanged, "human_review"},
		{audit.EventNodeStateChanged, "human_review"},
		{audit.EventApprovalRequested, "human_review"},
		{audit.EventApprovalDecided, "human_review"},
		{audit.EventNodeStateChanged, "human_review"}, // resume:WAITING→EXECUTING
		{audit.EventNodeStateChanged, "human_review"}, // →FAILED(approval_rejected)
	})
	assertDocTriggeredRow(t, env.d, execID)
	assertDocTraceNeverForged(t, env.d, execID)

	if got := env.stub.count(); got != 3 {
		t.Fatalf("llm stub calls = %d, want 3(parse/compliance/summary)", got)
	}
}

// ---- 断言基建 ----

// docAuditRow:断言用投影(nodeID "" = NULL 列)。
type docAuditRow struct {
	eventType string
	nodeID    string
}

// docAuditChain 读执行审计链(ORDER BY id:同事务内雪花序 = 落行序;
// execution_id 串联 = issue #51 降级口径的 trace 维度替身)。
func docAuditChain(t *testing.T, d *sql.DB, execID uint64) []docAuditRow {
	t.Helper()
	rows, err := d.Query(`SELECT event_type, node_id FROM audit_log WHERE execution_id=? ORDER BY id`, execID)
	if err != nil {
		t.Fatalf("query audit chain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []docAuditRow
	for rows.Next() {
		var e docAuditRow
		var node sql.NullString
		if err := rows.Scan(&e.eventType, &node); err != nil {
			t.Fatalf("scan audit chain: %v", err)
		}
		if node.Valid {
			e.nodeID = node.String
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit chain rows: %v", err)
	}
	return out
}

func assertDocAuditSequence(t *testing.T, d *sql.DB, execID uint64, want []docAuditRow) {
	t.Helper()
	// 排序假设(review LOW-4,注释钉住):链混用多个雪花发生器(api=node 3,
	// worker 侧调度器/执行器=node 2/0),跨发生器的「雪花序=落行序」只在
	// 时间戳毫秒差 >0 时成立。相邻的跨进程行(approval.decided → resume
	// 状态迁移)实际先后由 outbox→relay 墙钟间隔保证(测试 IdleInterval
	// 20ms,生产更大)——勿把 relay 轮询调到亚毫秒,否则此处开始假红。
	// T6.1:高频四类经 audit_pending 异步——读链前先排空(reaper flush 的
	// 测试内等价物);triggered 同步行恒在场,排除逻辑不受影响。
	flushStagedAudit(t, d)
	got := docAuditChain(t, d, execID)
	// triggered 行单独断言(跨进程写入,与首个节点事件的相对序不保证)。
	filtered := got[:0:0]
	for _, e := range got {
		if e.eventType != audit.EventExecutionTriggered {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) != len(want) {
		t.Fatalf("audit chain rows = %d, want %d\ngot:  %+v\nwant: %+v", len(filtered), len(want), got, want)
	}
	for i := range want {
		if filtered[i] != want[i] {
			t.Fatalf("audit[%d] = %+v, want %+v\ngot:  %+v\nwant: %+v", i, filtered[i], want[i], got, want)
		}
	}
}

// assertDocTriggeredRow:执行级首事件形状(单独断言,理由见文件头)。
func assertDocTriggeredRow(t *testing.T, d *sql.DB, execID uint64) {
	t.Helper()
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND event_type=? AND node_id IS NULL AND actor_id=?`,
		execID, audit.EventExecutionTriggered, docExampleAuthor).Scan(&n); err != nil {
		t.Fatalf("count execution.triggered: %v", err)
	}
	if n != 1 {
		t.Fatalf("execution.triggered rows = %d, want 1(actor=%s, node_id NULL)", n, docExampleAuthor)
	}
}

// assertDocTraceNeverForged:全链 trace_id 恒 NULL(T6.2 前不伪造,降级口径
// 的机器可检半边;T6.2 合入后本断言翻转为一致性断言)。
func assertDocTraceNeverForged(t *testing.T, d *sql.DB, execID uint64) {
	t.Helper()
	var n int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE execution_id=? AND trace_id IS NOT NULL`, execID).Scan(&n); err != nil {
		t.Fatalf("count forged trace: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows with non-NULL trace_id = %d, want 0(T6.2 前不伪造)", n)
	}
}

// doDocJSON:请求/响应 JSON 面的小兜底(节点 e2e 内)。
func doDocJSON(t *testing.T, r *gin.Engine, method, path, token string, body []byte) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON body (code %d): %q", w.Code, w.Body.String())
		}
	}
	return w.Code, out
}

// docLogin:真实凭据换 token(页面/CLI 同一面)。
func docLogin(t *testing.T, r *gin.Engine, username string) string {
	t.Helper()
	code, out := doDocJSON(t, r, http.MethodPost, "/api/v1/auth/login", "",
		[]byte(fmt.Sprintf(`{"username":%q,"password":%q}`, username, docExamplePassword)))
	if code != http.StatusOK {
		t.Fatalf("login(%s) = %d %v, want 200", username, code, out)
	}
	tok, _ := out["token"].(string)
	if tok == "" {
		t.Fatalf("login(%s) response missing token: %v", username, out)
	}
	return tok
}
