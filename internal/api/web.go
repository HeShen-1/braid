// Web 静态面(T21.2 路由硬切统一 SPA):内嵌 dist 资产(go:embed,单二进制
// 零构建链——D-2026-09-24-t44 拍板)直接挂根路径面;旧审批台 `/ui` 前缀与
// 其原生三件套(index.html/app.js/style.css)随本任务删除,该前缀不再注册
// 任何路由 → gin 默认 404(硬切,零重定向——D-2026-10-09-ui-console 决策 6)。
//
// 路由面(web_test 逐条锚):
//   - /assets/*filepath → 内嵌 dist 的哈希资产(JS/CSS/woff2),目录请求
//     由 noDirListing 翻 404(禁目录列表契约不破);
//   - /、/login、/workflows*、/executions*、/approvals* → SPA 入口 HTML
//     200(react-router 客户端面,含 :draftId/:executionId/:nodeId 深链;
//     直达与刷新必须 200 启动)。
//
// 安全姿态(D-t44 原样继承,D-ui-console 决策 1,零放宽):
//   - CSP default-src 'none' + 无 unsafe-inline/unsafe-eval:脚本/样式
//     只走同源文件,内联即静默失效(web_test 锚禁内联形态);
//   - 服务端数据(instructions/comment 是他人自由文本)一律 React 文本
//     节点渲染,innerHTML 家族禁用(旧 app.js 的 textContent 契约随
//     T21.2 移入前端源码,vitest 承载);
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

// webFS 是 dist 入库面:vite 构建直出本目录(internal/api/web/),源码与
// 工具链在仓根 web/canvas/(D-2026-09-30-t141)。构建产物与 embed 指令
// 同 PR 旅行——go:embed 不接受空目录,`web` 面(index.html + assets/**)
// 在场是编译前提:重建后漏提交 dist 在此编译期响亮失败,不会静默发空页。
//
//go:embed web
var webFS embed.FS

// webCSP 是静态面内容安全策略(词面契约由 web_test 锚死)。
// D-2026-10-09-ui-console 决策 5:唯一改动 = 新增 font-src 'self'
// (自托管 Geist/Geist Mono woff2 入库;default-src 'none' 下
// @font-face 必被拒载)。这是同源白名单新增,非放宽到 unsafe-*;
// 其余指令逐字不动(web_test 全串逐字锚)。
const webCSP = `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`

// spaIndex 是 SPA 入口在 embed 面(web/)的相对路径;也是全部入口路径的
// 唯一响应体来源。
const spaIndex = "index.html"

// registerWeb 挂静态路由(全部叠 webSecurityHeaders):
//   - 资产面 /assets/*filepath:FileServer 根 = embed 面 web/(fs.Sub 剥掉
//     web/ 前缀),URL 路径与产物路径同形(index.html 里 vite base = "/");
//   - 入口面 /、/login 与三条客户端路由前缀(react-router:/workflows、
//     /executions、/approvals 各含深链)。
//
// 前缀同时注册精确路径与 *filepath:只挂 /workflows/*filepath 时,不带
// 尾斜杠的 /workflows 会被 gin RedirectTrailingSlash 做 301 → /workflows/,
// 而入口面契约是 200 且 Location 空(web_test 锚;SPA 深链同理——跳转即
// 丢路径)。
func (s *Server) registerWeb(r *gin.Engine) {
	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		// embed 指令与目录同 PR 旅行,编译期已钉死;到达即接线 bug。
		panic("api: embedded web assets: " + err.Error())
	}
	index, err := fs.ReadFile(assets, spaIndex)
	if err != nil {
		// 同上 fail-fast:入口 HTML 缺席 = 全部 SPA 路径无响应体。
		panic("api: embedded SPA index: " + err.Error())
	}
	entry := gin.WrapH(webSecurityHeaders(spaEntry(index)))
	r.GET("/assets/*filepath", gin.WrapH(webSecurityHeaders(noDirListing(assets))))
	r.GET("/", entry)
	r.GET("/login", entry)
	for _, prefix := range []string{"/workflows", "/executions", "/approvals"} {
		r.GET(prefix, entry)
		r.GET(prefix+"/*filepath", entry)
	}
}

// spaEntry 直接写构造期读入的入口字节,不经 FileServer:FileServer 对
// /index.html 结尾的路径做 301 → ./ 归一化(serveFile 的 index.html
// 特例),深链会被跳成目录路径,而 SPA 客户端路由靠 200 启动——入口面
// 必须是 200 而非跳转(web_test 锚 Location 为空 + 体等于入口字节)。
func spaEntry(index []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}

// noDirListing:目录请求且该目录无 index.html 时 404(fail-closed)。
// http.FileServer 对无 index 的目录渲染自动列表——assets/ 是 embed 面
// 唯一子目录资产面(哈希产物),不为浏览开面;未来构建产物加目录
// (sourcemap 等)无需逐个登记即默认拒绝。
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
