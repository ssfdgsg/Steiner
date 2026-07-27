// 控制台外壳：侧边导航 + 页面切换。
// 路由用 hash（#/presets）而非 History API——嵌入式静态托管下无需服务端改写规则。
import { useEffect, useState } from 'react'
import { Presets } from './pages/Presets'
import { Backends } from './pages/Backends'
import { Explain } from './pages/Explain'
import { Runtime } from './pages/Runtime'

const NAV = [
  { key: 'presets', label: '调度方案', node: <Presets /> },
  { key: 'backends', label: '后端实例', node: <Backends /> },
  { key: 'explain', label: '调度解释器', node: <Explain /> },
  { key: 'runtime', label: '运行态', node: <Runtime /> },
] as const

type NavKey = (typeof NAV)[number]['key']

function currentKey(): NavKey {
  const h = window.location.hash.replace(/^#\/?/, '')
  return (NAV.find((n) => n.key === h)?.key ?? 'presets') as NavKey
}

export function App() {
  const [page, setPage] = useState<NavKey>(currentKey)

  useEffect(() => {
    const onHash = () => setPage(currentKey())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  const active = NAV.find((n) => n.key === page) ?? NAV[0]

  return (
    <div className="app">
      <nav className="sidebar">
        <div className="brand">
          ai-gateway
          <small>推理调度控制台</small>
        </div>
        {NAV.map((n) => (
          <button
            key={n.key}
            className={n.key === page ? 'nav-item active' : 'nav-item'}
            onClick={() => {
              window.location.hash = `#/${n.key}`
              setPage(n.key)
            }}
          >
            {n.label}
          </button>
        ))}
        <div style={{ flex: 1 }} />
        <a className="nav-item" href="/metrics" target="_blank" rel="noreferrer">
          原始指标 ↗
        </a>
      </nav>
      <main className="main">{active.node}</main>
    </div>
  )
}
