// MCP server 面(T13.1,D-2026-09-30-t131-mcp-server):平台工作流暴露
// 为 MCP 工具,外部 agent 可发现(list)/触发(trigger)/查询(query)。
// 边界:挂在主 api 二进制 /mcp 路由(非独立入口),工具 handler 直调
// service 缝(scheduler.Instantiate / Store 查询族)——不复用 internal/tools
// 执行面(节点运行时工具域,消费向桥接归 t13.2)。
//
// 协议版本钉死 2026-07-28(D-t131 决策 2):SupportedProtocolVersions 单
// 元素硬钉(该选项只能收窄,单元素即拒绝一切协商),stateless 语义是该
// 版本规范强制——无 initialize 握手、无 Mcp-Session-Id、POST-only(非
// POST 405)、每请求临时会话;与 api 面水平扩展姿态同构,零 session
// 存储/零粘性路由。鉴权 = 同一 Authenticate 判定核 + token 作用域(scp
// claim,决策 3):作用域不足 = 工具结果面 IsError(HTTP 200,JSON-RPC
// 语义层拒绝;token 无效/缺失仍归 401)。租户:tnt 透传全部 Store 调用,
// 跨租户 404 面与 REST 同判。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"braid/internal/auth"
	"braid/internal/engine"
)

// mcpProtocolVersion 是唯一支持的 MCP 协议版本(D-t131 决策 2 钉死;
// 错版本客户端收 SDK 版本错误——明拒优于暗降,版本协商面不开放)。
const mcpProtocolVersion = "2026-07-28"

// mcpClaimsCtxKey 是 claims 从 gin 面注入 SDK request ctx 的键(工具
// handler 的唯一主体来源;工具面不读 gin context——SDK 拿到的是改造后
// 的 *http.Request)。
type mcpClaimsCtxKey struct{}

// claimsFromMCP 取注入的认证主体;缺席 = 接线 bug(路由必经 mcpEntry),
// 工具面 fail-closed 拒绝。
func claimsFromMCP(ctx context.Context) (auth.Claims, error) {
	claims, ok := ctx.Value(mcpClaimsCtxKey{}).(auth.Claims)
	if !ok {
		return auth.Claims{}, errors.New("mcp: unauthenticated context")
	}
	return claims, nil
}

// requireMCPScope 是工具面作用域闸:不足 = 带 scope 名的工具错误(IsError
// 结果;D-t131 决策 5 语义面)。
func requireMCPScope(ctx context.Context, want auth.Scope) error {
	claims, err := claimsFromMCP(ctx)
	if err != nil {
		return err
	}
	return auth.RequireScope(claims, want)
}

// mcpStoreError 把 store 错误映射为工具结果面错误(镜像 REST mapStoreError
// 语义):ErrNotFound → not_found、ErrConflict → conflict、其余 generic
// internal 且底层细节只进 slog(评审 MEDIUM:DB 错误文本泄进工具结果面
// = 泄给本面最不受信受众,且 DB 故障误标 not_found 会误导自动化 agent
// 判「工作流已删除」)。
func mcpStoreError(op string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return errors.New("not_found")
	case errors.Is(err, ErrConflict):
		return errors.New("conflict")
	default:
		slog.Error("api: mcp "+op, "err", err)
		return errors.New("internal")
	}
}

// mcpPageIn 是列表工具共用分页参数(REST parsePageParams 同值域:缺省
// limit=defaultPageLimit,上界 maxPageLimit;offset 上界 maxPageOffset)。
type mcpPageIn struct {
	Limit  int `json:"limit,omitempty" jsonschema:"page size, default 20, max 100"`
	Offset int `json:"offset,omitempty" jsonschema:"page offset, max 10000"`
}

func (p mcpPageIn) resolve() (limit, offset int, err error) {
	limit = p.Limit
	if limit == 0 {
		limit = defaultPageLimit
	}
	if limit < 1 || limit > maxPageLimit {
		return 0, 0, fmt.Errorf("bad_limit: want 1..%d, got %d", maxPageLimit, p.Limit)
	}
	if p.Offset < 0 || p.Offset > maxPageOffset {
		return 0, 0, fmt.Errorf("bad_offset: want 0..%d, got %d", maxPageOffset, p.Offset)
	}
	return limit, p.Offset, nil
}

// --- braid_list_workflows(workflows:read)---

type listWorkflowsIn struct {
	Status string    `json:"status,omitempty" jsonschema:"filter: DRAFT|PUBLISHED|ARCHIVED, empty = all"`
	Page   mcpPageIn `json:"page,omitempty"`
}

