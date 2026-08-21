// 控制台外壳：固定侧栏 + 上下文栏 + 内容区。
// 路由用 hash（#/overview）而非 History API——嵌入式静态托管无需服务端改写规则，刷新不 404。
import { useEffect, useMemo, useState } from 'react'
import { api, checkAuth, ApiError } from './api'
import { getToken, setToken, clearToken, AUTHED_EVENT, UNAUTHORIZED_EVENT } from './auth'
import { RefreshProvider, useRefreshState, usePoll } from './hooks'
import { Icon, type IconName } from './icons'
import { Freshness } from './components'
import { duration } from './format'
import { Overview } from './pages/Overview'
import { Presets } from './pages/Presets'
import { Backends } from './pages/Backends'
import { Explain } from './pages/Explain'
import { Runtime } from './pages/Runtime'
import { CacheQueue } from './pages/CacheQueue'
import { Operations } from './pages/Operations'
import steinerSchedulerMark from './assets/steiner-scheduler-mark-cropped.png'

/** LoginGate 管理面令牌登录页：静态壳公开加载，数据请求需先认证（C1）。 */
function LoginGate() {
  const [value, setValue] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const token = value.trim()
    if (!token) {
      setError('请输入管理面令牌（config 中 server.admin_token）')
      return
    }
    setBusy(true)
    setError(null)
    try {
      setToken(token)
      await checkAuth()
    } catch (err) {
      clearToken()
      setError(
        err instanceof ApiError
          ? `令牌验证失败（HTTP ${err.status}）：${err.message}`
          : '令牌验证失败，请确认网关已启动且令牌正确',
      )
      setBusy(false)
      return
    }
    setBusy(false)
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: 'var(--bg, #14161d)',
        color: 'var(--fg, #e8eaf0)',
      }}
    >
      <form
        onSubmit={submit}
        style={{
          width: 380,
          padding: 28,
          borderRadius: 12,
          background: 'var(--panel, #1d2029)',
          border: '1px solid var(--border, #2c3040)',
          display: 'flex',
          flexDirection: 'column',
          gap: 14,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <img src={steinerSchedulerMark} alt="" style={{ width: 34, height: 34 }} />
          <div>
            <strong style={{ fontSize: 17 }}>Steiner 管理控制台</strong>
            <div style={{ fontSize: 12, opacity: 0.65 }}>推理网关调度控制台</div>
          </div>
        </div>
        <label style={{ fontSize: 13, opacity: 0.85 }}>
          管理面令牌（server.admin_token）
          <input
            type="password"
            autoFocus
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="Bearer 令牌"
            style={{
              display: 'block',
              width: '100%',
              boxSizing: 'border-box',
              marginTop: 6,
              padding: '8px 10px',
              borderRadius: 8,
              border: '1px solid var(--border, #2c3040)',
              background: 'var(--input-bg, #171a22)',
              color: 'inherit',
              fontSize: 14,
            }}
          />
        </label>
        {error && <div style={{ fontSize: 13, color: '#ff6b6b' }}>{error}</div>}
        <button type="submit" className="btn primary" disabled={busy} style={{ alignSelf: 'flex-end' }}>
          {busy ? '验证中…' : '登录'}
        </button>
        <div style={{ fontSize: 12, opacity: 0.55 }}>
          令牌仅保存在本浏览器 localStorage，用于请求头 `Authorization: Bearer &lt;token&gt;`；登出即清除。
        </div>
      </form>
    </div>
  )
}

interface NavEntry {
  key: string
  label: string
  icon: IconName
  node: JSX.Element
  /** aliases 旧 hash 兼容，保证已有书签不失效。 */
  aliases?: string[]
}

const NAV: NavEntry[] = [
  { key: 'overview', label: '总览', icon: 'overview', node: <Overview /> },
  { key: 'presets', label: '调度方案', icon: 'policy', node: <Presets /> },
  { key: 'backends', label: '后端实例', icon: 'server', node: <Backends /> },
  { key: 'explain', label: '调度解释器', icon: 'explain', node: <Explain /> },
  { key: 'runtime', label: '运行监控', icon: 'monitor', node: <Runtime />, aliases: ['runtime'] },
  { key: 'cache-queue', label: 'KV Cache 与队列', icon: 'cache', node: <CacheQueue /> },
  { key: 'operations', label: '告警与扩缩容', icon: 'bell', node: <Operations /> },
]

const REFRESH_OPTIONS = [
  { value: 2000, label: '2s' },
  { value: 5000, label: '5s' },
  { value: 15000, label: '15s' },
  { value: 60000, label: '60s' },
]

