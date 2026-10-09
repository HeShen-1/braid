package api

// T21.2 Web 静态面契约测试(路由硬切 + 统一 SPA):入口面 + 资产面 + 安全头。
// 零栈依赖——Server 构造传 nil dbh(静态路由不触 DB;NewServer 要求非 nil 的
// registry/trig/idp,dbh 是运行期依赖;idp 用零值 BuiltinProvider——本文件
// 不触登录路由,零值 DB 即无害),CI 无 t13 栈也全量覆盖。
//
// 锚五件事:
//  ① 入口 200 面:/、/login 与三条客户端路由(精确路径 + 深链 + 尾斜杠)
//     必须 200 + text/html + Location 空 + 体恰等入口字节 + 同套 CSP/
//     安全头——入口是 200 而非 301(FileServer 对 index.html 结尾做
//     ./ 归一化,深链会被跳走;D-2026-10-09-ui-console 决策 6);
//  ② 硬切 404 面:/ui 与 /ui/* 不再注册任何路由 → gin 默认 404 且零重定向
//     (用户明确「硬切干净」;旧三件套 index.html/app.js/style.css 已删,
//     404 是终态而非过渡态);
//  ③ 资产面可达:入口 HTML 引用的 script/modulepreload/stylesheet 逐条回取
//     200(哈希产物名,漂移门在 CI web job,此处锚可达性);CSS 里
//     url(/assets/*.woff2) 的自托管字体同判(font-src 'self' 的真实载荷);
//     全部 assets/*.js 合并断言 bundle 端点锚仍在(前后端契约漂移在此红);
//  ④ 缺失不回落:assets/nope.js → 404、assets/ 目录 → 404(noDirListing
//     禁目录列表 fail-closed);入口面之外的路径不再享受任何 HTML 回退
//     (旧 /ui/canvas/* 的 spaRoute 随硬切删除);
//  ⑤ CSP 全串逐字 + 四条安全头逐字(D-t44 安全姿态原样继承,D-ui-console
//     决策 1/5:CSP 唯一允许的改动是新增 font-src 'self';零 unsafe-*)。
//
// 变异自查:入口面改 301/404 → ①红;重挂 /ui/* → ②红;CSP 增删/改序/放宽
// unsafe-* → ⑤红;X-Frame-Options 删 DENY 或 nosniff/no-referrer/no-cache
// 漂移 → ⑤红;assets 目录放行列表 → ④红;入口指到不存在的 bundle/CSS →
// ③红;删 font-src 而 CSS 仍引 woff2 → ⑤红(词面锚)。

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"braid/internal/auth"
)

// wantWebCSP 与 webCSP 同源,但逐字重写一份:断言若直接引用常量,常量被
// 变异(删指令/放宽 unsafe-*)时断言跟着漂移 = 哑绿;逐字副本才挡得住。
const wantWebCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"connect-src 'self'; img-src 'self'; font-src 'self'; base-uri 'none'; " +
	"frame-ancestors 'none'; form-action 'self'"

// webEntryPaths 是入口 200 面的全清单(D-ui-console 决策 6 的 SPA 路由表,
// 每种路由给三种请求形态:精确路径、尾斜杠、深链)。
var webEntryPaths = []string{
	"/", "/login",
	"/workflows", "/workflows/", "/workflows/d-7",
	"/executions", "/executions/", "/executions/1234567890",
	"/approvals", "/approvals/", "/approvals/1234567890/node-7",
}

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

// assertSecurityHeaders 逐字锚静态面五头(CSP 全串 + 四条安全头)。
func assertSecurityHeaders(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	csp := w.Header().Get("Content-Security-Policy")
	// 全串逐字锚(D-ui-console 决策 1/5):逐词 contains 挡不住「多塞一条
	// 指令」与改序;逐字锚把口径收成一条——任何增删/改序/放宽在此红。
	if csp != wantWebCSP {
		t.Fatalf("%s CSP 逐字漂移(D-ui-console 决策 1/5:只允许 font-src 'self' 一条新增):\n got %q\nwant %q",
			what, csp, wantWebCSP)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("%s CSP 含 unsafe-*: %q", what, csp)
	}
	for _, h := range []struct{ name, want string }{
		// 点击劫持旧浏览器兜底(frame-ancestors 不识别的客户端)。
		{"X-Frame-Options", "DENY"},
		{"X-Content-Type-Options", "nosniff"},
		{"Referrer-Policy", "no-referrer"},
		{"Cache-Control", "no-cache"},
	} {
		if got := w.Header().Get(h.name); got != h.want {
			t.Fatalf("%s %s = %q, want %q", what, h.name, got, h.want)
		}
	}
}

