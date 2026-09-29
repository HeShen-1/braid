// 审计查询面(T6.3):GET /api/v1/audit —— 三维度过滤(execution/trace/人)
// + event_at 时间范围 + keyset 分页(T12.2,D-t63 #5 收敛路径:游标 =
// 展示序 (event_at,id) 排他下界,响应携 next_cursor,offset/total 退役)。
// 可见性 = 仅读 audit_log(高频四类 ≤ AuditFlushInterval 滞后);actor 维度
// admin-only(参数条件闸);三维度互斥恰一,无全量翻页面。
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
	PrevHash         string          `json:"prev_hash"` // T9.3:'' = 链首/存量未启链行
	RowHash          *string         `json:"row_hash"`  // T9.3:nil = 存量未启链行(D-t63 #4 兑现)
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
		PrevHash:         e.PrevHash,
		RowHash:          e.RowHash,
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

// encodeAuditCursor 游标线格式:base64(RawURL,查询参数安全)"v1:<unixmill>:<id>"
// ——版本前缀留演化位(增维时不静默改义);unixmill 与 event_at 的
// DATETIME(3) 毫秒精度对齐,epoch 毫秒是绝对值,不受 DSN loc 影响。
func encodeAuditCursor(cur AuditCursor) string {
	raw := fmt.Sprintf("v1:%d:%d", cur.EventAt.UnixMilli(), cur.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// parseAuditCursor 游标解析:base64 / 版本 / 两数值逐项校验,任一失配
// 400 bad_cursor 面(坏游标是明确的坏请求,不是空结果)。id ≥ 1、mill ≥ 0
// (1970 侧游标合法——语义只是「全序之下的所有行」)。
func parseAuditCursor(raw string) (*AuditCursor, bool) {
	dec, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(string(dec), ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, false
	}
	mill, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || mill < 0 {
		return nil, false
	}
	id, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || id == 0 {
		return nil, false
	}
	return &AuditCursor{EventAt: time.UnixMilli(mill).UTC(), ID: id}, true
}

// parseAuditPageParams 解析审计分页参数:limit 与 parsePageParams 同域
// 单源(缺省 20/上界 100);cursor 可选(缺省 = 首页);offset 已随 keyset
// 契约退役——携带即 400 bad_offset(契约收口:静默忽略会让旧客户端在
// 不知情下拿到恒首页)。
func parseAuditPageParams(c *gin.Context) (limit int, cursor *AuditCursor, ok bool) {
	limit = defaultPageLimit
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageLimit {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_limit"})
			return 0, nil, false
		}
		limit = n
	}
	if raw := c.Query("offset"); raw != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_offset"})
		return 0, nil, false
	}
	if raw := c.Query("cursor"); raw != "" {
		cur, ok := parseAuditCursor(raw)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_cursor"})
			return 0, nil, false
		}
		cursor = cur
	}
	return limit, cursor, true
}

// listAuditEvents:GET /audit —— 维度互斥校验(400)→ actor 维度 admin 闸
// (403)→ 执行维度钉分区(404)→ 查询 → items + next_cursor(keyset 契约,
// 与 definitions/executions 的 page envelope 有意分叉)。
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
	limit, cursor, ok := parseAuditPageParams(c)
	if !ok {
		return
	}
	if aq.ExecutionID != 0 {
		// 钉分区:execution 行直读 exec_date(DATE_FORMAT 单源公式),
		// 单分区裁剪 + idx_audit_exec;缺行 404(与 GET /executions/:id
		// 同面)。exec_date 不可变,钉分区读与审计查询分语句无撕裂面
		// (keyset 契约下无 total,COUNT 一致快照诉求已退役)。
		exec, err := s.st.GetExecution(c.Request.Context(), s.dbh, aq.ExecutionID)
		if err != nil {
			mapStoreError(c, err)
			return
		}
		aq.ExecDate = exec.ExecDate
	}
	// 语句级超时保留(无分区谓词的 trace/actor 维度是最重每请求查询
	// 形态,长查询持 audit_log 共享 MDL 会与 janitor 分区 DDL 互锁,
	// 见 auditQueryTimeout 注释);读路径无事务(keyset 单语句,镜像
	// execstore 先例)。
	qCtx, cancel := context.WithTimeout(c.Request.Context(), auditQueryTimeout)
	defer cancel()
	items, next, err := s.st.ListAuditEvents(qCtx, s.dbh, aq, cursor, limit)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	out := make([]auditEventDTO, 0, len(items))
	for _, e := range items {
		out = append(out, toAuditEventDTO(e))
	}
	nextCursor := ""
	if next != nil {
		nextCursor = encodeAuditCursor(*next)
	}
	c.JSON(http.StatusOK, gin.H{
		"items":       out,
		"next_cursor": nextCursor,
	})
}
