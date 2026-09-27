// 审计查询面(T6.3):GET /api/v1/audit —— 三维度过滤(execution/trace/人)
// + event_at 时间范围 + 分页。契约与拍板见 D-2026-09-25-t63-audit-query-api:
// 可见性 = 仅读 audit_log(高频四类 ≤ AuditFlushInterval 滞后);actor 维度
// admin-only(参数条件闸);三维度互斥恰一,无全量翻页面。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
)

// traceIDQueryRe 镜像 audit_log.trace_id 域(CHAR(32) hex 小写;schema 单源,
// 此处只做查询面形态预检——audit 包写入侧同式校验,坏形态 400 区别于空结果)。
var traceIDQueryRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// maxActorQueryLen 镜像 DDL actor_id VARCHAR(128)(audit 包 maxActorID 同源;
// 超宽是明确的坏请求,不是「查无此人」)。
const maxActorQueryLen = 128

// auditQueryTimeout 是审计查询的语句级超时上界(对抗审计 concurrency
// MEDIUM):无分区谓词的 trace/actor 维度是全系统最重的每请求查询形态,
// 长查询持 audit_log 共享 MDL 会与 janitor 分区 DDL 互锁——排他 MDL 等待
// 期间阻塞全部 audit_log 写入(触发/审批热路径 + flush)。上界把最坏持锁
// 窗口钉死(janitor lock_wait 5s 先行弃轮,下一小时 tick 重试);超时走
// 500 面与 slog 观测。连接池上限:三值已 env 化(BRAID_DB_MAX_OPEN /
// MAX_IDLE / CONN_MAX_LIFETIME,T7.1 收口 D-t63 移交),施加点
// db.ConfigurePool;饱和观测 braid_db_pool_* 指标面。
const auditQueryTimeout = 15 * time.Second

// auditEventDTO 是审计行视图:payload 原样直嵌(已过脱敏钩子,pii_masked
// 声明掩蔽事实);usage/cost 列组由 llm.call.completed 与 llm.call.discarded
// 双词携带(NULL=无用量语义)。ID 字符串化契约同 executionDTO(snowflake 超 2^53,IEEE754 消费方
// 静默折叠)。created_at 暴露落库滞后(异步面观测,D-t61 #7)。
type auditEventDTO struct {
	ID               string          `json:"id"`
	ExecDate         string          `json:"exec_date"`
	EventAt          time.Time       `json:"event_at"`
	CreatedAt        time.Time       `json:"created_at"`
	EventType        string          `json:"event_type"`
	ExecutionID      string          `json:"execution_id"`
	NodeExecutionID  *string         `json:"node_execution_id"`
	NodeID           *string         `json:"node_id"`
	AttemptID        *string         `json:"attempt_id"`
	ActorType        string          `json:"actor_type"`
	ActorID          string          `json:"actor_id"`
	TraceID          *string         `json:"trace_id"`
	PIIMasked        bool            `json:"pii_masked"`
	Model            *string         `json:"model"`
	PromptTokens     *uint32         `json:"prompt_tokens"`
	CompletionTokens *uint32         `json:"completion_tokens"`
	TotalTokens      *uint32         `json:"total_tokens"`
	CostAmount       *float64        `json:"cost_amount"`
	CostCurrency     *string         `json:"cost_currency"`
	CostEstimated    bool            `json:"cost_estimated"`
	Payload          json.RawMessage `json:"payload"`
}

func uint64PtrToString(p *uint64) *string {
	if p == nil {
		return nil
	}
	s := strconv.FormatUint(*p, 10)
	return &s
}

func toAuditEventDTO(e AuditEvent) auditEventDTO {
	return auditEventDTO{
		ID:               strconv.FormatUint(e.ID, 10),
		ExecDate:         e.ExecDate,
		EventAt:          e.EventAt,
		CreatedAt:        e.CreatedAt,
		EventType:        e.EventType,
		ExecutionID:      strconv.FormatUint(e.ExecutionID, 10),
		NodeExecutionID:  uint64PtrToString(e.NodeExecID),
		NodeID:           e.NodeID,
		AttemptID:        uint64PtrToString(e.AttemptID),
		ActorType:        e.ActorType,
		ActorID:          e.ActorID,
		TraceID:          e.TraceID,
		PIIMasked:        e.PIIMasked,
		Model:            e.Model,
		PromptTokens:     e.PromptTokens,
		CompletionTokens: e.CompletionTokens,
		TotalTokens:      e.TotalTokens,
		CostAmount:       e.CostAmount,
		CostCurrency:     e.CostCurrency,
		CostEstimated:    e.CostEstimated,
		Payload:          json.RawMessage(e.Payload),
	}
}

