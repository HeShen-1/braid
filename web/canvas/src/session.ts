// 会话态:token 与用户名同存 sessionStorage(每标签页独立,与
// D-t44 #1 的 Bearer 载体拍板同源)。tokenStore 仍在 api.ts——它是
// HTTP 契约的一部分;这里只补"当前是谁"。
import { tokenStore } from './api.ts'

const USER_KEY = 'braid_canvas_user'

function readUser(): string | null {
  try {
    return sessionStorage.getItem(USER_KEY)
  } catch {
    return null
  }
}

export const session = {
  get user(): string | null {
    return readUser()
  },
  get authed(): boolean {
    return tokenStore.get() !== null
  },
  start(token: string, username: string) {
    tokenStore.set(token)
    try {
      sessionStorage.setItem(USER_KEY, username)
    } catch {
      /* 私密模式等存储不可用:仅本次内存态生效 */
    }
  },
  clear() {
    tokenStore.clear()
    try {
      sessionStorage.removeItem(USER_KEY)
    } catch {
      /* 同上 */
    }
  },
}
