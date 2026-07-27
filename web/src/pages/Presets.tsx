// 调度方案页：内置方案一键切换 + 手写表达式编辑。
import { useEffect, useState } from 'react'
import { api, type PresetsResp } from '../api'
import { usePoll, useToast } from '../hooks'
import { Toast } from '../components'

export function Presets() {
  const { data, error, loading, refresh } = usePoll<PresetsResp>(() => api.presets(), 5000)
  const { toast, show } = useToast()
  // target 切换目标策略槽位；默认 default，可选其余已存在的策略。
  const [target, setTarget] = useState('default')
  const [applying, setApplying] = useState<string | null>(null)

  // 手写表达式编辑器状态：切换槽位或远端变更时同步，编辑中不覆盖用户输入。
  const [editing, setEditing] = useState(false)
  const [filter, setFilter] = useState('')
  const [score, setScore] = useState('')
  const current = data?.policies[target]

  useEffect(() => {
    if (!editing && current) {
      setFilter(current.filter)
      setScore(current.score)
    }
  }, [current, editing])

  const apply = async (name: string) => {
    setApplying(name)
    try {
      await api.applyPreset(name, target)
      show('ok', `已切换到「${data?.presets.find((p) => p.name === name)?.title ?? name}」`)
      setEditing(false)
      refresh()
    } catch (e) {
      show('err', e instanceof Error ? e.message : String(e))
    } finally {
      setApplying(null)
    }
  }

  const saveCustom = async () => {
    try {
      await api.putPolicy(target, filter, score)
      show('ok', '表达式已生效')
      setEditing(false)
      refresh()
    } catch (e) {
      show('err', e instanceof Error ? e.message : String(e))
    }
  }

  if (loading) return <div className="empty">加载中…</div>
  if (error) return <div className="empty">加载失败：{error}</div>
  if (!data) return null

  const policyNames = Object.keys(data.policies).sort()

  return (
    <>
      <div className="page-head">
        <div>
          <h1>调度方案</h1>
          <p className="page-desc">
            一键切换调度算式，运行期立即生效（编译校验 → 持久化 → 集群广播），无需重启。
          </p>
        </div>
        <div className="row">
          <label style={{ margin: 0 }}>目标策略槽位</label>
          <select
            value={target}
            onChange={(e) => {
              setTarget(e.target.value)
              setEditing(false)
            }}
            style={{ width: 'auto' }}
          >
            {policyNames.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </div>
      </div>

      <div className="card">
        <h2>
          当前生效：
          {current?.preset === 'custom' ? (
            <span className="badge warn">自定义表达式</span>
          ) : (
            <span className="badge accent">
              {data.presets.find((p) => p.name === current?.preset)?.title ?? current?.preset}
            </span>
          )}
        </h2>
        <div className="field">
          <label>过滤表达式 filter（返回 bool，false 淘汰该后端）</label>
          <textarea
            rows={2}
            value={filter}
            onChange={(e) => {
              setFilter(e.target.value)
              setEditing(true)
            }}
          />
        </div>
        <div className="field">
          <label>打分表达式 score（数值，分数最小者胜出）</label>
          <textarea
            rows={3}
            value={score}
            onChange={(e) => {
              setScore(e.target.value)
              setEditing(true)
            }}
          />
        </div>
        <div className="row">
          <button className="primary" onClick={saveCustom} disabled={!editing}>
            保存自定义表达式
          </button>
          {editing && (
            <button
              onClick={() => {
                setEditing(false)
                if (current) {
                  setFilter(current.filter)
                  setScore(current.score)
                }
              }}
            >
              放弃修改
            </button>
          )}
          <span className="hint">
            可用变量：running / waiting / kv_usage / inflight / prefix_match / hit_rate /
            gen_tps / ttft_ewma / preempt_rate / weight / prompt_len / raw[...] / vars[...]
          </span>
        </div>
      </div>

      <h2 style={{ fontSize: 15, margin: '22px 0 12px' }}>内置方案</h2>
      <div className="grid">
        {data.presets.map((p) => {
          const active = current?.preset === p.name
          return (
            <div key={p.name} className={active ? 'preset-card active' : 'preset-card'}>
              <div className="preset-title">
                {p.title}
                {active && <span className="badge accent">生效中</span>}
              </div>
              <div className="preset-desc">{p.description}</div>
              <div className="expr">filter: {p.filter}</div>
              <div className="expr">score: {p.score}</div>
              <div className="row">
                <button
                  className={active ? '' : 'primary'}
                  disabled={active || applying !== null}
                  onClick={() => apply(p.name)}
                >
                  {active ? '当前方案' : applying === p.name ? '切换中…' : `应用到 ${target}`}
                </button>
              </div>
            </div>
          )
        })}
      </div>

      <Toast toast={toast} />
    </>
  )
}