function currentKey(): string {
  const h = window.location.hash.replace(/^#\/?/, '').split('?')[0]
  const hit = NAV.find((n) => n.key === h || n.aliases?.includes(h))
  return hit?.key ?? NAV[0].key
}

export function App() {
  const [page, setPage] = useState<string>(currentKey)
  const [collapsed, setCollapsed] = useState(false)
  const [authed, setAuthed] = useState<boolean>(() => getToken() !== '')
  const refresh = useRefreshState(5000)

  useEffect(() => {
    const onHash = () => setPage(currentKey())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  // 认证事件：401 → 登出态；登录成功 → 进入控制台（已挂载的轮询带令牌自动恢复）。
  useEffect(() => {
    const onUnauthorized = () => setAuthed(false)
    const onAuthed = () => setAuthed(true)
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
    window.addEventListener(AUTHED_EVENT, onAuthed)
    return () => {
      window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
      window.removeEventListener(AUTHED_EVENT, onAuthed)
    }
  }, [])

  if (!authed) {
    return <LoginGate />
  }

  const active = NAV.find((n) => n.key === page) ?? NAV[0]

  // 顶栏健康摘要与侧栏运行信息共用一份 stats 轮询（各页面不重复请求）。
  const stats = usePoll(api.stats, refresh.intervalMs > 0 ? refresh.intervalMs * 2 : 0, refresh.nonce)
  const health = useMemo(() => {
    const b = stats.data?.backends
    if (!b) return { tone: 'muted', text: '状态未知' }
    if (b.total === 0) return { tone: 'warn', text: '无已注册后端' }
    if (b.available === 0) return { tone: 'err', text: '无可用后端' }
    if (b.available < b.total) return { tone: 'warn', text: `${b.available}/${b.total} 后端可用` }
    return { tone: 'ok', text: '全部后端可用' }
  }, [stats.data])

  return (
    <RefreshProvider value={refresh}>
      <div className="app">
        <nav className={collapsed ? 'sidebar collapsed' : 'sidebar'} aria-label="主导航">
          <div className="brand">
            <span className="brand-mark" aria-hidden="true">
              <img src={steinerSchedulerMark} alt="" />
            </span>
            <span className="brand-text">
              <strong>Steiner</strong>
              <span>推理网关控制台</span>
            </span>
          </div>
          <div className="nav">
            {NAV.map((n) => (
              <a
                key={n.key}
                href={`#/${n.key}`}
                className={n.key === page ? 'nav-item active' : 'nav-item'}
                aria-current={n.key === page ? 'page' : undefined}
                title={collapsed ? n.label : undefined}
              >
                <span className="ico">
                  <Icon name={n.icon} />
                </span>
                <span className="label">{n.label}</span>
              </a>
            ))}
            <div className="nav-group">外部工具</div>
            <a className="nav-item" href="/metrics" target="_blank" rel="noreferrer" title="原始指标">
              <span className="ico">
                <Icon name="external" />
              </span>
              <span className="label">原始指标</span>
            </a>
          </div>
          <div className="sidebar-foot">
            <dl>
              <dt>运行时长</dt>
              <dd>{duration(stats.data?.uptime_seconds)}</dd>
              <dt>累计请求</dt>
              <dd>{stats.data ? stats.data.aggregate.requests_total.toLocaleString('zh-CN') : '-'}</dd>
            </dl>
            <div className="collapse-row">
              <button
                className="btn sm ghost"
                onClick={() => setCollapsed((v) => !v)}
                aria-label={collapsed ? '展开侧栏' : '折叠侧栏'}
              >
                <Icon name={collapsed ? 'chevronRight' : 'chevronLeft'} size={13} />
                {!collapsed && '折叠'}
              </button>
            </div>
          </div>
        </nav>

        <main className="main">
          <header className="topbar">
            <h1>{active.label}</h1>
            <span className={`badge ${health.tone}`}>
              <span className="dot" />
              {health.text}
            </span>
            <div className="topbar-tools">
              <Freshness state={stats} />
              <label className="switch">
                <input
                  type="checkbox"
                  checked={refresh.auto}
                  onChange={(e) => refresh.setAuto(e.target.checked)}
                />
                自动刷新
              </label>
              <select
                aria-label="刷新频率"
                style={{ width: 88 }}
                value={String(refresh.selectedMs)}
                disabled={!refresh.auto}
                onChange={(e) => refresh.setIntervalMs(Number(e.target.value))}
              >
                {REFRESH_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
              <button className="btn icon" onClick={refresh.bump} aria-label="立即刷新" title="立即刷新">
                <Icon name="refresh" />
              </button>
              <button className="btn ghost" onClick={clearToken} title="清除令牌并回到登录页">
                登出
              </button>
            </div>
          </header>
          <div className="content">{active.node}</div>
        </main>
      </div>
    </RefreshProvider>
  )
}
