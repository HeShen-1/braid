package api

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
	"braid/internal/tools"
)

// Server 组装 definition CRUD 的 REST 面。生产模式由部署层经 GIN_MODE
// 控制;不在此强制设值,避免吞掉部署层选择。
type Server struct {
	cfg Config
	st  *Store
	dbh *sql.DB // 写路径两步事务(CreateDraft/UpdateDraft/Publish)的包裹边界
	// toolReg 发布白名单真源(T3.2 换源:T5.4 的 BRAID_TOOL_ALLOWLIST 静态
	// env 退役,白名单 = registry 实际注册名;空 registry = 禁用一切工具,
	// 语义与空名单一致)。api 与 worker 各自装配同一注册集(T3.5 起内置工具)。
	toolReg *tools.Registry
}

// NewServer 构造 definition CRUD 服务(dbh 为连接池;ids 生成草稿行 id;
// toolReg 发布白名单来源,发布点经 dsl.WithToolAllowlist(registry 名单)。
// nil registry 响亮拒绝:静默空名单会把「配置缺漏」伪装成「禁用工具」。
func NewServer(cfg Config, dbh *sql.DB, ids IDGen, toolReg *tools.Registry) *Server {
	if toolReg == nil {
		panic("api: nil tools.Registry (publish allowlist source)")
	}
	return &Server{cfg: cfg, st: NewStore(ids), dbh: dbh, toolReg: toolReg}
}

// Router 注册全部路由。角色面:写(author|admin)与读(任意有效 token)
// 在路由声明,中间件形态见 middleware.go(T5.2 换装点)。草稿走独立
// /api/v1/drafts 前缀:gin 路由树不允许同层 static 与 param 混挂
// (/definitions/drafts/:id 与 /definitions/:name 冲突)。
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	// 不信任任何代理头:当前路由面不用 ClientIP,先解除 X-Forwarded-For
	// 伪造面(fail-closed),T5.2 若需要真实客户端 IP 再显式开名单。
	_ = r.SetTrustedProxies(nil)
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	authed := r.Group("/api/v1", Authenticate(s.cfg.JWTSecret))
	v1 := authed
	v1.POST("/definitions", RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.createDefinition)
	v1.GET("/definitions", s.listDefinitions)
	v1.GET("/definitions/:name", s.getLatestPublished)
	v1.GET("/definitions/:name/versions", s.listVersions)
	v1.GET("/definitions/:name/versions/:version", s.getVersion)
	v1.POST("/definitions/:name/versions/:version/archive",
		RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.archiveVersion)

	v1.GET("/drafts/:id", s.getDraft)
	v1.PUT("/drafts/:id", RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.updateDraft)
	v1.POST("/drafts/:id/publish", RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.publishDraft)
	return r
}
