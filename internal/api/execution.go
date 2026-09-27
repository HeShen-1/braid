// execution 触发面(T5.5):POST /api/v1/executions —— 钉版解析
// (LatestPublished / GetVersion)→ scheduler.Instantiate 物化。api 不
// 直接持有 *scheduler.Scheduler,经 ExecutionTrigger 单方法缝(测试以
// fake 替身注入,api 保持零 Redis 消费包;真链集成测试见 server_test.go)。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/audit"
	"braid/internal/engine"
	"braid/internal/scheduler"
)

// triggerTypeAPI 是 workflow_execution.trigger_type 的 DDL ENUM「API」值
// (词表单源在 DDL;scheduler 侧空值默认同此)。T8 定时触发落地时在此
// 扩展第二个值。
const triggerTypeAPI = "API"

// definitionNameRe 镜像 DSL $.name 域(schema 是单源,此处只做触发面
// 形态预检):非域形态 400 区别于 404(与 bad_id/bad_version 哲学一致,
// 形态非法 ≠ 不存在)。
var definitionNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

// IdempotencyKeyHeader 是触发幂等键的 HTTP 头(D-t55 §5 定名,Stripe
// 惯例)。可选项:缺省 → 不去重(DDL uk_exec_idem 允许多 NULL,现状);
// 提供 → 作用域 = 主体+定义二维(防全局键跨主体抢注,DDL :75 注释),
// 落库值 = 作用域复合的 SHA-256(64 hex,适配 VARCHAR(128);长度前缀
// 帧防分隔符碰撞)。去重窗口 = 永久(uk 语义);重放响应 200 + 原执行
// 回显(非 201:无新资源创建)。
const (
	IdempotencyKeyHeader = "Idempotency-Key"
	// maxIdempotencyKeyLen 是原始键上限(DDL 列宽 128;哈希后 64 恒适配,
	// 原始值限宽保证客户端可读对账面不至于无界)。
	maxIdempotencyKeyLen = 128
	// idempotencyScopeV1 是作用域哈希的域分隔版本前缀(升版 = 全量键
	// 重新生效,显式不兼容换装点)。
	idempotencyScopeV1 = "braid.exec.idem.v1"
)