type mcpWorkflowOut struct {
	Name      string     `json:"name"`
	Version   *uint32    `json:"version"`
	Status    string     `json:"status"`
	UpdatedAt time.Time  `json:"updated_at"`
	Published *time.Time `json:"published_at,omitempty"`
}

type listWorkflowsOut struct {
	Workflows []mcpWorkflowOut `json:"workflows"`
	Total     int              `json:"total"`
}

func (s *Server) mcpListWorkflows(ctx context.Context, _ *mcp.CallToolRequest, in listWorkflowsIn) (*mcp.CallToolResult, listWorkflowsOut, error) {
	if err := requireMCPScope(ctx, auth.ScopeWorkflowsRead); err != nil {
		return nil, listWorkflowsOut{}, err
	}
	status := in.Status
	switch status {
	case "", statusDraft, statusPublished, statusArchived:
	default:
		return nil, listWorkflowsOut{}, fmt.Errorf("bad_status: %q", status)
	}
	limit, offset, err := in.Page.resolve()
	if err != nil {
		return nil, listWorkflowsOut{}, err
	}
	claims, _ := claimsFromMCP(ctx)
	items, total, err := s.st.List(ctx, s.dbh, claims.Tenant, status, limit, offset)
	if err != nil {
		return nil, listWorkflowsOut{}, mcpStoreError("list definitions", err)
	}
	out := listWorkflowsOut{Workflows: make([]mcpWorkflowOut, 0, len(items)), Total: total}
	for _, d := range items {
		out.Workflows = append(out.Workflows, mcpWorkflowOut{
			Name: d.Name, Version: d.Version, Status: d.Status,
			UpdatedAt: d.UpdatedAt, Published: d.PublishedAt,
		})
	}
	return nil, out, nil
}

// --- braid_get_workflow(workflows:read)---

type getWorkflowIn struct {
	Name    string  `json:"name" jsonschema:"workflow definition name"`
	Version *uint32 `json:"version,omitempty" jsonschema:"pinned version, empty = latest PUBLISHED"`
}

