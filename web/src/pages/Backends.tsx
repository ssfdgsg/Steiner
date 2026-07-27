// 后端总览页：实时负载表格、隔离/恢复、动态注册与摘除。
import { useState } from 'react'
import { api, type BackendView } from '../api'
import { usePoll, useToast } from '../hooks'
import { Empty, Meter, StateBadge, Toast } from '../components'
import { num } from '../format'

export function Backends() {
  const { data, error, loading, refresh } = usePoll<BackendView[]>(() => api.backends(), 2000)
  const { toast, show } = useToast()
  const [busy, setBusy] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)

  const act = async (id: string, fn: () => Promise<unknown>, okText: string) => {
    setBusy(id)
    try {
      await fn()
      show('ok', okText)
      refresh()
    } catch (e) {
      show('err', e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  if (loading) return <Empty text="加载中…" />
  if (error) return <Empty text={`加载失败：${error}`} />

  const list = data ?? []

  return (
    <>
      <div className="page-head">
        <div>
          <h1>后端实例</h1>
          <p className="page-desc">
            每 2 秒刷新。指标来自后端 /metrics 直采归一化，在途数为网关侧视角。
          </p>
        </div>
        <button className="primary" onClick={() => setAdding((v) => !v)}>
          {adding ? '收起' : '+ 注册后端'}
        </button>
      </div>

      {adding && <AddBackendForm onDone={() => { setAdding(false); refresh() }} onToast={show} />}

      <div className="card">
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>ID</th>
                <th>引擎</th>
                <th>状态</th>
                <th>在途</th>
                <th>运行中</th>
                <th>排队</th>
                <th>KV 占用</th>
                <th>前缀命中</th>
                <th>吞吐 tok/s</th>
                <th>地址</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {list.map((b) => (
                <tr key={b.id}>
                  <td>
                    <strong>{b.id}</strong>
                    {b.snapshot?.err && (
                      <div className="hint" title={b.snapshot.err}>
                        采集异常
                      </div>
                    )}
                  </td>
                  <td>{b.engine}</td>
                  <td>
                    <StateBadge healthy={b.healthy} cordoned={b.cordoned} ejected={b.ejected} />
                  </td>
                  <td className="num">{b.inflight}</td>
                  <td className="num">{num(b.snapshot?.running)}</td>
                  <td className="num">{num(b.snapshot?.waiting)}</td>
                  <td>
                    <Meter value={b.snapshot?.kv_usage ?? 0} />
                  </td>
                  <td className="num">{num((b.snapshot?.hit_rate ?? 0) * 100, 1)}%</td>
                  <td className="num">{num(b.snapshot?.gen_tok_per_sec, 1)}</td>
                  <td className="hint">{b.url}</td>
                  <td>
                    <div className="row" style={{ gap: 6 }}>
                      <button
                        disabled={busy === b.id}
                        onClick={() =>
                          act(
                            b.id,
                            () => api.cordon(b.id, !b.cordoned),
                            b.cordoned ? `${b.id} 已恢复接流` : `${b.id} 已隔离`,
                          )
                        }
                      >
                        {b.cordoned ? '恢复' : '隔离'}
                      </button>
                      <button
                        className="danger"
                        disabled={busy === b.id}
                        onClick={() => {
                          if (!confirm(`确认摘除后端 ${b.id}？在途请求会正常完成。`)) return
                          void act(b.id, () => api.removeBackend(b.id), `${b.id} 已摘除`)
                        }}
                      >
                        摘除
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
              {list.length === 0 && (
                <tr>
                  <td colSpan={11}>
                    <Empty text="暂无后端" />
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>

      <Toast toast={toast} />
    </>
  )
}

/** AddBackendForm 动态注册后端表单（对应 POST /admin/backends）。 */
function AddBackendForm({
  onDone,
  onToast,
}: {
  onDone: () => void
  onToast: (kind: 'ok' | 'err', text: string) => void
}) {
  const [id, setId] = useState('')
  const [url, setUrl] = useState('')
  const [engine, setEngine] = useState('vllm')
  const [models, setModels] = useState('')
  const [maxConc, setMaxConc] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const submit = async () => {
    setSubmitting(true)
    try {
      await api.addBackend({
        id,
        url,
        engine,
        models: models
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean),
        max_concurrency: maxConc ? Number(maxConc) : 0,
      })
      onToast('ok', `后端 ${id} 已注册`)
      onDone()
    } catch (e) {
      onToast('err', e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="card">
      <h2>注册后端</h2>
      <div className="grid">
        <div className="field">
          <label>ID（唯一）</label>
          <input value={id} onChange={(e) => setId(e.target.value)} placeholder="vllm-c" />
        </div>
        <div className="field">
          <label>地址</label>
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="http://10.0.0.9:8000"
          />
        </div>
        <div className="field">
          <label>引擎</label>
          <select value={engine} onChange={(e) => setEngine(e.target.value)}>
            <option value="vllm">vllm</option>
            <option value="vllm_omni">vllm_omni</option>
            <option value="sglang">sglang</option>
            <option value="sglang_omni">sglang_omni</option>
          </select>
        </div>
        <div className="field">
          <label>加入的模型路由（逗号分隔）</label>
          <input
            value={models}
            onChange={(e) => setModels(e.target.value)}
            placeholder="qwen3-32b, *"
          />
        </div>
        <div className="field">
          <label>并发上限（0 不限）</label>
          <input
            value={maxConc}
            onChange={(e) => setMaxConc(e.target.value)}
            placeholder="256"
            inputMode="numeric"
          />
        </div>
      </div>
      <button className="primary" onClick={submit} disabled={submitting || !id || !url || !models}>
        {submitting ? '提交中…' : '注册'}
      </button>
      <span className="hint" style={{ marginLeft: 10 }}>
        生效顺序：注册表 → 持久化（失败自动回滚）→ 集群广播；PD 路由不支持动态加入。
      </span>
    </div>
  )
}
