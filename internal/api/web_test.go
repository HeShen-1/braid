package api

// T4.4 Web 资产服务契约测试:内嵌静态面(/ui/)+ 安全头。零栈依赖——
// Server 构造传 nil dbh(静态路由不触 DB;NewServer 要求非 nil 的
// registry/trig/idp,dbh 是运行期依赖;idp 用零值 BuiltinProvider——
// 本文件不触登录路由,零值 DB 即无害),CI 无 t13 栈也全量覆盖。
//
// 锚两件事:①安全头契约(CSP 无 unsafe-inline、nosniff、no-referrer);
// ②端点锚——页面脚本必须引用真实 API 面(login/pending/decision),
// 前端与后端契约漂移在此红。
//
// 变异自查:CSP 删任一指令(含 frame-ancestors/base-uri/form-action
// 三个不回退指令)→ 头断言红;X-Frame-Options 删 DENY → 头断言红;
// app.js 改用 innerHTML 渲染 → XSS-sink 锚红;端点改路径 → 锚断言红。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
)

// newWebTestRouter 零栈路由:静态面 + healthz 即可。
func newWebTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	// 构造序列与 login_idp_test.go 的 newIDPTestRouter 同构,直接复用
	// (idp 零值内置:本文件不触登录路由)。
	return newIDPTestRouter(t, auth.BuiltinProvider{})
}

