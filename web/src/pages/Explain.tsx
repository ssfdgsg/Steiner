// 调度解释器页：模拟一次选路并展示逐后端打分明细，回答"为什么路由到 X"。
import { useState } from 'react'
import { api, type ExplainResp, type PresetsResp } from '../api'
import { usePoll } from '../hooks'
import { Empty, Meter } from '../components'
import { num } from '../format'

export function Explain() {
  const { data: presets } = usePoll<PresetsResp>(() => api.presets(), 0)
  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [policy, setPolicy] = useState('')
  const [session, setSession] = useState('')
  const [result, setResult] = useState<ExplainResp | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [running, setRunning] = useState(false)

  const run = async () => {
    setRunning(true)
    setError(null)
    try {
      setResult(await api.explain({ model, prompt, policy, session }))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setResult(null)
    } finally {
      setRunning(false)
    }
  }

  const policyNames = Object.keys(presets?.policies ?? {}).sort()

  return (
    <>
      <div className="page-head">
        <div>
          <h1>调度解释器</h1>
          <p className="page-desc">
            对给定请求特征模拟一次选路，展示每个后端的过滤结果与打分（升序，第一行即会被选中的后端）。
            不产生任何真实转发，也不改变任何状态。
          </p>
        </div>
      </div>

      <div className="card">
        <div className="grid">
          <div className="field">
            <label>模型名（必填，未命中路由时走 * 兜底）</label>
            <input value={model} onChange={(e) => setModel(e.target.value)} placeholder="qwen3-32b" />
          </div>
          <div className="field">
            <label>策略槽位（留空用路由默认）</label>
            <select value={policy} onChange={(e) => setPolicy(e.target.value)}>
              <option value="">（路由默认）</option>
              {policyNames.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label>会话 ID（可选，影响一致性哈希与粘性）</label>
            <input value={session} onChange={(e) => setSession(e.target.value)} placeholder="user-42" />
          </div>
        </div>
        <div className="field">
          <label>提示词（可选，用于计算前缀命中率 prefix_match）</label>
          <textarea
            rows={3}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="你是一个助手…"
          />
        </div>
        <button className="primary" onClick={run} disabled={!model || running}>
          {running ? '计算中…' : '模拟选路'}
        </button>
      </div>

      {error && <Empty text={`失败：${error}`} />}

      {result && (
        <div className="card">
          <h2>
            路由 <span className="badge">{result.route}</span> 策略{' '}
            <span className="badge">{result.strategy}</span>
            {result.strategy === 'expression' && (
              <>
                {' '}
                使用 <span className="badge accent">{result.policy}</span>
              </>
            )}
          </h2>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>排名</th>
                  <th>后端</th>
                  <th>分数（越小越优）</th>
                  <th>过滤</th>
                  <th>可用</th>
                  <th>前缀命中</th>
                  <th>在途</th>
                  <th>运行中</th>
                  <th>排队</th>
                  <th>KV 占用</th>
                  <th>备注</th>
                </tr>
              </thead>
              <tbody>
                {result.scores.map((s, i) => (
                  <tr key={s.backend}>
                    <td>{i === 0 && s.pass && s.available ? <span className="badge accent">选中</span> : i + 1}</td>
                    <td>
                      <strong>{s.backend}</strong>
                    </td>
                    <td className="num">{num(s.score, 4)}</td>
                    <td>
                      {s.pass ? (
                        <span className="badge ok">通过</span>
                      ) : (
                        <span className="badge err">淘汰</span>
                      )}
                    </td>
                    <td>{s.available ? '是' : <span className="badge err">否</span>}</td>
                    <td className="num">{num(s.prefix_match * 100, 1)}%</td>
                    <td className="num">{s.inflight}</td>
                    <td className="num">{num(s.running)}</td>
                    <td className="num">{num(s.waiting)}</td>
                    <td>
                      <Meter value={s.kv_usage} />
                    </td>
                    <td className="hint">{s.err ?? ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="hint" style={{ marginBottom: 0 }}>
            注：非 expression 策略（轮询/随机/一致性哈希等）不使用分数，此处的打分仅供参考对比。
          </p>
        </div>
      )}
    </>
  )
}