// TestWebSPAEntryServed 入口 200 面:/、/login 与三条客户端路由的三种请求
// 形态都必须吐同一份构造期读入的入口字节,且不是跳转。
func TestWebSPAEntryServed(t *testing.T) {
	r := newWebTestRouter(t)

	base := webGet(t, r, "/")
	if base.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200", base.Code)
	}
	index := base.Body.String()
	// 入口形态锚:SPA 挂载点在场 + 资产引用是根绝对路径(vite base = "/",
	// T21.2 硬切的一部分;base 退回 /ui/canvas/ 即在此红)。
	if !strings.Contains(index, `id="root"`) {
		t.Fatalf("入口 HTML 缺 SPA 挂载点:\n%s", index)
	}
	if !strings.Contains(index, `src="/assets/`) || !strings.Contains(index, `href="/assets/`) {
		t.Fatalf("入口 HTML 资产引用不是根绝对路径 /assets/(vite base 漂移):\n%s", index)
	}
	// 内联脚本/内联样式禁入(CSP 无 unsafe-inline,内联即静默失效):
	// script 标签必须带 src,禁 style 属性与内联事件句柄。
	lower := strings.ToLower(index)
	for _, banned := range []string{"onclick=", "onerror=", "onload=", "javascript:"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("入口 HTML 含内联事件句柄/伪协议 %q(CSP 违约)", banned)
		}
	}
	if strings.Contains(index, "<script>") {
		t.Fatalf("入口 HTML 含无 src 的 script 标签(CSP 违约)")
	}
	if strings.Contains(index, ` style="`) || strings.Contains(index, ` style='`) {
		t.Fatalf("入口 HTML 含内联 style 属性(CSP 违约)")
	}

	for _, p := range webEntryPaths {
		w := webGet(t, r, p)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200(SPA 入口面)", p, w.Code)
		}
		// 入口面必须是 200 而非 301/302:FileServer 对 /index.html 结尾的
		// 路径做 ./ 归一化跳转,跳转即丢客户端路由(SPA 深链启动面失效)。
		if loc := w.Header().Get("Location"); loc != "" {
			t.Fatalf("%s 返回跳转 %q(入口面必须 200,深链不得被归一化)", p, loc)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("%s content-type = %q, want text/html", p, ct)
		}
		if w.Body.String() != index {
			t.Fatalf("%s 响应体与入口 / 不一致(入口面必须吐同一份入口字节)", p)
		}
		assertSecurityHeaders(t, w, p)
	}

	// 入口前缀下一切路径都是客户端路由空间(资源面独立收敛在 /assets/):
	// 带 '.' 的路径同样回入口 HTML——旧 /ui/canvas/* 的 spaRoute「末段含
	// '.' 即 404」例外随硬切删除,不再有第二口径(web_test 锚死这个方向,
	// 免得未来有人把资源后缀例外请回来)。
	for _, p := range []string{"/workflows/nope.js", "/executions/1/2.css"} {
		w := webGet(t, r, p)
		if w.Code != http.StatusOK || w.Body.String() != index {
			t.Fatalf("%s = %d, want 200 入口字节(入口前缀不设资源后缀例外,资源面只在 /assets/)", p, w.Code)
		}
	}
}

