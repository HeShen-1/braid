// 本标签页的操作完成后刷新读面；不广播token或业务载荷。
export const CONSOLE_REFRESH = 'braid:console-refresh'
export const LIVE_REFRESH_MS = 5000
export function refreshConsole() { window.dispatchEvent(new Event(CONSOLE_REFRESH)) }
