// 主题启动脚本(D-2026-10-09-console-v2 决策 1)。
//
// 为什么是独立文件而不是 index.html 里的内联 <script>:静态面 CSP 逐字钉死
// `script-src 'self'`(internal/api/web.go 的 webCSP,web_test.go 逐字锚),
// 内联脚本在生产直接不可执行。同源外部脚本既过 CSP,又是**阻塞式**经典脚本
// ——在 <head> 里同步执行,赶在首次绘制之前把 data-theme 落定,浅色用户不会
// 先看到一闪的深色。
//
// 为什么必须落在 /assets/ 下:静态面只服务资产前缀的文件(internal/api/web.go
// registerWeb),其余路径一律走 SPA 入口;public/assets/ 里的文件由 vite 原样
// 拷到 dist 的 assets/(不哈希,故 index.html 可以静态引用)。
//
// 键与取值口径见 src/theme.ts(用户无偏好时按分面缺省:画布浅色、运营面深色);
// src/theme.test.ts 钉住两处同源。
try {
  var t = localStorage.getItem('braid.theme')
  var routeDefault = location.pathname.indexOf('/workflows') === 0 ? 'light' : 'dark'
  document.documentElement.dataset.theme = t === 'light' || t === 'auto' || t === 'dark' ? t : routeDefault
} catch (e) {
  document.documentElement.dataset.theme = location.pathname.indexOf('/workflows') === 0 ? 'light' : 'dark'
}
