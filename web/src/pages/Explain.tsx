// 调度解释器：回答"为什么这次请求会被路由到某个实例"。
// 流程与判据全部由 GET /admin/explain 的真实响应推导，不虚构服务端内部阶段或耗时。
import { useMemo, useState } from 'react'
import { api, type ExplainResp, type PresetsResp, type ScoreDetail } from '../api'
import { usePoll } from '../hooks'
import {
  type Column,
  DataTable,
  Notice,
  Panel,
  StateBlock,
  UsageBar,
} from '../components'
import { Icon } from '../icons'
import { num, pct } from '../format'

interface HistoryItem {
  model: string
  prompt: string
  policy: string
  session: string
  winner: string
  at: string
}

/** winnerOf 最终选中实例：可用且通过过滤的候选中代价分最低者。 */
function winnerOf(scores: ScoreDetail[]): ScoreDetail | undefined {
  return scores
    .filter((s) => s.available && s.pass && !s.err)
    .reduce<ScoreDetail | undefined>((best, s) => (!best || s.score < best.score ? s : best), undefined)
}

export function Explain() {
  const presets = usePoll<PresetsResp>(api.presets, 0)
  const models = usePoll(api.models, 0)

  const [model, setModel] = useState('')
  const [prompt, setPrompt] = useState('')
  const [policy, setPolicy] = useState('')
  const [session, setSession] = useState('')
  const [result, setResult] = useState<ExplainResp | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [running, setRunning] = useState(false)
  const [history, setHistory] = useState<HistoryItem[]>([])

  const run = async () => {
    if (!model.trim()) return
    setRunning(true)
    setError(null)
    try {
      const r = await api.explain({
        model: model.trim(),
        prompt: prompt || undefined,
        policy: policy || undefined,
        session: session || undefined,
      })
      setResult(r)
      const w = winnerOf(r.scores)
      setHistory((h) =>
        [
          {
            model: model.trim(),
            prompt,
            policy,
            session,
            winner: w?.backend ?? '无可用实例',
            at: new Date().toLocaleTimeString('zh-CN', { hour12: false }),
          },
          ...h,
        ].slice(0, 8),
      )
    } catch (e) {
      setResult(null)
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setRunning(false)
    }
  }

  return (
    <>
      <p className="page-desc">
        以给定模型、Prompt、策略槽位与会话 ID 模拟一次选路，展示候选实例的过滤结果与打分明细。该操作只读，不会真实转发请求。
      </p>

      <div className="cols-7-5" style={{ gridTemplateColumns: '360px minmax(0, 1fr)' }}>
        <div className="stack">
          <Panel title="模拟条件">
            <div className="stack" style={{ gap: 12 }}>
              <div className="field">
                <label htmlFor="ex-model">模型名（必填）</label>
                <input
                  id="ex-model"
                  value={model}
                  list="ex-model-list"
                  placeholder="qwen2.5-72b"
                  onChange={(e) => setModel(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') void run()
                  }}
                />
                <datalist id="ex-model-list">
                  {(models.data ?? []).map((m) => (
                    <option key={m.name} value={m.name} />
                  ))}
                </datalist>
              </div>
              <div className="field">
                <label htmlFor="ex-prompt">Prompt（可选，用于 KV 前缀匹配）</label>
                <textarea
                  id="ex-prompt"
                  value={prompt}
                  onChange={(e) => setPrompt(e.target.value)}
                  placeholder="你是一个资深运维工程师……"
                />
              </div>
              <div className="field">
                <label htmlFor="ex-policy">策略槽位（可选，默认取路由配置）</label>
                <select id="ex-policy" value={policy} onChange={(e) => setPolicy(e.target.value)}>
                  <option value="">（按模型路由配置）</option>
                  {Object.keys(presets.data?.policies ?? {}).map((p) => (
                    <option key={p} value={p}>
                      {p}
                    </option>
                  ))}
                </select>
              </div>
              <div className="field">
                <label htmlFor="ex-session">Session ID（可选，验证会话粘性）</label>
                <input id="ex-session" value={session} onChange={(e) => setSession(e.target.value)} />
              </div>
              <button className="btn primary" disabled={!model.trim() || running} onClick={run}>
                <Icon name="explain" size={14} />
                {running ? '计算中…' : '执行解释'}
              </button>
            </div>
          </Panel>

          <Panel title="最近查询" subtitle={`${history.length} 条`}>
            {history.length === 0 ? (
              <StateBlock title="暂无查询记录" detail="本页记录仅保存在当前浏览器会话中。" />
            ) : (
              <div className="stack" style={{ gap: 8 }}>
                {history.map((h, i) => (
                  <button
                    key={`${h.at}-${i}`}
                    className="preset-row"
                    style={{ textAlign: 'left', cursor: 'pointer' }}
                    onClick={() => {
                      setModel(h.model)
                      setPrompt(h.prompt)
                      setPolicy(h.policy)
                      setSession(h.session)
                    }}
                  >
                    <span className="meta">
                      <h3>
                        <span className="mono">{h.model}</span>
                      </h3>
                      <p>
                        → {h.winner} · {h.at}
                      </p>
                    </span>
                  </button>
                ))}
              </div>
            )}
          </Panel>
        </div>

        <div className="stack">
          {error && <Notice kind="err">{error}</Notice>}
          {!result && !error && (
            <Panel title="路由决策">
              <StateBlock
                title="尚未执行解释"
                detail="填写模型名后点击「执行解释」，将展示候选过滤与打分排序过程。"
              />
            </Panel>
          )}
          {result && <Decision result={result} />}
        </div>
      </div>
    </>
  )
}