// parseAuditTimeRange 解析 from/to(RFC3339,event_at 闭区间):坏格式与
// from>to 同面 400 bad_time_range(不做逐参数分码——两者都是「区间不可
// 解释」,调用方无不同处置义)。
func parseAuditTimeRange(c *gin.Context) (from, to *time.Time, ok bool) {
	if raw := c.Query("from"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_time_range"})
			return nil, nil, false
		}
		from = &t
	}
	if raw := c.Query("to"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_time_range"})
			return nil, nil, false
		}
		to = &t
	}
	if from != nil && to != nil && from.After(*to) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_time_range"})
		return nil, nil, false
	}
	return from, to, true
}

// listAuditEvents:GET /audit —— 维度互斥校验(400)→ actor 维度 admin 闸
// (403)→ 执行维度钉分区(404)→ 查询 → items + page envelope(与
// definitions/executions 列表同构)。
func (s *Server) listAuditEvents(c *gin.Context) {
	execRaw := c.Query("execution_id")
	traceRaw := c.Query("trace_id")
	actorRaw := c.Query("actor_id")
	// 三维度互斥恰一(D-t63 #3):全空/组合 400。不设「无维度」全量翻页
	// 面——审计行自身携带 actor_id,全量翻页 = 旁路按人闸的监控面。
	dims := 0
	for _, v := range []string{execRaw, traceRaw, actorRaw} {
		if v != "" {
			dims++
		}
	}
	if dims != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_query"})
		return
	}
	aq := AuditQuery{}
	switch {
	case execRaw != "":
		id, err := strconv.ParseUint(execRaw, 10, 64)
		if err != nil || id == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_execution_id"})
			return
		}
		aq.ExecutionID = id
	case traceRaw != "":
		if !traceIDQueryRe.MatchString(traceRaw) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_trace_id"})
			return
		}
		aq.TraceID = traceRaw
	default:
		// 按人维度(D-t63 #2):跨执行的个人行为全景 = 监控/敏感面,收
		// admin;in-handler 参数条件闸(闸只绑 actor 维度,路由中间件做
		// 不到——形态镜像 T5.2 审批决定面)。读拒绝不落审计(路由级 403
		// 同惯例;issue #33「越权落审计」只钉审批决定写面)。
		if len(actorRaw) > maxActorQueryLen {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_actor_id"})
			return
		}
		claims, ok := ClaimsFrom(c)
		if !ok {
			// 路由挂 Authenticate,无 claims 是接线 bug;fail-closed 401
			// (RequireAnyRole 同判)。
			abort(c, http.StatusUnauthorized, "missing_token")
			return
		}
		if !claimsHaveAnyRole(claims, auth.RoleAdmin) {
			abort(c, http.StatusForbidden, "role_denied")
			return
		}
		aq.ActorID = actorRaw
	}
	from, to, ok := parseAuditTimeRange(c)
	if !ok {
		return
	}
	aq.From, aq.To = from, to
	limit, offset, ok := parsePageParams(c)
	if !ok {
		return
	}
	if aq.ExecutionID != 0 {
		// 钉分区:execution 行直读 exec_date(DATE_FORMAT 单源公式),
		// 单分区裁剪 + idx_audit_exec;缺行 404(与 GET /executions/:id
		// 同面)。
		exec, err := s.st.GetExecution(c.Request.Context(), s.dbh, aq.ExecutionID)
		if err != nil {
			mapStoreError(c, err)
			return
		}
		aq.ExecDate = exec.ExecDate
	}
	// 一致快照(对抗审计 sql/concurrency MEDIUM×2):COUNT 与页行同事务
	// (RR 快照在首语句建立)——消掉同响应内 total/items 撕裂(1s flush
	// 节奏下 total 低报会让客户端早停漏行)。与 execstore「读路径无事务」
	// 先例的有意分叉:审计是证据链,撕裂后果重于人触发级写入面。跨页
	// OFFSET 漂移仍在(登记 D-t63 已知边界,收敛路径 = keyset 分页)。
	qCtx, cancel := context.WithTimeout(c.Request.Context(), auditQueryTimeout)
	defer cancel()
	var (
		items []AuditEvent
		total int
	)
	if err := WithTx(qCtx, s.dbh, func(tx DBTX) error {
		var err error
		items, total, err = s.st.ListAuditEvents(qCtx, tx, aq, limit, offset)
		return err
	}); err != nil {
		mapStoreError(c, err)
		return
	}
	out := make([]auditEventDTO, 0, len(items))
	for _, e := range items {
		out = append(out, toAuditEventDTO(e))
	}
	c.JSON(http.StatusOK, gin.H{
		"items": out,
		"page":  gin.H{"limit": limit, "offset": offset, "total": total},
	})
}