type getWorkflowOut struct {
	Name        string    `json:"name"`
	Version     *uint32   `json:"version"`
	Status      string    `json:"status"`
	Description string    `json:"description"`
	DSLHash     string    `json:"dsl_hash"`
	DSL         string    `json:"dsl"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *Server) mcpGetWorkflow(ctx context.Context, _ *mcp.CallToolRequest, in getWorkflowIn) (*mcp.CallToolResult, getWorkflowOut, error) {
	if err := requireMCPScope(ctx, auth.ScopeWorkflowsRead); err != nil {
		return nil, getWorkflowOut{}, err
	}
	if in.Version != nil && *in.Version == 0 {
		return nil, getWorkflowOut{}, errors.New("bad_version: 0")
	}
	claims, _ := claimsFromMCP(ctx)
	var (
		def Definition
		err error
	)
	if in.Version == nil {
		def, err = s.st.LatestPublished(ctx, s.dbh, claims.Tenant, in.Name)
	} else {
		def, err = s.st.GetVersion(ctx, s.dbh, claims.Tenant, in.Name, *in.Version)
	}
	if err != nil {
		return nil, getWorkflowOut{}, mcpStoreError("get workflow", err)
	}
	return nil, getWorkflowOut{
		Name: def.Name, Version: def.Version, Status: def.Status,
		Description: def.Description, DSLHash: def.DSLHash,
		DSL: string(def.DSL), UpdatedAt: def.UpdatedAt,
	}, nil
}

// --- braid_trigger_workflow(executions:write)---

// triggerWorkflowIn 的 Input 用 map[string]json.RawMessage(逐值原始字节)
// 而非 map[string]any(评审 MEDIUM:any 经 SDK 解码成 float64,>2^53 整数
// 被静默折叠——与 REST 原始 JSON 路径的输入保真面不一致);RawMessage 的
// schema 推断产出 null/array(不可用),故该工具的 InputSchema 在 AddTool
// 处显式给定(bypass 推断),值层零损:json.Marshal(map[string]RawMessage)
// 与 REST 手写 body 同字节语义。
type triggerWorkflowIn struct {
	DefinitionName string                     `json:"definition_name" jsonschema:"workflow definition name"`
	Version        *uint32                    `json:"version,omitempty" jsonschema:"pinned version, empty = latest PUBLISHED"`
	Input          map[string]json.RawMessage `json:"input,omitempty" jsonschema:"workflow input object, empty = zero input"`
	IdempotencyKey string                     `json:"idempotency_key,omitempty" jsonschema:"client idempotency key, dedup scope = subject+definition"`
}

type triggerWorkflowOut struct {
	executionAckDTO
	Replayed bool `json:"replayed" jsonschema:"true = idempotency replay of the first execution"`
}

func (s *Server) mcpTriggerWorkflow(ctx context.Context, _ *mcp.CallToolRequest, in triggerWorkflowIn) (*mcp.CallToolResult, triggerWorkflowOut, error) {
	if err := requireMCPScope(ctx, auth.ScopeExecutionsWrite); err != nil {
		return nil, triggerWorkflowOut{}, err
	}
	claims, _ := claimsFromMCP(ctx)
	var input json.RawMessage
	if len(in.Input) > 0 {
		b, err := json.Marshal(in.Input)
		if err != nil {
			return nil, triggerWorkflowOut{}, fmt.Errorf("bad_input: %v", err)
		}
		input = b
	}
	out, terr := s.triggerCore(ctx, claims, triggerRequest{
		DefinitionName: in.DefinitionName,
		Version:        in.Version,
		Input:          input,
	}, in.IdempotencyKey)
	if terr != nil {
		// internal 细节只进日志(REST writeTriggerError 同判:错误响应/
		// 工具结果面恒 generic,不泄底层)。
		if terr.code == triggerInternal && terr.err != nil {
			slog.Error("api: mcp trigger", "err", terr.err)
			return nil, triggerWorkflowOut{}, errors.New("internal")
		}
		return nil, triggerWorkflowOut{}, errors.New(string(terr.code))
	}
	return nil, triggerWorkflowOut{executionAckDTO: out.ack, Replayed: out.replayed}, nil
}

// --- braid_get_execution(executions:read)---

type getExecutionIn struct {
	ID string `json:"id" jsonschema:"execution id (decimal string)"`
}

// getExecutionOut 镜像 executionDetailDTO,但 Input/Outputs 用 any 而非
// json.RawMessage:SDK 对 RawMessage 的 schema 推断产出 null/array 形
// (与任意对象正文冲突,输出校验自拒);any 推断为通配,语义不变(nil =
// null,与 REST detail 的 null 面同判)。
type getExecutionOut struct {
	executionDTO
	DefinitionHash string                `json:"definition_hash"`
	Input          any                   `json:"input"`
	Outputs        any                   `json:"outputs"`
	Nodes          []nodeExecutionDTO    `json:"nodes"`
	Audit          executionAuditPointer `json:"audit"`
}

// rawToAny 把 DB 侧原始 JSON 字节解码为 any(空 = nil = null 面)。解码
// 失败按 nil 处理——不可达防御(DB 值均经写入侧 JSON 语义约束),非静默
// 吞错路径。
func rawToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

func (s *Server) mcpGetExecution(ctx context.Context, _ *mcp.CallToolRequest, in getExecutionIn) (*mcp.CallToolResult, getExecutionOut, error) {
	if err := requireMCPScope(ctx, auth.ScopeExecutionsRead); err != nil {
		return nil, getExecutionOut{}, err
	}
	id, err := strconv.ParseUint(in.ID, 10, 64)
	if err != nil || id == 0 {
		return nil, getExecutionOut{}, fmt.Errorf("bad_id: %q", in.ID)
	}
	claims, _ := claimsFromMCP(ctx)
	exec, err := s.st.GetExecution(ctx, s.dbh, claims.Tenant, id)
	if err != nil {
		return nil, getExecutionOut{}, mcpStoreError("get execution "+in.ID, err)
	}
	nodes, err := s.st.ListNodeExecutions(ctx, s.dbh, id)
	if err != nil {
		return nil, getExecutionOut{}, mcpStoreError("list nodes", err)
	}
	nodeOut := make([]nodeExecutionDTO, 0, len(nodes))
	for _, n := range nodes {
		nodeOut = append(nodeOut, toNodeExecutionDTO(n))
	}
	return nil, getExecutionOut{
		executionDTO:   toExecutionDTO(exec),
		DefinitionHash: exec.DefinitionHash,
		Input:          rawToAny(exec.Input),
		Outputs:        rawToAny(exec.Outputs),
		Nodes:          nodeOut,
		Audit:          executionAuditPointer{ExecDate: exec.ExecDate, TraceID: exec.TraceID},
	}, nil
}

// --- braid_list_executions(executions:read)---

type listExecutionsIn struct {
	Name   string    `json:"name,omitempty" jsonschema:"filter by workflow name"`
	Status string    `json:"status,omitempty" jsonschema:"filter by status, empty = all"`
	Page   mcpPageIn `json:"page,omitempty"`
}

type listExecutionsOut struct {
	Executions []executionDTO `json:"executions"`
	Total      int            `json:"total"`
}

func (s *Server) mcpListExecutions(ctx context.Context, _ *mcp.CallToolRequest, in listExecutionsIn) (*mcp.CallToolResult, listExecutionsOut, error) {
	if err := requireMCPScope(ctx, auth.ScopeExecutionsRead); err != nil {
		return nil, listExecutionsOut{}, err
	}
	switch in.Status {
	case "", string(engine.WorkflowPending), string(engine.WorkflowRunning),
		string(engine.WorkflowWaitingApproval), string(engine.WorkflowSucceeded),
		string(engine.WorkflowFailed), string(engine.WorkflowCancelled):
	default:
		return nil, listExecutionsOut{}, fmt.Errorf("bad_status: %q", in.Status)
	}
	limit, offset, err := in.Page.resolve()
	if err != nil {
		return nil, listExecutionsOut{}, err
	}
	claims, _ := claimsFromMCP(ctx)
	items, total, err := s.st.ListExecutions(ctx, s.dbh, claims.Tenant, in.Name, in.Status, limit, offset)
	if err != nil {
		return nil, listExecutionsOut{}, mcpStoreError("list executions", err)
	}
	out := listExecutionsOut{Executions: make([]executionDTO, 0, len(items)), Total: total}
	for _, e := range items {
		out.Executions = append(out.Executions, toExecutionDTO(e))
	}
	return nil, out, nil
}

// buildMCPServer 装配 MCP server 实例:版本硬钉(单元素)+ 五工具。
// 工具面作用域/租户判定在 handler 内(requireMCPScope/claimsFromMCP),
// 不在 SDK 层——SDK 面是协议编解码,平台语义单点在 api 包。
func (s *Server) buildMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "braid", Version: "v1"}, &mcp.ServerOptions{
		SupportedProtocolVersions: []string{mcpProtocolVersion},
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "braid_list_workflows",
		Description: "List workflow definitions visible to the calling tenant (paged).",
	}, s.mcpListWorkflows)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "braid_get_workflow",
		Description: "Get a workflow definition (DSL body, hash, status); latest PUBLISHED or pinned version.",
	}, s.mcpGetWorkflow)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "braid_trigger_workflow",
		Description: "Trigger a workflow run (async; returns execution id and PENDING ack; idempotency_key dedups per subject+definition).",
		// InputSchema 显式给定(评审 MEDIUM):In.Input 是 map[string]json.RawMessage,
		// 其 schema 推断产出 null/array 不可用;显式 object+通配值层保 >2^53
		// 整数精度(any 解码 → float64 折叠,与 REST 输入保真面不一致)。
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"definition_name": {"type": "string", "description": "workflow definition name"},
				"version": {"type": "integer", "description": "pinned version, omitted = latest PUBLISHED"},
				"input": {"type": "object", "description": "workflow input object, omitted = zero input"},
				"idempotency_key": {"type": "string", "description": "client idempotency key, dedup scope = subject+definition"}
			},
			"required": ["definition_name"],
			"additionalProperties": false
		}`),
	}, s.mcpTriggerWorkflow)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "braid_get_execution",
		Description: "Get execution status, node statuses, inputs/outputs and audit pointer.",
	}, s.mcpGetExecution)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "braid_list_executions",
		Description: "List executions visible to the calling tenant, optional name/status filter (paged).",
	}, s.mcpListExecutions)
	return srv
}

