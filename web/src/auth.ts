// 控制台管理面令牌存取（C1 认证配套）。
//
// 流程：/admin/ui/ 静态壳公开加载（SPA 先加载后登录）；用户输入
// server.admin_token 后存 localStorage，此后所有 admin 数据请求携带
// `Authorization: Bearer <token>`；任一请求 401 时派发 unauthorized 事件，
// App 回到登录态。
const TOKEN_KEY = 'steiner_admin_token'
let sessionToken = ''

export const UNAUTHORIZED_EVENT = 'steiner:unauthorized'
export const AUTHED_EVENT = 'steiner:authed'

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? sessionToken
  } catch {
    return sessionToken
  }
}

/** setToken 保存令牌并通知 App 已登录。 */
export function setToken(token: string): void {
  sessionToken = token
  try {
    localStorage.setItem(TOKEN_KEY, token)
  } catch {
    /* 隐私模式下 localStorage 不可用：仅本次会话内有效 */
  }
  window.dispatchEvent(new CustomEvent(AUTHED_EVENT))
}

/** clearToken 清除令牌（登出 / 令牌失效）。 */
export function clearToken(): void {
  sessionToken = ''
  try {
    localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* noop */
  }
  window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT))
}
