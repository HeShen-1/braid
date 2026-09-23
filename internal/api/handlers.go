// definition CRUD handlers:路由进(store 原语)+ DTO 出(错误映射
// 见 mapStoreError)。写路径统一经 WithTx 包裹(两步写的原子性);
// DSL 请求体按原样字节透传 store(校验与 hash 契约在 store 一处钉死)。
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// maxDSLBody 是 DSL 请求体上限:DSL 文档的合理上界(description ≤2000、
// 节点数十级);超限即拒绝,不做流式解析(写面窄,无放大价值)。
const maxDSLBody = 1 << 20 // 1 MiB

// 分页参数域(值配置化归 T7.1;先以常量钉死契约面)。
const (
	defaultPageLimit = 20
	maxPageLimit     = 100
	maxPageOffset    = 10000 // 深翻页防护:O(offset) 行跳过扫描的请求面上界
)

// definitionDTO 是列表/详情共用行视图(无 dsl 正文;detail 才带)。
// ID 序列化为字符串:snowflake id ≈ 9.6e16 已超 float64 精确整数界
// 2^53,JSON 数字形态会被一切 IEEE754 消费方(JS/Python json)静默
// 截断成相邻 id——回显即寻错行。字符串是雪碧 ID 的业界标准线形态。
type definitionDTO struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Version     *uint32    `json:"version"`
	Status      string     `json:"status"`
	Description string     `json:"description"`
	DSLHash     string     `json:"dsl_hash"`
	CreatedBy   string     `json:"created_by"`
	UpdatedBy   string     `json:"updated_by"`
	PublishedBy *string    `json:"published_by"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// definitionDetailDTO 是详情视图:dsl 为存储形态原文(合法 JSON 直嵌)。
type definitionDetailDTO struct {
	definitionDTO
	DSL json.RawMessage `json:"dsl"`
}

func toDTO(d Definition) definitionDTO {
	return definitionDTO{
		ID: strconv.FormatUint(d.ID, 10), Name: d.Name, Version: d.Version, Status: d.Status,
		Description: d.Description, DSLHash: d.DSLHash,
		CreatedBy: d.CreatedBy, UpdatedBy: d.UpdatedBy,
		PublishedBy: d.PublishedBy, PublishedAt: d.PublishedAt,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

func toDetail(d Definition) definitionDetailDTO {
	return definitionDetailDTO{definitionDTO: toDTO(d), DSL: json.RawMessage(d.DSL)}
}

type issueDTO struct {
	Code   string `json:"code"`
	Node   string `json:"node,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Detail string `json:"detail"`
}

// mapStoreError 把 store 错误面映射到 HTTP 契约:
// InvalidDSL→400(带聚合 issues)、NameMismatch→400、NotFound→404、
// Conflict→409(并发闸显式失败)、其余→500(仅日志,不外泄细节)。
func mapStoreError(c *gin.Context, err error) {
	var ide *InvalidDSLError
	switch {
	case errors.As(err, &ide):
		issues := make([]issueDTO, 0, len(ide.Issues))
		for _, i := range ide.Issues {
			issues = append(issues, issueDTO{Code: i.Code, Node: i.Node, Ref: i.Ref, Detail: i.Detail})
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_dsl", "issues": issues})
	case errors.Is(err, ErrNameMismatch):
		c.JSON(http.StatusBadRequest, gin.H{"error": "name_mismatch"})
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
	case errors.Is(err, ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "conflict"})
	default:
		slog.Error("api: definition store failure", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

// readDSLBody 读原样 DSL 字节(不经 gin binding:store 需要字节本体;
// hash 与 JSON 列语义都锚在字节/存储形态上)。仅真超限(*http.
// MaxBytesError)413;其余读体失败(断连/坏 chunked)是传输层错误,
// 谎报 413 会误导客户端重试逻辑与超限告警——统一 400 bad_request。
func readDSLBody(c *gin.Context) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDSLBody)
	b, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body_too_large"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request"})
		}
		return nil, false
	}
	return b, true
}