func webGet(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWebAssetsServed(t *testing.T) {
	r := newWebTestRouter(t)

	t.Run("root_redirects", func(t *testing.T) {
		for _, p := range []string{"/", "/ui"} {
			w := webGet(t, r, p)
			if w.Code != http.StatusFound || w.Header().Get("Location") != "/ui/" {
				t.Fatalf("%s = %d %q, want 302 /ui/", p, w.Code, w.Header().Get("Location"))
			}
		}
	})

	t.Run("index_html_with_security_headers", func(t *testing.T) {
		w := webGet(t, r, "/ui/")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("content-type = %q", ct)
		}
		csp := w.Header().Get("Content-Security-Policy")
		// 全 8 指令逐词锚(对抗审计:base-uri/frame-ancestors/form-action
		// 是 CSP 规范不回退到 default-src 的三个指令,frame-ancestors 更是
		// 本页唯一点击劫持防线——删任何一个都必须红)。
		for _, want := range []string{
			"default-src 'none'", "script-src 'self'", "style-src 'self'",
			"connect-src 'self'", "img-src 'self'", "base-uri 'none'",
			"frame-ancestors 'none'", "form-action 'self'",
		} {
			if !strings.Contains(csp, want) {
				t.Fatalf("CSP 缺 %q: %q", want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("CSP 含 unsafe-*: %q", csp)
		}
		// 点击劫持旧浏览器兜底(frame-ancestors 不识别的客户端)。
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("X-Frame-Options 缺失(应 DENY)")
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("nosniff 缺失")
		}
		body := w.Body.String()
		// 内联脚本/内联样式禁入(CSP 无 unsafe-inline,内联即静默失效):
		// script 标签必须带 src,禁 style 属性与内联事件句柄。
		for _, banned := range []string{"onclick=", "onerror=", "onload=", "javascript:"} {
			if strings.Contains(strings.ToLower(body), banned) {
				t.Fatalf("index.html 含内联事件句柄/伪协议 %q(CSP 违约)", banned)
			}
		}
		if strings.Contains(body, "<script>") {
			t.Fatalf("index.html 含无 src 的 script 标签(CSP 违约)")
		}
		if strings.Contains(body, "style=") {
			t.Fatalf("index.html 含内联 style 属性(CSP 违约)")
		}
	})

	t.Run("app_js_endpoint_anchors", func(t *testing.T) {
		w := webGet(t, r, "/ui/app.js")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Fatalf("content-type = %q", ct)
		}
		js := w.Body.String()
		for _, anchor := range []string{
			"/api/v1/auth/login",
			"/api/v1/approvals/pending",
			"/decision",
		} {
			if !strings.Contains(js, anchor) {
				t.Fatalf("app.js 缺端点锚 %q(前后端契约漂移)", anchor)
			}
		}
		// XSS 纪律:服务端数据渲染禁等价 sink 家族(textContent-only 契约;
		// 锚属性访问/调用形态,注释里的词面不算——对抗审计:子串锚需覆盖
		// insertAdjacentHTML/document.write/srcdoc 等绕过形态)。
		for _, banned := range []string{".innerHTML", ".outerHTML", "insertAdjacentHTML", "document.write", "srcdoc"} {
			if strings.Contains(js, banned) {
				t.Fatalf("app.js 使用 %s(自由文本 XSS 向量,违 textContent 契约)", banned)
			}
		}
	})

	t.Run("style_css", func(t *testing.T) {
		w := webGet(t, r, "/ui/style.css")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
			t.Fatalf("content-type = %q", ct)
		}
	})

	t.Run("missing_asset_404", func(t *testing.T) {
		if w := webGet(t, r, "/ui/nope.js"); w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	// T14.1 画布(D-2026-09-30-t141):dist 入库 + 同静态路由零新端点。
	// 锚三件事:①/ui/canvas/ 页面可达且安全头与审批台同套(CSP 不因
	// 框架化放宽——React 内联样式走 CSSOM,不需求 unsafe-inline);
	// ②index.html 引用的 bundle/CSS 真在 embed 里(vite 产物名带 hash,
	// 从页面提取再回取,漂移门在 CI web job 的 git diff,此处锚可达性);
	// ③无内联事件句柄/无 src 的 script(vite 产物全外部文件契约)。
	t.Run("canvas_index_and_bundle", func(t *testing.T) {
		w := webGet(t, r, "/ui/canvas/")
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("content-type = %q", ct)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("canvas CSP 含 unsafe-*(预期零放宽,D-t141 #4): %q", csp)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("X-Frame-Options 缺失(应 DENY)")
		}
		page := w.Body.String()
		for _, banned := range []string{"onclick=", "onerror=", "onload=", "javascript:"} {
			if strings.Contains(strings.ToLower(page), banned) {
				t.Fatalf("canvas index.html 含内联事件句柄/伪协议 %q", banned)
			}
		}
		if strings.Contains(page, "<script>") {
			t.Fatalf("canvas index.html 含无 src 的 script 标签")
		}
		// bundle(src)与样式(href)双回取:页面引用什么,embed 就必须有什么。
		src := extractAttr(t, page, "script", "src")
		href := extractAttr(t, page, "link", "href")
		for _, ref := range []struct{ path, wantCT string }{
			{src, "javascript"},
			{href, "text/css"},
		} {
			aw := webGet(t, r, ref.path)
			if aw.Code != http.StatusOK {
				t.Fatalf("%s = %d", ref.path, aw.Code)
			}
			if ct := aw.Header().Get("Content-Type"); !strings.Contains(ct, ref.wantCT) {
				t.Fatalf("%s content-type = %q, want *%s*", ref.path, ct, ref.wantCT)
			}
		}
		// 端点锚(同 app.js 纪律):bundle 必须引用真实 API 面,前后端
		// 契约漂移在此红。XSS 纪律(禁 innerHTML 家族)锚在 vitest 源码
		// 扫描——React 运行时内部合法使用 innerHTML,bundle 级子串禁令
		// 会误伤框架本体;画布代码全走 React 文本节点转义。
		bw := webGet(t, r, src)
		bundle := bw.Body.String()
		for _, anchor := range []string{
			"/api/v1/auth/login",
			"/definitions?status=DRAFT",
			"/drafts/",
			"/publish",
		} {
			if !strings.Contains(bundle, anchor) {
				t.Fatalf("canvas bundle 缺端点锚 %q(前后端契约漂移)", anchor)
			}
		}

		// 目录列表 fail-closed:assets/ 无 index.html,FileServer 默认会
		// 渲染 autoindex——noDirListing 必须把它翻成 404(对抗审查 LOW,
		// 未来 sourcemap 等新目录默认拒绝)。
		if w := webGet(t, r, "/ui/canvas/assets/"); w.Code != http.StatusNotFound {
			t.Fatalf("/ui/canvas/assets/ = %d, want 404(禁目录列表)", w.Code)
		}
		// 有 index.html 的目录(/ui/canvas/)不受影响,仍 200。
		if w := webGet(t, r, "/ui/canvas/"); w.Code != http.StatusOK {
			t.Fatalf("/ui/canvas/ = %d, want 200", w.Code)
		}
	})
}

// extractAttr 从 HTML 里取第一个满足 tag+attr 的属性值(画布产物唯一
// script/stylesheet 引用;提不出来 = 产物形态漂移,红即是胜利)。
// link 限定 rel="stylesheet":未来加 favicon 等其它 link 不误捕、也
// 不产出误导性的 content-type 失败(对抗审查 LOW)。
func extractAttr(t *testing.T, page, tag, attr string) string {
	t.Helper()
	for _, line := range strings.Split(page, "\n") {
		if !strings.Contains(line, "<"+tag) || !strings.Contains(line, attr+`="`) {
			continue
		}
		if tag == "link" && !strings.Contains(line, `rel="stylesheet"`) {
			continue
		}
		q := attr + `="`
		i := strings.Index(line, q)
		if i < 0 {
			continue
		}
		rest := line[i+len(q):]
		if j := strings.Index(rest, `"`); j >= 0 {
			return rest[:j]
		}
	}
	t.Fatalf("canvas index.html 未找到 %s[%s] 引用:\n%s", tag, attr, page)
	return ""
}
