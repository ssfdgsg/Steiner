// 后端实例：实时负载对比、详情排障与注册/隔离/摘除等运维操作。
import { useMemo, useState } from 'react'
import { api, type BackendView } from '../api'
import { useInterval, usePoll, useRefreshControl, useToast } from '../hooks'
import {
  Async,
  ConfirmDialog,
  type Column,
  DataTable,
  Drawer,
  DrawerSection,
  KVList,
  Notice,
  Panel,
  StatusBadge,
  Toast,
  UsageBar,
} from '../components'
import { Icon } from '../icons'
import { ago, num, pct, stamp } from '../format'

const ENGINES = [
  { value: 'vllm', label: 'vLLM' },
  { value: 'vllm_omni', label: 'vLLM-Omni' },
  { value: 'sglang', label: 'SGLang' },
  { value: 'sglang_omni', label: 'SGLang-Omni' },
]

type StatusFilter = 'all' | 'available' | 'abnormal'

/** mainState 后端主状态：与 StatusBadge 的优先级保持一致，供筛选与排序复用。 */
function mainState(b: BackendView): 'available' | 'cordoned' | 'ejected' | 'unhealthy' {
  if (b.cordoned) return 'cordoned'
  if (b.ejected) return 'ejected'
  if (!b.healthy) return 'unhealthy'
  return 'available'
}