function Decision({ result }: { result: ExplainResp }) {
  const winner = useMemo(() => winnerOf(result.scores), [result])
  const available = result.scores.filter((s) => s.available).length
  const passed = result.scores.filter((s) => s.available && s.pass && !s.err).length
  const ranked = useMemo(() => [...result.scores].sort((a, b) => a.score - b.score), [result])

  const steps = [
    {
      title: '请求入站',
      detail: `模型 ${result.model}，命中路由 ${result.route}`,
      done: true,
    },
    {
      title: '策略解析',
      detail: `调度算法 ${result.strategy || '默认'}，策略槽位 ${result.policy || 'default'}`,
      done: true,
    },
    {
      title: '候选可用性',
      detail: `池内 ${result.scores.length} 个实例，其中 ${available} 个可用（健康、未隔离、未熔断、未超并发）`,
      done: available > 0,
    },
    {
      title: '过滤表达式',
      detail: `${passed} 个实例通过 filter，${available - passed} 个被淘汰`,
      done: passed > 0,
    },
    {
      title: '打分排序',
      detail: winner
        ? `最低代价 ${num(winner.score, 4)}，前缀命中 ${pct(winner.prefix_match)}`
        : '无候选可打分',
      done: !!winner,
    },
  ]

  const columns: Column<ScoreDetail>[] = [
    {
      key: 'backend',
      title: '实例',
      width: 140,
      sortValue: (s) => s.backend,
      render: (s) => (
        <span className="mono">
          {s.backend}
          {winner?.backend === s.backend && (
            <span className="badge ok" style={{ marginLeft: 8 }}>
              选中
            </span>
          )}
        </span>
      ),
    },
    {
      key: 'score',
      title: '代价分',
      align: 'right',
      sortValue: (s) => s.score,
      render: (s) => <span className="num">{num(s.score, 4)}</span>,
    },
    {
      key: 'result',
      title: '判定',
      render: (s) =>
        !s.available ? (
          <span className="badge muted">不可用</span>
        ) : s.err ? (
          <span className="badge err">表达式错误</span>
        ) : s.pass ? (
          <span className="badge ok">通过</span>
        ) : (
          <span className="badge warn">淘汰</span>
        ),
    },
    {
      key: 'prefix',
      title: '前缀命中',
      align: 'right',
      sortValue: (s) => s.prefix_match,
      render: (s) => <span className="num">{pct(s.prefix_match)}</span>,
    },
    {
      key: 'inflight',
      title: '在途',
      align: 'right',
      sortValue: (s) => s.inflight,
      render: (s) => <span className="num">{s.inflight}</span>,
    },
    {
      key: 'load',
      title: '运行 / 排队',
      align: 'right',
      sortValue: (s) => s.running,
      render: (s) => (
        <span className="num">
          {num(s.running)} / {num(s.waiting)}
        </span>
      ),
    },
    {
      key: 'kv',
      title: 'KV 占用',
      width: 150,
      sortValue: (s) => s.kv_usage,
      render: (s) => <UsageBar value={s.kv_usage} label="KV 占用" />,
    },
    {
      key: 'err',
      title: '备注',
      render: (s) => (s.err ? <span className="hint">{s.err}</span> : <span className="hint">—</span>),
    },
  ]

  return (
    <>
      <Panel
        title="路由决策"
        tools={
          winner ? (
            <span className="badge ok">
              <Icon name="check" size={12} />
              选中 {winner.backend}
            </span>
          ) : (
            <span className="badge err">无可用实例</span>
          )
        }
      >
        <div className="flow">
          {steps.map((s, i) => (
            <div
              key={s.title}
              className={`flow-step ${i === steps.length - 1 && s.done ? 'final' : s.done ? 'done' : ''}`}
            >
              <div className="flow-rail">
                <span className="flow-node">{i + 1}</span>
                {i < steps.length - 1 && <span className="flow-line" />}
              </div>
              <div className="flow-body">
                <h4>{s.title}</h4>
                <p>{s.detail}</p>
              </div>
            </div>
          ))}
        </div>
        {!winner && (
          <Notice kind="warn">
            没有实例同时满足「可用」与「通过过滤」。请检查实例健康状态、并发上限与 filter 表达式。
          </Notice>
        )}
      </Panel>

      <Panel title="逐实例代价明细" subtitle={`${result.scores.length} 个候选 · 分数越小越优`} flush>
        <DataTable
          columns={columns}
          rows={ranked}
          rowKey={(s) => s.backend}
          empty="该模型路由池内没有实例"
        />
      </Panel>
    </>
  )
}
