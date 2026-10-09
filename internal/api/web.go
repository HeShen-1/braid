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
// D-2026-10-09-ui-console 决策 5:唯一改动 = 新增 font-src 'self'
// (自托管 Geist/Geist Mono woff2 入库;default-src 'none' 下
// @font-face 必被拒载)。这是同源白名单新增,非放宽到 unsafe-*;
// 其余指令逐字不动(web_test 全串逐字锚)。
const webCSP = `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`

// registerWeb 挂静态路由:/ 与 /ui 收编到 /ui/;/ui/*filepath 走内嵌 FS
// (fs.Sub 把 embed 的 web/ 前缀剥掉,FileServer 根即资产根)。
// T14.1 画布(D-2026-09-30-t141):embed 面 web/canvas/ 是 vite 构建产物
// (dist 入库;源码与工具链在仓根 web/canvas/,构建直出本目录),同路由
// 零新端点:/ui/canvas/ → web/canvas/index.html。
// T21.1(D-2026-10-09-ui-console 决策 6):画布换 react-router,客户端
// 深链(/ui/canvas/executions 等)在 embed 面无对应文件,由 spaFallback
// 回退入口 HTML;basename 本 PR 仍是 /ui/canvas/,T21.2 才硬切根。
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
		http.StripPrefix("/ui", spaFallback(assets, noDirListing(assets))))))
}

// spaIndex 是画布 SPA 入口,也是非资源路径的回退目标(embed 面契约,
// 构造期读入——缺文件即接线 bug,与 fs.Sub 同款 fail-fast)。
const spaIndex = "canvas/index.html"

// spaFallback 给 /ui/canvas/* 的非资源路径回退到 SPA 入口:react-router
// 接管 /workflows、/executions 等客户端路由后,直达与刷新不能 404
// (D-2026-10-09-ui-console 决策 6)。判定边界(web_test 逐条锚):
//   - 目标在 embed 面存在 → 既有语义原样:文件即文件;目录无 index.html
//     由 noDirListing 翻 404(canvas/assets/ 禁目录列表契约不破);
//   - 目标不存在且末段不含 '.'(非资源文件)→ 入口 HTML 200;
//   - 末段含 '.'(nope.js / assets/x.css)→ 404 保持(资源缺失必须响亮,
//     回退成 HTML 会让 module 加载失败变成 MIME 报错,掩盖真实 404);
//   - /ui/ 下非 canvas 面(/ui/nope)→ 不回退(审批台 T21.2 前仍是旧三件套)。
//
// 回退直接写入口字节而不经 FileServer:FileServer 对 /index.html 结尾的
// 路径做 301 → ./ 归一化(serveFile 的 index.html 特例),深链会被跳成
// /ui/canvas/,而 SPA 客户端路由靠 200 启动——回退必须是 200 而非跳转
// (web_test 锚 Location 为空)。
func spaFallback(assets fs.FS, next http.Handler) http.Handler {
	index, err := fs.ReadFile(assets, spaIndex)
	if err != nil {
		// embed 指令与目录同 PR 旅行,编译期已钉死;到达即接线 bug。
		panic("api: embedded canvas index: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if spaRoute(assets, req.URL.Path) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// spaRoute 判定是否回退(口径见 spaFallback 注释)。Stat 只做存在性判定:
// 不存在(含非法路径、'..' 等 embed FS 拒绝形态)一律走回退分支,且回退
// 只吐固定的入口字节,不按请求路径开文件——无穿越面。
func spaRoute(assets fs.FS, urlPath string) bool {
	if !strings.HasPrefix(urlPath, "/canvas/") {
		return false
	}
	p := strings.Trim(urlPath, "/")
	if p == "" {
		return false
	}
	if _, err := fs.Stat(assets, p); err == nil {
		return false
	}
	return !strings.Contains(path.Base(p), ".")
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