export function Backends() {
  const { nonce } = useRefreshControl()
  const state = usePoll(api.backends, useInterval(), nonce)
  const models = usePoll(api.models, useInterval(6), nonce)
  const { toast, show } = useToast()

  const [q, setQ] = useState('')
  const [status, setStatus] = useState<StatusFilter>('all')
  const [engine, setEngine] = useState('all')
  const [selected, setSelected] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [removing, setRemoving] = useState<BackendView | null>(null)
  const [busy, setBusy] = useState<string | null>(null)

  const all = state.data ?? []
  const rows = useMemo(() => {
    const kw = q.trim().toLowerCase()
    return all.filter((b) => {
      if (kw && !`${b.id} ${b.url} ${b.engine}`.toLowerCase().includes(kw)) return false
      if (engine !== 'all' && b.engine !== engine) return false
      if (status === 'available' && mainState(b) !== 'available') return false
      if (status === 'abnormal' && mainState(b) === 'available') return false
      return true
    })
  }, [all, q, engine, status])

  const current = all.find((b) => b.id === selected) ?? null

  const act = async (id: string, fn: () => Promise<unknown>, okText: string) => {
    setBusy(id)
    try {
      await fn()
      show('ok', okText)
      state.refresh()
    } catch (e) {
      show('err', e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  const columns: Column<BackendView>[] = [
    {
      key: 'id',
      title: '实例 ID',
      width: 150,
      sortValue: (b) => b.id,
      render: (b) => <span className="mono">{b.id}</span>,
    },
    {
      key: 'state',
      title: '状态',
      width: 96,
      sortValue: (b) => mainState(b),
      render: (b) => <StatusBadge healthy={b.healthy} cordoned={b.cordoned} ejected={b.ejected} />,
    },
    {
      key: 'engine',
      title: '引擎',
      sortValue: (b) => b.engine,
      render: (b) => <span className="mono">{b.engine}</span>,
    },
    {
      key: 'weight',
      title: '权重',
      align: 'right',
      sortValue: (b) => b.weight,
      render: (b) => <span className="num">{num(b.weight)}</span>,
    },
    {
      key: 'inflight',
      title: '在途',
      align: 'right',
      sortValue: (b) => b.inflight,
      render: (b) => <span className="num">{b.inflight}</span>,
    },
    {
      key: 'running',
      title: '运行 / 排队',
      align: 'right',
      sortValue: (b) => b.snapshot?.running ?? 0,
      render: (b) => (
        <span className="num">
          {num(b.snapshot?.running)} / {num(b.snapshot?.waiting)}
        </span>
      ),
    },
    {
      key: 'kv',
      title: 'KV 占用',
      width: 150,
      sortValue: (b) => b.snapshot?.kv_usage ?? 0,
      render: (b) => <UsageBar value={b.snapshot?.kv_usage} label="KV 占用" />,
    },
    {
      key: 'hit',
      title: '前缀命中',
      align: 'right',
      sortValue: (b) => b.snapshot?.hit_rate ?? 0,
      render: (b) => <span className="num">{pct(b.snapshot?.hit_rate)}</span>,
    },
    {
      key: 'tps',
      title: '吞吐 tok/s',
      align: 'right',
      sortValue: (b) => b.snapshot?.gen_tok_per_sec ?? 0,
      render: (b) => <span className="num">{num(b.snapshot?.gen_tok_per_sec, 1)}</span>,
    },
    {
      key: 'snapshot',
      title: '快照',
      sortValue: (b) => b.snapshot?.time ?? '',
      render: (b) =>
        b.snapshot?.err ? (
          <span className="badge err" title={b.snapshot.err}>
            采集失败
          </span>
        ) : (
          <span className="hint">{ago(b.snapshot?.time)}</span>
        ),
    },
    {
      key: 'actions',
      title: '操作',
      align: 'right',
      width: 150,
      render: (b) => (
        <span className="cell-actions">
          <button
            className="btn sm"
            disabled={busy === b.id}
            onClick={(e) => {
              e.stopPropagation()
              void act(
                b.id,
                () => api.cordon(b.id, !b.cordoned),
                b.cordoned ? `已恢复 ${b.id} 的调度` : `已隔离 ${b.id}，不再参与选路`,
              )
            }}
          >
            {b.cordoned ? '恢复' : '隔离'}
          </button>
          <button
            className="btn sm danger"
            disabled={busy === b.id}
            onClick={(e) => {
              e.stopPropagation()
              setRemoving(b)
            }}
          >
            <Icon name="trash" size={13} />
          </button>
        </span>
      ),
    },
  ]

  return (
    <>
      <p className="page-desc">
        实例负载来自引擎指标直采（运行中、排队、KV 占用、前缀命中、生成吞吐）。隔离仅停止新请求选路，在途请求正常完成；摘除会同时更新持久层。
      </p>

      <Panel
        title="推理后端实例"
        subtitle={`${rows.length} / ${all.length} 个`}
        flush
        tools={
          <button className="btn primary" onClick={() => setAdding(true)}>
            <Icon name="plus" size={14} />
            注册实例
          </button>
        }
      >
        <div className="filterbar">
          <input
            className="search"
            placeholder="搜索实例 ID / 地址 / 引擎"
            aria-label="搜索实例"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          <select
            aria-label="状态筛选"
            style={{ width: 130 }}
            value={status}
            onChange={(e) => setStatus(e.target.value as StatusFilter)}
          >
            <option value="all">全部状态</option>
            <option value="available">仅可用</option>
            <option value="abnormal">仅异常</option>
          </select>
          <select
            aria-label="引擎筛选"
            style={{ width: 150 }}
            value={engine}
            onChange={(e) => setEngine(e.target.value)}
          >
            <option value="all">全部引擎</option>
            {ENGINES.map((e) => (
              <option key={e.value} value={e.value}>
                {e.label}
              </option>
            ))}
          </select>
          <span className="hint" style={{ marginLeft: 'auto' }}>
            点击行查看实例详情
          </span>
        </div>
        <Async state={state} skeletonRows={6}>
          {() => (
            <DataTable
              columns={columns}
              rows={rows}
              rowKey={(b) => b.id}
              selectedKey={selected}
              onSelect={(b) => setSelected(b.id)}
              empty={all.length ? '当前筛选条件下没有实例' : '尚未注册任何后端实例'}
            />
          )}
        </Async>
      </Panel>

      {current && (
        <Drawer title={current.id} subtitle={current.url} onClose={() => setSelected(null)}>
          <DrawerSection title="状态">
            <KVList
              items={[
                [
                  '主状态',
                  <StatusBadge
                    healthy={current.healthy}
                    cordoned={current.cordoned}
                    ejected={current.ejected}
                  />,
                ],
                ['健康探针', current.healthy ? '通过' : '失败'],
                ['隔离', current.cordoned ? '是（不参与选路）' : '否'],
                ['熔断', current.ejected ? '是（错误率触发）' : '否'],
                ['引擎', <span className="mono">{current.engine}</span>],
                ['权重', num(current.weight)],
                ['所属模型', modelsOf(models.data, current.id) || '—'],
              ]}
            />
          </DrawerSection>

          <DrawerSection title="负载">
            <KVList
              items={[
                ['在途请求', String(current.inflight)],
                ['引擎运行中', num(current.snapshot?.running)],
                ['引擎排队', num(current.snapshot?.waiting)],
                ['KV 占用', <UsageBar value={current.snapshot?.kv_usage} label="KV 占用" />],
                ['前缀命中率', pct(current.snapshot?.hit_rate)],
                ['生成吞吐', `${num(current.snapshot?.gen_tok_per_sec, 1)} tok/s`],
                ['快照时间', stamp(current.snapshot?.time)],
              ]}
            />
            {current.snapshot?.err && (
              <div style={{ marginTop: 10 }}>
                <Notice kind="err">指标采集失败：{current.snapshot.err}</Notice>
              </div>
            )}
          </DrawerSection>

          {current.labels && Object.keys(current.labels).length > 0 && (
            <DrawerSection title="标签">
              <KVList items={Object.entries(current.labels).map(([k, v]) => [k, v])} />
            </DrawerSection>
          )}

          {current.prom_vars && Object.keys(current.prom_vars).length > 0 && (
            <DrawerSection title="Prometheus 变量">
              <KVList
                items={Object.entries(current.prom_vars).map(([k, v]) => [k, num(v, 3)])}
              />
            </DrawerSection>
          )}

          <DrawerSection title="操作">
            <div className="row">
              <button
                className="btn"
                disabled={busy === current.id}
                onClick={() =>
                  void act(
                    current.id,
                    () => api.cordon(current.id, !current.cordoned),
                    current.cordoned ? `已恢复 ${current.id} 的调度` : `已隔离 ${current.id}`,
                  )
                }
              >
                {current.cordoned ? '恢复调度' : '隔离实例'}
              </button>
              <button className="btn danger" onClick={() => setRemoving(current)}>
                摘除实例
              </button>
            </div>
          </DrawerSection>
        </Drawer>
      )}

      {removing && (
        <ConfirmDialog
          title="摘除后端实例"
          confirmText="确认摘除"
          danger
          busy={busy === removing.id}
          onCancel={() => setRemoving(null)}
          onConfirm={async () => {
            const id = removing.id
            await act(id, () => api.removeBackend(id), `已摘除实例 ${id}`)
            setRemoving(null)
            if (selected === id) setSelected(null)
          }}
        >
          <div>
            即将从注册表与持久层移除实例 <code className="mono">{removing.id}</code>（
            {removing.url}）。
          </div>
          <div>
            在途请求会正常完成，但该实例不再被选中。恢复需重新注册；若实例属于 PD 组，网关会拒绝该操作。
          </div>
        </ConfirmDialog>
      )}

      {adding && (
        <AddBackendDialog
          modelNames={(models.data ?? []).filter((m) => !m.pd_group).map((m) => m.name)}
          onClose={() => setAdding(false)}
          onDone={(msg) => {
            show('ok', msg)
            setAdding(false)
            state.refresh()
          }}
          onError={(msg) => show('err', msg)}
        />
      )}

      <Toast toast={toast} />
    </>
  )
}

function modelsOf(models: Array<{ name: string; backends: string[] }> | null, id: string): string {
  if (!models) return ''
  return models
    .filter((m) => m.backends.includes(id))
    .map((m) => m.name)
    .join(', ')
}

/** AddBackendDialog 动态注册实例。models 为必填项：新实例必须加入至少一个非 PD 模型路由。 */
function AddBackendDialog({
  modelNames,
  onClose,
  onDone,
  onError,
}: {
  modelNames: string[]
  onClose: () => void
  onDone: (msg: string) => void
  onError: (msg: string) => void
}) {
  const [id, setId] = useState('')
  const [url, setUrl] = useState('http://')
  const [engine, setEngine] = useState('vllm')
  const [weight, setWeight] = useState('1')
  const [maxConc, setMaxConc] = useState('0')
  const [models, setModels] = useState<string[]>(modelNames.slice(0, 1))
  const [busy, setBusy] = useState(false)

  const valid = id.trim() !== '' && /^https?:\/\/.+/.test(url.trim()) && models.length > 0

  const submit = async () => {
    setBusy(true)
    try {
      await api.addBackend({
        id: id.trim(),
        url: url.trim(),
        engine,
        weight: Number(weight) || 0,
        max_concurrency: Number(maxConc) || 0,
        models,
      })
      onDone(`实例 ${id.trim()} 已注册`)
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <ConfirmDialog
      title="注册推理后端"
      confirmText="注册"
      busy={busy}
      onCancel={onClose}
      onConfirm={() => {
        if (valid) void submit()
      }}
    >
      <div className="form-grid">
        <div className="field">
          <label htmlFor="nb-id">实例 ID（必填，全局唯一）</label>
          <input id="nb-id" value={id} onChange={(e) => setId(e.target.value)} placeholder="vllm-05" />
        </div>
        <div className="field">
          <label htmlFor="nb-url">地址（必填，http/https）</label>
          <input id="nb-url" value={url} onChange={(e) => setUrl(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="nb-engine">推理引擎</label>
          <select id="nb-engine" value={engine} onChange={(e) => setEngine(e.target.value)}>
            {ENGINES.map((e) => (
              <option key={e.value} value={e.value}>
                {e.label}
              </option>
            ))}
          </select>
        </div>
        <div className="field">
          <label htmlFor="nb-weight">权重</label>
          <input id="nb-weight" value={weight} onChange={(e) => setWeight(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="nb-conc">并发上限（0 表示不限）</label>
          <input id="nb-conc" value={maxConc} onChange={(e) => setMaxConc(e.target.value)} />
        </div>
      </div>
      <div className="field">
        <label htmlFor="nb-models">加入的模型路由（必填，可多选）</label>
        {modelNames.length === 0 ? (
          <Notice kind="warn">当前没有可加入的非 PD 模型路由，无法动态注册实例。</Notice>
        ) : (
          <select
            id="nb-models"
            multiple
            size={Math.min(5, modelNames.length)}
            value={models}
            onChange={(e) =>
              setModels(Array.from(e.target.selectedOptions).map((o) => o.value))
            }
          >
            {modelNames.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        )}
      </div>
      {!valid && <span className="hint">请填写实例 ID、合法地址并至少选择一个模型路由。</span>}
    </ConfirmDialog>
  )
}