// scopedIdempotencyKey 计算落库值:SHA-256 over 长度前缀帧的
// (版本, 主体, 定义名, 原始键)——定长 64 hex,主体/定义/键任意字节
// (含分隔符形态)无碰撞歧义。
func scopedIdempotencyKey(subject, definitionName, key string) string {
	h := sha256.New()
	for _, part := range []string{idempotencyScopeV1, subject, definitionName, key} {
		_ = binary.Write(h, binary.BigEndian, uint64(len(part)))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ExecutionTrigger 是触发缝:*scheduler.Scheduler 结构性满足(方法集
// 子集)。Instantiate 仅触 MySQL(单事务物化执行 + 全节点行 + outbox),
// 推进链(outbox→relay→asynq→worker)不在 api 进程。
type ExecutionTrigger interface {
	Instantiate(ctx context.Context, p scheduler.InstantiateParams) (uint64, error)
}

// triggerRequest 是 POST /executions 请求体。version 缺省 = 该 name 最新
// PUBLISHED(LatestPublished 语义,全归档 ⇒ 404 禁新触发);显式 version
// 须 PUBLISHED(ARCHIVED ⇒ 409)。input 缺省/显式 null 同义(Instantiate
// 零输入→{} 契约)。
type triggerRequest struct {
	DefinitionName string          `json:"definition_name"`
	Version        *uint32         `json:"version"`
	Input          json.RawMessage `json:"input"`
}

// executionAckDTO 是触发响应:Instantiate 契约回显而非 DB 读回——缝的
// fake 替身无行可读,且 PENDING/API/钉版回显在契约面已知;完整行视图
// (节点状态与审计指针)走 GET /executions/:id。ID 字符串化契约同
// definitionDTO(snowflake ≈ 9.6e16 超 2^53,JSON 数值形态被 IEEE754
// 消费方静默折叠)。
type executionAckDTO struct {
	ID                string `json:"id"`
	DefinitionName    string `json:"definition_name"`
	DefinitionVersion uint32 `json:"definition_version"`
	Status            string `json:"status"`
	TriggerType       string `json:"trigger_type"`
	TriggeredBy       string `json:"triggered_by"`
}

// readTriggerBody 读触发请求体(readBodyLimited 单源分界:仅真超限 413,
// 传输层失败 400)+ JSON 解码(坏 JSON 400 bad_request)。
func readTriggerBody(c *gin.Context, dst *triggerRequest) bool {
	b, ok := readBodyLimited(c)
	if !ok {
		return false
	}
	if err := json.Unmarshal(b, dst); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		return false
	}
	return true
}

// isJSONObject 报告 input 形态是否可放行:缺省/显式 null = 空对象语义
// (Instantiate 契约),顶层对象放行,其余(数组/标量)拒绝。前置校验
// 镜像 sched.go Instantiate 的拒非对象({{inputs.*}} 解析面 = 顶层对象):
// 放行会在 Start 处确定性失败、经 asynq 重试后 archived、执行永卡
// PENDING——在触发面以 400 拒绝,不谎报 500。
func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var probe map[string]json.RawMessage
	return json.Unmarshal(raw, &probe) == nil
}

// resolveTriggerTarget 解析钉版目标:缺省 version = 最新 PUBLISHED;
// 显式 version 经 GetVersion 读行,非 PUBLISHED(即 ARCHIVED)是明确的
// 生命周期闸拒绝 ⇒ ErrConflict(409)。
func (s *Server) resolveTriggerTarget(c *gin.Context, req triggerRequest) (Definition, error) {
	if req.Version == nil {
		return s.st.LatestPublished(c.Request.Context(), s.dbh, req.DefinitionName)
	}
	def, err := s.st.GetVersion(c.Request.Context(), s.dbh, req.DefinitionName, *req.Version)
	if err != nil {
		return Definition{}, err
	}
	if def.Status != statusPublished {
		return Definition{}, fmt.Errorf("%w: %q v%d is %s, not PUBLISHED",
			ErrConflict, def.Name, *def.Version, def.Status)
	}
	return def, nil
}

// triggerExecution:body 形态校验(400 面)→ 钉版解析(404/409 面)→
// Instantiate 物化(404 归档竞态 / 500 面)→ 201 契约回显。
func (s *Server) triggerExecution(c *gin.Context) {
	var req triggerRequest
	if !readTriggerBody(c, &req) {
		return
	}
	if req.DefinitionName == "" || !definitionNameRe.MatchString(req.DefinitionName) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_definition_name"})
		return
	}
	if req.Version != nil && *req.Version == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_version"})
		return
	}
	if !isJSONObject(req.Input) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_input"})
		return
	}
	// 显式 null 归一为缺省语义:Instantiate 的零输入→{} 只认 len==0,
	// "null" 字面量会以 JSON null 落库,违反 DDL「零输入存 {}」契约
	//(评审 MEDIUM:探针三链实证)。
	if string(req.Input) == "null" {
		req.Input = nil
	}
	def, err := s.resolveTriggerTarget(c, req)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	// 幂等键(D-t55 §5,T4.2 兑现):作用域哈希后透传;撞键重放见下。
	idemKey := c.GetHeader(IdempotencyKeyHeader)
	if len(idemKey) > maxIdempotencyKeyLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_idempotency_key"})
		return
	}
	scoped := ""
	if idemKey != "" {
		scoped = scopedIdempotencyKey(actorFrom(c), def.Name, idemKey)
	}
	// TriggerType 显式 "API"(与审计 payload 同值单源);TraceID = root
	// trace(T6.2:traceSpan middleware 注入 request ctx,应用层强制落
	// workflow_execution.trace_id——DDL 注释口径;Tracing(nil) 测试路径
	// 无 span → 空串 → NULL,「内部测试路径允许 NULL」同注释)。
	var rootTrace string
	if sc := trace.SpanContextFromContext(c.Request.Context()); sc.HasTraceID() {
		rootTrace = sc.TraceID().String()
	}
	actor := actorFrom(c)
	execID, err := s.trig.Instantiate(c.Request.Context(), scheduler.InstantiateParams{
		DefinitionID:   def.ID,
		Input:          req.Input,
		TriggeredBy:    actor,
		TriggerType:    triggerTypeAPI,
		IdempotencyKey: scoped,
		TraceID:        rootTrace,
	})
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrNotFound):
			// 解析→物化之间版本被归档(TOCTOU 竞态窗):Instantiate 内部
			// PUBLISHED 复核兜底,对外与「无发布版本」同面 404。
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		case errors.Is(err, engine.ErrConflict) && scoped != "":
			// 幂等键撞(uk_exec_idem):读回原执行做重放响应。校验维度与
			// 作用域哈希同维(定义名,非版本行 id)——跨版本发布后的合法
			// 重放(v4 解析、v3 落库)按名匹配 200 回显首执行;异名命中
			// 才是作用域违规(SHA-256 碰撞或键空间错乱,不可达)→ 500
			// 响亮不静默错回显。
			prior, rerr := s.st.GetExecutionByIdempotencyKey(c.Request.Context(), s.dbh, scoped)
			if rerr != nil {
				mapStoreError(c, rerr)
				return
			}
			if prior.DefinitionName != def.Name {
				slog.Error("api: idempotency key scope violation",
					"execution_id", prior.ID, "key_definition", prior.DefinitionName, "request_definition", def.Name)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
				return
			}
			c.JSON(http.StatusOK, executionAckDTO{
				ID:                strconv.FormatUint(prior.ID, 10),
				DefinitionName:    prior.DefinitionName,
				DefinitionVersion: prior.DefinitionVersion,
				Status:            prior.Status,
				TriggerType:       prior.TriggerType,
				TriggeredBy:       prior.TriggeredBy,
			})
		default:
			slog.Error("api: instantiate execution", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		}
		return
	}
	// Version 解引用安全:GetVersion 按非空 version 等值匹配、LatestPublished
	// 限定 PUBLISHED;「PUBLISHED 行必有 version」由 Publish 写路径原子
	// 落 version+status 保证(uk_def_name_version 兜底),非 DDL CHECK。
	s.writeTriggerAudit(c, execID, def.ID, actor)
	c.JSON(http.StatusCreated, executionAckDTO{
		ID:                strconv.FormatUint(execID, 10),
		DefinitionName:    def.Name,
		DefinitionVersion: *def.Version,
		Status:            string(engine.WorkflowPending), // Instantiate 物化即 PENDING
		TriggerType:       triggerTypeAPI,
		TriggeredBy:       actor,
	})
}

