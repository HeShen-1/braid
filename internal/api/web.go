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
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed web/index.html web/app.js web/style.css web/canvas
var webFS embed.FS

// webCSP 是静态面内容安全策略(词面契约由 web_test 锚死)。
const webCSP = `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`

// registerWeb 挂静态路由:/ 与 /ui 收编到 /ui/;/ui/*filepath 走内嵌 FS
// (fs.Sub 把 embed 的 web/ 前缀剥掉,FileServer 根即资产根)。
// T14.1 画布(D-2026-09-30-t141):embed 面 web/canvas/ 是 vite 构建产物
// (dist 入库;源码与工具链在仓根 web/canvas/,构建直出本目录),同路由
// 零新端点:/ui/canvas/ → web/canvas/index.html。
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
		http.StripPrefix("/ui", noDirListing(assets)))))
}

// noDirListing:目录请求且该目录无 index.html 时 404(fail-closed)。
// http.FileServer 对无 index 的目录渲染自动列表——canvas/assets/ 是
// embed 面第一个子目录资产面(T14.1),不为哈希产物开 browsing 面;
// 未来构建产物加目录(sourcemap 等)无需逐个登记即默认拒绝。
func noDirListing(assets fs.FS) http.Handler {
	file := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Stat 前剥首尾 /(目录请求带尾斜杠,embed FS 视为非法路径)。
		p := strings.Trim(req.URL.Path, "/")
		if st, err := fs.Stat(assets, p); err == nil && st.IsDir() {
			if idx, ierr := fs.Stat(assets, path.Join(p, "index.html")); ierr != nil || idx.IsDir() {
				http.NotFound(w, req)
				return
			}
		}
		file.ServeHTTP(w, req)
	})
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
