package api

import (
	"database/sql"
	"fmt"
	"reflect"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"braid/internal/auth"
	"braid/internal/metrics"
	"braid/internal/tools"
)

// Server 组装 definition CRUD 的 REST 面。生产模式由部署层经 GIN_MODE
// 控制;不在此强制设值,避免吞掉部署层选择。
type Server struct {
	cfg Config
	st  *Store
	ids IDGen   // 审计行 id 生成(audit.Event.ID 复用调用方雪花;与 st 同源)
	dbh *sql.DB // 写路径两步事务(CreateDraft/UpdateDraft/Publish)的包裹边界
	// toolReg 发布白名单真源(T3.2 换源:T5.4 的 BRAID_TOOL_ALLOWLIST 静态
	// env 退役,白名单 = registry 实际注册名;空 registry = 禁用一切工具,
	// 语义与空名单一致)。api 与 worker 各自装配同一注册集(T3.5 起内置工具)。
	toolReg *tools.Registry
	// trig 是 execution 触发缝(T5.5):*scheduler.Scheduler 结构性满足;
	// 单方法面让测试以 fake 替身注入(Instantiate 仅触 MySQL,api 保持
	// 零 Redis 消费包)。nil 响亮拒绝,理由同 toolReg。
	trig ExecutionTrigger
	// idp 是认证身份源缝(T5.3,ARCHITECTURE §3.6):登录面唯一消费者
	// (login.go)。生产默认 auth.BuiltinProvider(内置用户/角色 + bcrypt);
	// 替换实现注入即换源(凭证核验型插件 P1,如 LDAP bind;OIDC 属
	// redirect 流,不经本接口,须另立回调路由面——错误族与 User→
	// SignToken 契约可复用),契约见 internal/auth/identity.go。nil 响亮
	// 拒绝,理由同 toolReg。
	idp auth.IdentityProvider
	// loginThrottle 登录防御层(T6.4 落地,T11.3 DB 锚定换装:0010
	// login_throttle 表,多副本共享;构造于 NewServer,nil DB = 零栈
	// 桩面防御跳过)。
	loginThrottle *loginThrottle
	// tracer 是 OTel root span 面(T6.2;cmd/api 装配,测试默认 nil =
	// 不起 span,请求 ctx 无 trace id,audit 落 NULL——镜像 loginMetrics
	// 注入姿势)。span 边界 = authed 组每请求;trace id 是 audit 行与
	// outbox 行的传播载体(D-2026-09-27-t62-otel-trace)。
	tracer trace.Tracer
	// rollbackMetrics 是回滚面指标(T9.2/T6.4;nil = 观测关闭——测试默认,
	// cmd/api 装配注入;requested/applied/denied{reason} 三计数器,
	// D-2026-09-28-t92 决策 10)。
	rollbackMetrics *metrics.RollbackSet
	// loginMetrics 是登录指标面(T6.4;nil = 观测关闭——测试默认,
	// cmd/api 装配注入)。
	loginMetrics *metrics.LoginSet
	// fleetGateMetrics 是 mode=all 混跑窗硬闸拒绝计数(T15.6;nil = 观测
	// 关闭——测试默认,cmd/api 装配注入;拒绝痕迹主面 = 结构化 slog)。
	fleetGateMetrics *metrics.FleetGateSet
	// oidc 是 OIDC 插件面(T11.1;T16.1 起协议实现迁企业增值面
	// internal/enterprise/oidc):nil = 未配置,路由不挂载。注入经
	// OIDCProvider setter(镜像 LoginMetrics/Tracing 先例),NewServer
	// 签名不动——与并行任务的装配面冲突收敛到零。测试桩直赋字段
	// (OIDCProvider 缝,oidc.go),生产唯一实现 = 企业树 *oidc.OIDCProvider
	//(cmd 组合根装配;OSS 构建剔除企业树后登录面完好,路由零挂载)。
	oidc OIDCProvider
}

