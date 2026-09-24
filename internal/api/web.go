// Web 审批页静态面(T4.4):内嵌静态资产(go:embed,单二进制零构建链
// 零新依赖——D-2026-09-24-t44-approval-web 拍板)挂 /ui/。安全姿态:
//   - CSP default-src 'none' + 无 unsafe-inline/unsafe-eval:脚本/样式
//     只走同源文件,内联即静默失效(app_test 锚禁内联形态);
//   - 服务端数据(instructions/comment 是他人自由文本)一律前端
//     textContent 渲染,innerHTML 禁用(app.js 契约,web_test 锚);
//   - token 载体 = Bearer + sessionStorage:非 cookie 即无 CSRF 自动
//     携带面;nosniff / no-referrer / no-cache 同套。
package api

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed web/index.html web/app.js web/style.css
var webFS embed.FS

// webCSP 是静态面内容安全策略(词面契约由 web_test 锚死)。
const webCSP = `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`

// registerWeb 挂静态路由:/ 与 /ui 收编到 /ui/;/ui/*filepath 走内嵌 FS
// (fs.Sub 把 embed 的 web/ 前缀剥掉,FileServer 根即资产根)。
func (s *Server) registerWeb(r *gin.Engine) {
	redirect := func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/ui/")
	}
	r.GET("/", redirect)
	r.GET("/ui", redirect)
	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		// embed 指令与目录同 PR 旅行,编译期已钉死;到达即接线 bug。
		panic("api: embedded web assets: " + err.Error())
	}
	r.GET("/ui/*filepath", gin.WrapH(webSecurityHeaders(
		http.StripPrefix("/ui", http.FileServer(http.FS(assets))))))
}

// webSecurityHeaders 给静态响应统一叠安全头(X-Frame-Options 是 CSP
// frame-ancestors 的旧浏览器兜底;两者同设,锚在 web_test)。
func webSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", webCSP)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, req)
	})
}