// mcpEntry 是 /mcp 的单路由入口:stateless 语义先于认证截断非 POST
// (405,与 SDK stateless 模式同判),POST 走 Authenticate 判定核 → claims
// 注入 request ctx → SDK handler。认证失败面貌 = REST 同款(401 码族)。
func (s *Server) mcpEntry(h http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.AbortWithStatus(http.StatusMethodNotAllowed)
			return
		}
		authenticate(c, s.cfg.JWTSecret, s.cfg.TokenBinding)
		if c.IsAborted() {
			return
		}
		claims, ok := ClaimsFrom(c)
		if !ok {
			abort(c, http.StatusUnauthorized, "missing_token")
			return
		}
		ctx := context.WithValue(c.Request.Context(), mcpClaimsCtxKey{}, claims)
		h.ServeHTTP(c.Writer, c.Request.WithContext(ctx))
	}
}

// registerMCP 挂载 MCP 面:server 实例构建一次共享(stateless 无会话态,
// 五工具的 schema 推断/注册只在启动期跑一次——评审 LOW:每请求重建 =
// 每请求 5 次反射推断);Any 挂载使非 POST 也抵达本入口统一 405(gin 缺
// 路由 = 404,规范语义是 405)。独立于 /api/v1 路由组——MCP 是平级协议面,
// 非 REST 资源族;traceSpan 链同组(触发工具落 trace_id 与 REST 同构)。
func (s *Server) registerMCP(r *gin.Engine) {
	srv := s.buildMCPServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	r.Any("/mcp", s.traceSpan(), s.mcpEntry(h))
}