// NewServer 构造 definition CRUD + execution 触发服务(dbh 为连接池;
// ids 生成草稿行 id;toolReg 发布白名单来源,发布点经 dsl.WithToolAllowlist
// (registry 名单);trig 触发缝,生产装配传 *scheduler.Scheduler;idp
// 认证身份源缝,生产默认 auth.BuiltinProvider{DB: dbh})。
// nil registry / nil trig / nil idp(含 typed-nil)响亮拒绝:静默空缺会把
// 「配置缺漏」伪装成「禁用工具」/「触发面 500」/「认证面 panic」。
func NewServer(cfg Config, dbh *sql.DB, ids IDGen, toolReg *tools.Registry, trig ExecutionTrigger, idp auth.IdentityProvider) *Server {
	if toolReg == nil {
		panic("api: nil tools.Registry (publish allowlist source)")
	}
	if trig == nil {
		panic("api: nil ExecutionTrigger (execution trigger seam)")
	}
	if idp == nil {
		panic("api: nil IdentityProvider (authenticate seam)")
	}
	// typed-nil 探测(对抗审计 MEDIUM 收口):ProviderFunc(nil)/(*T)(nil)
	// 装箱后接口值非 nil,== 检查放行,panic 被推迟到首个登录请求且呈现为
	// 空 body 500(不可诊断);构造期响亮失败,语义与 nil 检查一致。
	if v := reflect.ValueOf(idp); (v.Kind() == reflect.Pointer || v.Kind() == reflect.Func) && v.IsNil() {
		panic("api: typed-nil IdentityProvider (authenticate seam)")
	}
	return &Server{
		cfg: cfg, st: NewStore(ids), ids: ids, dbh: dbh,
		toolReg: toolReg, trig: trig, idp: idp,
		loginThrottle: newLoginThrottle(cfg, dbh),
	}
}

// LoginMetrics 注入登录指标面(T6.4;cmd/api 装配,测试默认 nil)。
func (s *Server) LoginMetrics(m *metrics.LoginSet) { s.loginMetrics = m }

// RollbackMetrics 注入回滚面指标(T9.2;nil = 观测关闭,镜像 LoginMetrics)。
func (s *Server) RollbackMetrics(m *metrics.RollbackSet) { s.rollbackMetrics = m }

// FleetGateMetrics 注入 mode=all 硬闸拒绝计数(T15.6;镜像 LoginMetrics)。
func (s *Server) FleetGateMetrics(m *metrics.FleetGateSet) { s.fleetGateMetrics = m }

// Tracing 注入 OTel tracer 面(T6.2;cmd/api 装配,测试默认 nil)。
func (s *Server) Tracing(t trace.Tracer) { s.tracer = t }

// OIDCProvider 注入 OIDC 插件面(T11.1;T16.1 起实现 = 企业增值面
// internal/enterprise/oidc,cmd/api 在 BRAID_OIDC_* 组配置完整时装配,见
// D-2026-09-28-t111-oidc-subject-anchor)。nil 拒绝:静默缺配会把「忘了
// 注入」伪装成「未配置」(路由面 404),可诊断性丢失。typed-nil 同拒
// (接口化后接口值非 nil 但方法调用即 panic,NewServer idp 探测同款——
// 对抗审计 LOW 收口)。
func (s *Server) OIDCProvider(p OIDCProvider) {
	if p == nil {
		panic("api: nil OIDCProvider (oidc plugin seam)")
	}
	if v := reflect.ValueOf(p); (v.Kind() == reflect.Pointer || v.Kind() == reflect.Func) && v.IsNil() {
		panic("api: typed-nil OIDCProvider (oidc plugin seam)")
	}
	s.oidc = p
}

// traceSpan 给 authed 组每请求起 root span(T6.2):trace id 随 request
// ctx 注入,触发/决策两路生产点据此落 audit 行与 outbox 行(经 ctx 自动
// 提取);Tracing(nil) 时零侵入(测试路径 trace 恒 NULL,DDL 注释的
// 「内部测试路径允许 NULL」口径)。
func (s *Server) traceSpan() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.tracer == nil {
			c.Next()
			return
		}
		ctx, span := s.tracer.Start(c.Request.Context(), "api "+c.FullPath())
		c.Request = c.Request.WithContext(ctx)
		c.Next()
		span.End()
	}
}