// writeTriggerAudit 落 execution.triggered 审计行(ARCHITECTURE L83:审计
// 记录触发者身份)。二段事务:Instantiate 已独立提交(engine/scheduler 本
// 任务只读红线,审计行无法进物化事务),此处独立小事务;失败仅 slog 观测、
// 不改 201——500 会诱导客户端重试造成重复触发,身份事实源由执行行
// triggered_by 列(DDL 钦定)保底。api.DBTX 是 engine.DBTX 超集,事务
// 直喂 audit.Append 零适配。
func (s *Server) writeTriggerAudit(c *gin.Context, execID, defID uint64, actor string) {
	auditID, err := s.ids.Next()
	if err != nil {
		slog.Error("api: audit id gen for execution.triggered", "err", err, "execution_id", execID)
		return
	}
	raw, err := json.Marshal(audit.ExecutionTriggeredPayload{
		DefinitionID: defID, TriggerType: triggerTypeAPI,
	})
	if err != nil { // 纯 id/枚举序列化,失败 = 调用方 bug
		slog.Error("api: marshal execution.triggered payload", "err", err, "execution_id", execID)
		return
	}
	err = WithTx(c.Request.Context(), s.dbh, func(tx DBTX) error {
		return audit.Append(c.Request.Context(), tx, audit.Event{
			ID:          auditID,
			EventType:   audit.EventExecutionTriggered,
			ExecutionID: execID,
			ActorType:   audit.ActorUser,
			ActorID:     actor, // JWT sub(T5.1);事实源镜像 = triggered_by 列
			Payload:     raw,
		})
	})
	if err != nil {
		slog.Error("api: audit execution.triggered", "err", err, "execution_id", execID)
	}
}