// TestWebOldUIPrefixHardCut 硬切契约:旧审批台前缀 /ui 与其下全部路径零
// 路由注册 → gin 默认 404,且不留任何重定向(用户「硬切干净」拍板)。
func TestWebOldUIPrefixHardCut(t *testing.T) {
	r := newWebTestRouter(t)
	for _, p := range []string{
		"/ui", "/ui/", "/ui/index.html",
		"/ui/app.js", "/ui/style.css", // 旧三件套已删,404 是终态
		"/ui/canvas", "/ui/canvas/", "/ui/canvas/executions", "/ui/canvas/executions/1234",
		"/ui/canvas/assets/index-abc123.js",
		"/ui/executions/1234", "/ui/approvals/1234/node-1",
	} {
		w := webGet(t, r, p)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404(旧 /ui/* 已硬切,零路由注册)", p, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "" {
			t.Fatalf("%s 返回跳转 %q,硬切契约不留重定向", p, loc)
		}
	}

	// 结构性锚(行为 404 之外再钉一步):路由表里零 /ui 前缀——未来有人
	// 重挂 /ui 但被中间件挡成 404 也在此红。同表钉住新静态面全清单:
	// 少一条入口路由 = 某类深链 404;多一条 = 静态面扩张未经契约确认。
	var got []string
	for _, ri := range r.Routes() {
		if strings.HasPrefix(ri.Path, "/ui") {
			t.Fatalf("静态面路由表重挂旧前缀: %s %s", ri.Method, ri.Path)
		}
		if ri.Method != http.MethodGet || strings.HasPrefix(ri.Path, "/api/") ||
			ri.Path == "/healthz" || ri.Path == "/mcp" {
			continue
		}
		got = append(got, ri.Path)
	}
	sort.Strings(got)
	want := []string{
		"/", "/approvals", "/approvals/*filepath", "/assets/*filepath",
		"/executions", "/executions/*filepath", "/login",
		"/workflows", "/workflows/*filepath",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("静态面路由表漂移:\n got %v\nwant %v", got, want)
	}
}

// TestWebAssetsServed 资产面:入口引用逐条可达 + 自托管字体可达 + bundle
// 端点锚 + 缺失与目录 fail-closed。
func TestWebAssetsServed(t *testing.T) {
	r := newWebTestRouter(t)
	page := webGet(t, r, "/").Body.String()

	// 入口引用的 script/modulepreload/stylesheet 逐条回取(哈希产物名从
	// 页面提取,漂移门在 CI web job 的 git diff,此处锚可达性)。
	var jsRefs, cssRefs []string
	refs := webEntryRefs(t, page)
	for _, ref := range refs {
		w := webGet(t, r, ref)
		if w.Code != http.StatusOK {
			t.Fatalf("入口引用 %s = %d, want 200(构建产物与 embed 指令同 PR 旅行)", ref, w.Code)
		}
		assertSecurityHeaders(t, w, ref)
		switch {
		case strings.HasSuffix(ref, ".js"):
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
				t.Fatalf("%s content-type = %q, want *javascript*", ref, ct)
			}
			jsRefs = append(jsRefs, ref)
		case strings.HasSuffix(ref, ".css"):
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
				t.Fatalf("%s content-type = %q, want text/css", ref, ct)
			}
			cssRefs = append(cssRefs, ref)
		}
	}
	if len(jsRefs) == 0 || len(cssRefs) == 0 {
		t.Fatalf("入口引用抽取不足(js=%v css=%v):入口必须引脚本与样式表", jsRefs, cssRefs)
	}

	// 自托管字体(CSP font-src 'self' 的唯一载荷):CSS 里
	// url(/assets/*.woff2) 逐条回取——字体没入库或路径漂移即红;零字体
	// 引用同样红(说明 CSP 放宽失去载荷依据)。
	fonts := 0
	for _, ref := range cssRefs {
		for _, u := range cssAssetURLs(webGet(t, r, ref).Body.String()) {
			w := webGet(t, r, u)
			if w.Code != http.StatusOK {
				t.Fatalf("CSS 引用 %s = %d, want 200(自托管字体必须入库)", u, w.Code)
			}
			if strings.HasSuffix(u, ".woff2") {
				fonts++
			}
		}
	}
	if fonts == 0 {
		t.Fatalf("CSS 未引用任何 /assets/*.woff2 字体(font-src 'self' 载荷漂移)")
	}

	// 端点锚:全部 assets/*.js 经 HTTP 逐条回取后合并正文断言——代码分割
	// 把 api 面放进共享 chunk,只扫入口 bundle 会漏掉契约漂移(锚属性访问
	// 形态的端点字符串;React 运行时内部合法使用 innerHTML,bundle 级
	// 子串禁令会误伤框架本体,故 XSS 纪律锚在 vitest 源码扫描)。
	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	names, err := fs.Glob(assets, "assets/*.js")
	if err != nil || len(names) == 0 {
		t.Fatalf("embed 面无 assets/*.js(names=%v err=%v):dist 与 embed 指令必须同 PR 旅行", names, err)
	}
	var bundles strings.Builder
	for _, name := range names {
		w := webGet(t, r, "/"+name)
		if w.Code != http.StatusOK {
			t.Fatalf("/%s = %d, want 200", name, w.Code)
		}
		bundles.WriteString(w.Body.String())
	}
	bundle := bundles.String()
	for _, anchor := range []string{
		"/api/v1/auth/login",
		"/api/v1/approvals/pending",
		"/decision",
		"/definitions?status=DRAFT",
		"/drafts/",
		"/publish",
		// 执行面:列表/详情/触发(幂等键头)/回滚/续跑/审批子资源/审计查询
		// + 触发面板的已发布定义下拉。
		"/api/v1/executions",
		"/rollback",
		"/resume",
		"/approvals/",
		"/audit?execution_id=",
		"/definitions?status=PUBLISHED",
	} {
		if !strings.Contains(bundle, anchor) {
			t.Fatalf("前端 bundle 缺端点锚 %q(前后端契约漂移)", anchor)
		}
	}

	// 缺失即 404,不回落入口:入口面之外的路径不再享受任何 HTML 回退
	// (旧 /ui/canvas/* 的 spaRoute 已随硬切删除)——资源缺失必须响亮,
	// 回退成 HTML 会让 module 加载失败变成 MIME 报错、掩盖真实 404。
	for _, p := range []string{"/assets/nope.js", "/assets/nope.css", "/assets/nope.woff2"} {
		if w := webGet(t, r, p); w.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", p, w.Code)
		}
	}
	// 目录列表 fail-closed:assets/ 无 index.html,FileServer 默认渲染
	// autoindex——noDirListing 必须翻成 404(未来 sourcemap 等新目录默认
	// 拒绝,无需逐个登记)。
	if w := webGet(t, r, "/assets/"); w.Code != http.StatusNotFound {
		t.Fatalf("/assets/ = %d, want 404(禁目录列表)", w.Code)
	}
}

// webEntryRefs 从入口 HTML 提取页面加载期引用的资产路径(script[src] 与
// rel=stylesheet/modulepreload 的 link[href]),按出现序去重返回。提不出
// script/stylesheets = 产物形态漂移,由调用方断言数量。
func webEntryRefs(t *testing.T, page string) []string {
	t.Helper()
	attr := func(line, key string) (string, bool) {
		q := key + `="`
		i := strings.Index(line, q)
		if i < 0 {
			return "", false
		}
		rest := line[i+len(q):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return "", false
		}
		return rest[:j], true
	}
	var refs []string
	seen := map[string]bool{}
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			refs = append(refs, v)
		}
	}
	for _, line := range strings.Split(page, "\n") {
		switch {
		case strings.Contains(line, "<script"):
			if v, ok := attr(line, "src"); ok {
				add(v)
			}
		case strings.Contains(line, "<link"):
			// 只认加载期资产;favicon 等其它 rel 不属本契约面。
			if !strings.Contains(line, `rel="stylesheet"`) && !strings.Contains(line, `rel="modulepreload"`) {
				continue
			}
			if v, ok := attr(line, "href"); ok {
				add(v)
			}
		}
	}
	return refs
}

// cssAssetURLs 提取 CSS 里以 /assets/ 开头的 url(...) 引用(vite base "/"
// 契约下的产物自引用;data: 等内联形态不属本面,静默跳过)。
func cssAssetURLs(css string) []string {
	var out []string
	for _, seg := range strings.Split(css, "url(") {
		i := strings.Index(seg, ")")
		if i < 0 {
			continue
		}
		u := strings.Trim(seg[:i], ` "'`)
		if strings.HasPrefix(u, "/assets/") {
			out = append(out, u)
		}
	}
	return out
}