// actorFrom 取认证主体(sub 直通 created_by/updated_by/published_by 值域)。
func actorFrom(c *gin.Context) string {
	claims, _ := ClaimsFrom(c)
	return claims.Subject
}

func (s *Server) createDefinition(c *gin.Context) {
	doc, ok := readDSLBody(c)
	if !ok {
		return
	}
	var def Definition
	err := WithTx(c.Request.Context(), s.dbh, func(tx DBTX) error {
		var err error
		def, err = s.st.CreateDraft(c.Request.Context(), tx, doc, actorFrom(c))
		return err
	})
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toDetail(def))
}

func (s *Server) updateDraft(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	doc, ok := readDSLBody(c)
	if !ok {
		return
	}
	var def Definition
	err := WithTx(c.Request.Context(), s.dbh, func(tx DBTX) error {
		var err error
		def, err = s.st.UpdateDraft(c.Request.Context(), tx, id, doc, actorFrom(c))
		return err
	})
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDetail(def))
}

func (s *Server) publishDraft(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	// 发布点收紧白名单(dsl.WithToolAllowlist 语义:名单来自 Config,
	// T3.2 registry 落地后换源;空名单 = 禁用一切工具)。
	var def Definition
	err := WithTx(c.Request.Context(), s.dbh, func(tx DBTX) error {
		var err error
		def, err = s.st.Publish(c.Request.Context(), tx, id, actorFrom(c), s.cfg.ToolAllowlist...)
		return err
	})
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDetail(def))
}

func (s *Server) archiveVersion(c *gin.Context) {
	version, ok := parseVersionParam(c)
	if !ok {
		return
	}
	def, err := s.st.Archive(c.Request.Context(), s.dbh, c.Param("name"), version, actorFrom(c))
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDetail(def))
}

func (s *Server) getLatestPublished(c *gin.Context) {
	def, err := s.st.LatestPublished(c.Request.Context(), s.dbh, c.Param("name"))
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDetail(def))
}

func (s *Server) getVersion(c *gin.Context) {
	version, ok := parseVersionParam(c)
	if !ok {
		return
	}
	def, err := s.st.GetVersion(c.Request.Context(), s.dbh, c.Param("name"), version)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDetail(def))
}

func (s *Server) listVersions(c *gin.Context) {
	items, err := s.st.ListVersions(c.Request.Context(), s.dbh, c.Param("name"))
	if err != nil {
		mapStoreError(c, err)
		return
	}
	out := make([]definitionDTO, 0, len(items))
	for _, d := range items {
		out = append(out, toDTO(d))
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (s *Server) getDraft(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	def, err := s.st.GetByID(c.Request.Context(), s.dbh, id)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDetail(def))
}

func (s *Server) listDefinitions(c *gin.Context) {
	status := c.Query("status")
	switch status {
	case "", statusDraft, statusPublished, statusArchived:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_status"})
		return
	}
	limit := defaultPageLimit
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageLimit {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_limit"})
			return
		}
		limit = n
	}
	offset := 0
	if raw := c.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxPageOffset {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_offset"})
			return
		}
		offset = n
	}
	items, total, err := s.st.List(c.Request.Context(), s.dbh, status, limit, offset)
	if err != nil {
		mapStoreError(c, err)
		return
	}
	out := make([]definitionDTO, 0, len(items))
	for _, d := range items {
		out = append(out, toDTO(d))
	}
	c.JSON(http.StatusOK, gin.H{
		"items": out,
		"page":  gin.H{"limit": limit, "offset": offset, "total": total},
	})
}

// parseIDParam 解析路径 :id(uint64);形态非法 400(区别于 404)。
func parseIDParam(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_id"})
		return 0, false
	}
	return id, true
}

// parseVersionParam 解析路径 :version(uint32);形态非法 400。
func parseVersionParam(c *gin.Context) (uint32, bool) {
	v, err := strconv.ParseUint(c.Param("version"), 10, 32)
	if err != nil || v == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_version"})
		return 0, false
	}
	return uint32(v), true
}