// Router 注册全部路由。角色面:写(author|admin)与读(任意有效 token)
// 在路由声明,中间件形态见 middleware.go(T5.2 换装点)。scp 执法面
// (T17.1,D-2026-10-01-t171-rest-scp):authed 组每条路由挂 RequireScope
// 声明所需作用域(scp 在场才执法,缺席 = 全角色凭证现状兼容);端点×
// 作用域矩阵的对抗枚举锚 = scope_enforce_test.go 策略表(两处同 PR 旅行)。
// 草稿走独立 /api/v1/drafts 前缀:gin 路由树不允许同层 static 与 param
// 混挂(/definitions/drafts/:id 与 /definitions/:name 冲突)。
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	// 代理信任面(T6.4):TrustedProxies 显式 CIDR 列表(空 = 不信任任何
	// 代理,fail-closed)。ClientIP 是登录防御/审计的键位:反代公开面必须
	// 配置代理段,否则全体客户端折叠进代理单地址的同一限流窗口(rev2
	// HIGH);名单只接受 CIDR,拒绝裸 IP 段语法错误静默放行。
	if err := r.SetTrustedProxies(s.cfg.TrustedProxies); err != nil {
		// ConfigFromEnv 已逐项 ParseCIDR 预检,此处不可达;防御性响亮。
		panic(fmt.Sprintf("api: trusted proxies %v: %v", s.cfg.TrustedProxies, err))
	}
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// Web 审批页静态面(T4.4):内嵌资产 + 安全头,见 web.go。
	s.registerWeb(r)

	// MCP server 面(T13.1,D-t131):POST /mcp 单端点,协议版本钉死
	// 2026-07-28 stateless;鉴权/租户/触发判定核与 REST 同源。
	s.registerMCP(r)

	// 登录面(T4.4):authed 组外——凭据换 token(错误面/防枚举语义见
	// login.go)。限流/账号锁定/登录审计已随 T6.4 落地(loginDefenseGate +
	// login_audit 表,旋钮见 Config;DB 锚定升级归 P1 停车场)。
	r.POST("/api/v1/auth/login", s.login)

	// IM 审批短链(T14.3,D-t143 决策 4):authed 组外——凭据 = 签名 URL
	//(HMAC capability,验证面 imlink.go);密钥未配(BRAID_IM_LINK_SECRET
	// 空)= 面关闭(端点恒挂载,404 收口,路由表不随配置漂移)。GET 带
	// 落票副作用是 capability link 的既定形态(重放幂等 + 结果页兜底,
	// 已知边界登记见 D-t143 决策 4/6)。
	r.GET("/api/v1/im/approval", s.imApproval)

	authed := r.Group("/api/v1", s.traceSpan(), Authenticate(s.cfg.JWTSecret, s.cfg.TokenBinding))
	v1 := authed
	// 自窄化 scoped token 签发面(T13.1,D-t131 决策 4):任意有效 token
	// 请求自身默认作用域子集(只窄不宽)——外部 agent 窄 token 的运维面;
	// 角色零门槛(读角色也允许自窄,上限由其角色默认集钉死)。scp 执法
	// 豁免(T17.1):自窄化构造性单调收窄(上界 = 角色默认 ∩ 呈现 token
	// 自身 scp),挂任何作用域要求都会打断窄 token 的链式再窄化——
	// 授权面在此端点无增益可守。
	v1.POST("/auth/tokens", s.issueToken)
	v1.POST("/definitions", RequireScope(auth.ScopeWorkflowsWrite),
		RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.createDefinition)
	v1.GET("/definitions", RequireScope(auth.ScopeWorkflowsRead), s.listDefinitions)
	v1.GET("/definitions/:name", RequireScope(auth.ScopeWorkflowsRead), s.getLatestPublished)
	v1.GET("/definitions/:name/versions", RequireScope(auth.ScopeWorkflowsRead), s.listVersions)
	v1.GET("/definitions/:name/versions/:version", RequireScope(auth.ScopeWorkflowsRead), s.getVersion)
	v1.POST("/definitions/:name/versions/:version/archive", RequireScope(auth.ScopeWorkflowsWrite),
		RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.archiveVersion)

	v1.GET("/drafts/:id", RequireScope(auth.ScopeWorkflowsRead), s.getDraft)
	v1.PUT("/drafts/:id", RequireScope(auth.ScopeWorkflowsWrite),
		RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.updateDraft)
	v1.POST("/drafts/:id/publish", RequireScope(auth.ScopeWorkflowsWrite),
		RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.publishDraft)

	// execution 面(T5.5):触发是写操作(消耗 LLM 预算),闸同 definition
	// 写面;查询/列表读面任意有效 token。回滚/续跑(T9.2)同判
	// executions:write(与触发同级执行域写操作)。
	v1.POST("/executions", RequireScope(auth.ScopeExecutionsWrite),
		RequireAnyRole(auth.RoleAuthor, auth.RoleAdmin), s.triggerExecution)
	v1.GET("/executions", RequireScope(auth.ScopeExecutionsRead), s.listExecutions)
	v1.GET("/executions/:id", RequireScope(auth.ScopeExecutionsRead), s.getExecution)
	// 回滚/续跑(T9.2,D-2026-09-28-t92):author|admin in-handler 闸
	// (拒绝落审计 rollback.denied,中间件做不到)——形态镜像审批决定面。
	v1.POST("/executions/:id/rollback", RequireScope(auth.ScopeExecutionsWrite), s.rollbackExecution)
	v1.POST("/executions/:id/resume", RequireScope(auth.ScopeExecutionsWrite), s.resumeExecution)

	// 待办列表(T4.4):任意有效 token,主体过滤即读边界(名单真相源 =
	// 快照 approvers,谓词面见 approvaltodo.go)。审批态读面同判
	// executions:read(审批是执行子资源域)。
	v1.GET("/approvals/pending", RequireScope(auth.ScopeExecutionsRead), s.listPendingApprovals)

	// 审批面(T4.2 立,T5.2 换装后形态):POST 决定的门 = approver|admin
	// 角色闸(handler 内,403 role_denied + approval.decision_denied 审计
	// reason=role_denied)叠加 DSL approvers[](快照真相源,越权 403 +
	// 同事件 reason=membership;DSL 名单是执行时权威)。GET 详情收窄到
	// 名单内审批人(comment 自由文本唯一暴露面不再开放;快照缺失
	// fail-closed,状态读面在 GET /executions/:id)。scp:决定 =
	// approvals:write(approver 默认集携带——REST 现状兼容刚需),
	// 详情 = executions:read。
	v1.GET("/executions/:id/approvals/:node_id", RequireScope(auth.ScopeExecutionsRead), s.getApproval)
	v1.POST("/executions/:id/approvals/:node_id/decision",
		RequireScope(auth.ScopeApprovalsWrite), s.decideApproval)

	// 审计查询面(T6.3):三维度互斥恰一(execution_id/trace_id/actor_id);
	// 可见性 = 仅读 audit_log(高频四类 ≤ flush interval 滞后,D-t63 #1)。
	// actor 维度 in-handler admin 闸(参数条件闸,路由中间件做不到——
	// 形态镜像 T5.2 审批决定面);execution/trace 维度读面任意有效 token。
	// scp = executions:read(audit_log 事件域 = 执行域,读面与执行查询同级;
	// actor 维度 admin 闸 in-handler 保留,与 scp 正交)。
	v1.GET("/audit", RequireScope(auth.ScopeExecutionsRead), s.listAuditEvents)

	// OIDC redirect 流(T11.1):authed 组外(凭据换 token 的入口同
	// login),注入过 OIDCProvider 才挂载(未配置零行为)。挂载点收敛在
	// 路由尾段——与并行任务的路由面改动错开(issue #82 冲突申报)。
	if s.oidc != nil {
		r.GET("/api/v1/auth/oidc/login", s.oidcLoginStart)
		r.GET("/api/v1/auth/oidc/callback", s.oidcCallback)
	}
	return r
}