// executionDTO 是列表行视图:无 input/outputs 正文(重组字段仅 detail
// 携带——definitionDTO 不带 dsl 同构;input 上限 1MiB×limit 100 的响应
// 放大面在列表侧结构性排除)。ID/DefinitionID 字符串化契约同
// definitionDTO(2^53 精度)。
type executionDTO struct {
	ID                string     `json:"id"`
	DefinitionID      string     `json:"definition_id"`
	DefinitionName    string     `json:"definition_name"`
	DefinitionVersion uint32     `json:"definition_version"`
	Status            string     `json:"status"`
	TriggerType       string     `json:"trigger_type"`
	TriggeredBy       string     `json:"triggered_by"`
	ErrorCode         *string    `json:"error_code"`
	ErrorMessage      *string    `json:"error_message"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
}

// nodeExecutionDTO:节点状态视图(uk_node_exec 字典序)。
type nodeExecutionDTO struct {
	ID           string     `json:"id"`
	NodeID       string     `json:"node_id"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	Attempts     uint16     `json:"attempts"`
	MaxAttempts  uint16     `json:"max_attempts"`
	ErrorCode    *string    `json:"error_code"`
	ErrorMessage *string    `json:"error_message"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

// executionAuditPointer 是审计行定位坐标(ROADMAP「审计指针」):exec_date
// 是 audit_log 分区裁剪键(writer 单源物化公式的查询侧镜像),配合行内
// execution_id 列即定位执行审计链;trace_id 是 T6.3 反查维度,T6.2 前
// 恒 null 不伪造(DDL 注释契约)。
type executionAuditPointer struct {
	ExecDate string  `json:"exec_date"`
	TraceID  *string `json:"trace_id"`
}

// executionDetailDTO:行视图 + input/outputs 正文 + 内嵌节点状态 + 审计
// 指针。节点数随 DSL 上限(数十级)有界,内嵌安全;P1 fan-out 行数膨胀
// 时再拆子端点。
type executionDetailDTO struct {
	executionDTO
	DefinitionHash string                `json:"definition_hash"` // 钉版四件套第四件,与 definition.dsl_hash 比对即知漂移(DDL :71)
	Input          json.RawMessage       `json:"input"`
	Outputs        json.RawMessage       `json:"outputs"` // nil = null(未终态)
	Nodes          []nodeExecutionDTO    `json:"nodes"`
	Audit          executionAuditPointer `json:"audit"`
}

func toExecutionDTO(e Execution) executionDTO {
	return executionDTO{
		ID: strconv.FormatUint(e.ID, 10), DefinitionID: strconv.FormatUint(e.DefinitionID, 10),
		DefinitionName: e.DefinitionName, DefinitionVersion: e.DefinitionVersion,
		Status: e.Status, TriggerType: e.TriggerType, TriggeredBy: e.TriggeredBy,
		ErrorCode: e.ErrorCode, ErrorMessage: e.ErrorMessage,
		CreatedAt: e.CreatedAt, StartedAt: e.StartedAt, FinishedAt: e.FinishedAt,
	}
}

func toExecutionDetail(e Execution, nodes []NodeExecution) executionDetailDTO {
	out := make([]nodeExecutionDTO, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, toNodeExecutionDTO(n))
	}
	return executionDetailDTO{
		executionDTO:   toExecutionDTO(e),
		DefinitionHash: e.DefinitionHash,
		Input:          json.RawMessage(e.Input),
		Outputs:        json.RawMessage(e.Outputs),
		Nodes:          out,
		Audit:          executionAuditPointer{ExecDate: e.ExecDate, TraceID: e.TraceID},
	}
}

func toNodeExecutionDTO(n NodeExecution) nodeExecutionDTO {
	return nodeExecutionDTO{
		ID: strconv.FormatUint(n.ID, 10), NodeID: n.NodeID, Type: n.Type, Status: n.Status,
		Attempts: n.Attempts, MaxAttempts: n.MaxAttempts,
		ErrorCode: n.ErrorCode, ErrorMessage: n.ErrorMessage,
		StartedAt: n.StartedAt, FinishedAt: n.FinishedAt,
	}
}

// listExecutions:GET /executions —— name(跨版本,按工作流列执行的 DDL
// 钦定维度)与 status 过滤 + 分页;顺序与 envelope 与 definitions 同构。
func (s *Server) listExecutions(c *gin.Context) {
	status := c.Query("status")
	switch status {
	case "", string(engine.WorkflowPending), string(engine.WorkflowRunning),
		string(engine.WorkflowWaitingApproval), string(engine.WorkflowSucceeded),
		string(engine.WorkflowFailed), string(engine.WorkflowCancelled):
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_status"})
		return
	}
	limit, offset, ok := parsePageParams(c)
	if !ok {
		return
	}
	items, total, err := s.st.ListExecutions(c.Request.Context(), s.dbh,
		c.Query("name"), status, limit, offset)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	out := make([]executionDTO, 0, len(items))
	for _, e := range items {
		out = append(out, toExecutionDTO(e))
	}
	c.JSON(http.StatusOK, gin.H{
		"items": out,
		"page":  gin.H{"limit": limit, "offset": offset, "total": total},
	})
}

// getExecution:GET /executions/:id —— 行视图 + 节点状态 + 审计指针。
// 两次读无事务包裹(钉版行集在物化时建立;读面轻微不一致 P0 可接受)。
func (s *Server) getExecution(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	exec, err := s.st.GetExecution(c.Request.Context(), s.dbh, id)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	nodes, err := s.st.ListNodeExecutions(c.Request.Context(), s.dbh, id)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, toExecutionDetail(exec, nodes))
}
