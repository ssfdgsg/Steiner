// 调度方案：展示各策略槽位当前生效方案，支持一键切换与表达式手工热更新。
import { useState } from 'react'
import { api, type Preset, type PresetsResp } from '../api'
import { useInterval, usePoll, useRefreshControl, useToast } from '../hooks'
import { Async, ConfirmDialog, Panel, StateBlock, Toast } from '../components'
import { PolicyEditor } from '../components/PolicyEditor'
import { Icon } from '../icons'

export function Presets() {
  const { nonce } = useRefreshControl()
  const state = usePoll(api.presets, useInterval(2), nonce)
  const { toast, show } = useToast()
  const [target, setTarget] = useState('default')
  const [pending, setPending] = useState<Preset | null>(null)
  const [busy, setBusy] = useState(false)

  const apply = async () => {
    if (!pending) return
    setBusy(true)
    try {
      await api.applyPreset(pending.name, target)
      show('ok', `已将「${pending.title}」应用到策略槽位 ${target}`)
      setPending(null)
      state.refresh()
    } catch (e) {
      show('err', e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <p className="page-desc">
        调度方案决定请求在候选实例间的过滤与打分方式。切换会立即生效于所选策略槽位，并同步到持久层与集群其余实例。
      </p>

      <Async state={state} skeletonRows={6}>
        {(d) => (
          <PresetsBody
            data={d}
            target={target}
            onTarget={setTarget}
            onPick={setPending}
            onSaved={(msg) => {
              show('ok', msg)
              state.refresh()
            }}
            onError={(msg) => show('err', msg)}
          />
        )}
      </Async>

      {pending && (
        <ConfirmDialog
          title="切换调度方案"
          confirmText="确认切换"
          busy={busy}
          onCancel={() => setPending(null)}
          onConfirm={apply}
        >
          <div>
            即将把策略槽位 <code className="mono">{target}</code> 的过滤与打分表达式替换为「
            {pending.title}」。
          </div>
          <div>
            该操作对使用此槽位的全部模型路由立即生效，并广播到集群其余网关实例。原表达式不会自动保存，如需回退请重新选择方案或手工填写。
          </div>
          <pre className="expr">{pending.score}</pre>
        </ConfirmDialog>
      )}
      <Toast toast={toast} />
    </>
  )
}

function PresetsBody({
  data,
  target,
  onTarget,
  onPick,
  onSaved,
  onError,
}: {
  data: PresetsResp
  target: string
  onTarget: (v: string) => void
  onPick: (p: Preset) => void
  onSaved: (msg: string) => void
  onError: (msg: string) => void
}) {
  const slots = Object.keys(data.policies)
  const current = data.policies[target]
  const activePreset = current ? data.presets.find((p) => p.name === current.preset) : undefined

  return (
    <>
      <Panel
        title="当前生效方案"
        tools={
          <label className="switch">
            策略槽位
            <select
              aria-label="策略槽位"
              style={{ width: 140 }}
              value={target}
              onChange={(e) => onTarget(e.target.value)}
            >
              {slots.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </label>
        }
      >
        {!current ? (
          <StateBlock title={`策略槽位 ${target} 不存在`} detail="请在下拉框中选择已配置的槽位。" />
        ) : (
          <div className="stack">
            <div className="row">
              {activePreset ? (
                <span className="badge ok">
                  <Icon name="check" size={12} />
                  {activePreset.title}
                </span>
              ) : (
                <span className="badge accent">自定义表达式</span>
              )}
              <span className="hint">
                {activePreset?.description ?? '当前表达式与任何内置方案都不匹配，由手工热更新写入。'}
              </span>
            </div>
            <div className="cols-2">
              <div>
                <label>过滤表达式 filter</label>
                <pre className="expr">{current.filter || '（空：不过滤候选实例）'}</pre>
              </div>
              <div>
                <label>打分表达式 score</label>
                <pre className="expr">{current.score}</pre>
              </div>
            </div>
          </div>
        )}
      </Panel>

      <Panel title="内置方案" subtitle={`应用到 ${target}`}>
        <div className="preset-list">
          {data.presets.map((p) => {
            const active = current?.preset === p.name
            return (
              <div className={active ? 'preset-row active' : 'preset-row'} key={p.name}>
                <div className="meta">
                  <h3>
                    {p.title}
                    <code className="mono hint">{p.name}</code>
                    {active && <span className="badge ok">已生效</span>}
                  </h3>
                  <p>{p.description}</p>
                  <pre className="expr" style={{ marginTop: 8 }}>
                    {p.score}
                  </pre>
                </div>
                <button className="btn primary" disabled={active} onClick={() => onPick(p)}>
                  {active ? '当前方案' : '应用'}
                </button>
              </div>
            )
          })}
        </div>
      </Panel>

      {current && (
        <PolicyEditor
          key={target}
          target={target}
          filter={current.filter}
          score={current.score}
          onSaved={onSaved}
          onError={onError}
        />
      )}
    </>
  )
}
