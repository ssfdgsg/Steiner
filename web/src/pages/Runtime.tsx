// 运行监控：集群请求趋势、PD 分离链路、金丝雀发布与集群成员。
// 只做观测与诊断，不混入实例注册或策略编辑（写操作集中在对应页面）。
import { useState } from 'react'
import { api, type PDGroup, type RolloutView } from '../api'
import { useInterval, usePoll, useRefreshControl, useToast } from '../hooks'
import {
  Async,
  ConfirmDialog,
  FeatureBadge,
  type Column,
  DataTable,
  MetricCard,
  Notice,
  Panel,
  Segmented,
  StateBlock,
  Toast,
} from '../components'
import { LineChart } from '../charts'
import { summarize, toPoint } from './Overview'
import { clock, ms, num, pct, stamp } from '../format'

const RANGES = [
  { value: 15, label: '15 分钟' },
  { value: 60, label: '1 小时' },
  { value: 180, label: '3 小时' },
  { value: 360, label: '6 小时' },
]

export function Runtime() {
  const { nonce } = useRefreshControl()
  const base = useInterval()
  const [minutes, setMinutes] = useState(60)

  const stats = usePoll(api.stats, base, nonce)
  const history = usePoll(() => api.history(minutes), base, nonce)
  const pd = usePoll(api.pd, base * 2, nonce)
  const cluster = usePoll(api.cluster, base * 2, nonce)
  const rollouts = usePoll(api.rollouts, base * 2, nonce)

  return (
    <>
      <p className="page-desc">
        网关侧观测视图。吞吐与时延取自内存采样缓冲（重启清空）；PD 链路与集群成员为即时状态。
      </p>

      <div className="kpis">
        <MetricCard
          label="累计请求"
          icon="overview"
          value={stats.data ? stats.data.aggregate.requests_total.toLocaleString('zh-CN') : '-'}
          foot={stats.data ? <span>错误率 {pct(stats.data.aggregate.error_rate, 2)}</span> : undefined}
        />
        <MetricCard
          label="可用后端"
          icon="server"
          tone="success"
          value={stats.data ? String(stats.data.backends.available) : '-'}
          unit={stats.data ? `/ ${stats.data.backends.total}` : undefined}
          foot={stats.data ? <span>在途请求 {stats.data.backends.inflight}</span> : undefined}
        />
        <MetricCard
          label="P95 延迟"
          icon="latency"
          tone="info"
          value={ms(stats.data?.aggregate.latency.p95_ms)}
          foot={
            stats.data ? (
              <span>
                均值 {ms(stats.data.aggregate.latency.avg_ms)} · TTFT P95{' '}
                {ms(stats.data.aggregate.ttft.p95_ms)}
              </span>
            ) : undefined
          }
        />
        <MetricCard
          label="队列深度"
          icon="queue"
          tone="warning"
          value={
            stats.data?.queue.enabled ? String(stats.data.queue.depth ?? 0) : '未启用'
          }
          foot={
            stats.data?.queue.enabled ? (
              <span>上限 {stats.data.queue.max_depth ?? '—'}</span>
            ) : (
              <span>容量排队未配置</span>
            )
          }
        />
      </div>

      <Panel
        title="吞吐与时延趋势"
        subtitle={history.data?.interval_seconds ? `采样间隔 ${history.data.interval_seconds}s` : undefined}
        tools={
          <Segmented
            ariaLabel="时间范围"
            value={minutes}
            options={RANGES}
            onChange={setMinutes}
          />
        }
      >
        <Async state={history}>
          {(d) =>
            !d.enabled ? (
              <StateBlock kind="disabled" title="时序采样未启用" />
            ) : (
              <div className="stack">
                <LineChart
                  height={260}
                  series={[
                    { name: 'RPS', color: 'var(--series-1)', points: (d.samples ?? []).map(toPoint('rps')) },
                  ]}
                  yFormat={(v) => num(v, 1)}
                  xFormat={(x) => clock(new Date(x).toISOString())}
                  summary={summarize(d.samples ?? [], 'rps', (v) => `${num(v, 2)} RPS`)}
                />
                <LineChart
                  height={220}
                  series={[
                    {
                      name: '平均时延',
                      color: 'var(--series-2)',
                      points: (d.samples ?? []).map(toPoint('latency_avg_ms')),
                    },
                    {
                      name: 'P95 时延',
                      color: 'var(--series-3)',
                      points: (d.samples ?? []).map(toPoint('latency_p95_ms')),
                    },
                  ]}
                  yFormat={(v) => `${Math.round(v)}ms`}
                  xFormat={(x) => clock(new Date(x).toISOString())}
                  summary={summarize(d.samples ?? [], 'latency_p95_ms', (v) => ms(v))}
                />
              </div>
            )
          }
        </Async>
      </Panel>

      <Panel title="PD 分离组与链路">
        <Async state={pd}>
          {(groups) => {
            const names = Object.keys(groups)
            if (!names.length)
              return (
                <StateBlock
                  kind="disabled"
                  title="未配置 PD 分离"
                  detail="Prefill/Decode 分离需要在配置中声明 pd_groups，拓扑不支持动态变更。"
                />
              )
            return (
              <div className="stack">
                {names.map((n) => (
                  <PDGroupBlock key={n} name={n} group={groups[n]} />
                ))}
              </div>
            )
          }}
        </Async>
      </Panel>

      <div className="cols-2">
        <Panel
          title="金丝雀发布"
          tools={<FeatureBadge enabled={rollouts.data?.enabled} />}
        >
          <Async state={rollouts}>
            {(d) =>
              !d.enabled ? (
                <StateBlock kind="disabled" title="未配置金丝雀发布" />
              ) : (
                <RolloutTable rollouts={d.rollouts ?? []} onDone={rollouts.refresh} />
              )
            }
          </Async>
        </Panel>

        <Panel title="集群成员" tools={<FeatureBadge enabled={cluster.data?.enabled} />}>
          <Async state={cluster}>
            {(d) =>
              !d.enabled ? (
                <StateBlock
                  kind="disabled"
                  title="单机部署"
                  detail="未启用集群协调，策略与后端变更仅作用于本实例。"
                />
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>实例</th>
                        <th>地址</th>
                        <th>角色</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(d.members ?? []).map((m) => (
                        <tr key={m.id}>
                          <td className="mono">
                            {m.id}
                            {m.id === d.self && (
                              <span className="badge accent" style={{ marginLeft: 8 }}>
                                本机
                              </span>
                            )}
                          </td>
                          <td className="mono">{m.addr ?? '—'}</td>
                          <td>
                            {m.leader ? (
                              <span className="badge ok">Leader</span>
                            ) : (
                              <span className="badge muted">Follower</span>
                            )}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )
            }
          </Async>
        </Panel>
      </div>
    </>
  )
}

function PDGroupBlock({ name, group }: { name: string; group: PDGroup }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="preset-row" style={{ flexDirection: 'column', alignItems: 'stretch' }}>
      <div className="row">
        <strong className="mono">{name}</strong>
        <span className="badge accent">{group.strategy || '默认策略'}</span>
        <span className="hint">策略槽位 {group.policy || 'default'}</span>
        <span className="hint">
          Prefill {group.prefill.length} · Decode {group.decode.length} · 链路{' '}
          {group.links?.length ?? 0}
        </span>
        <button className="btn sm ghost" style={{ marginLeft: 'auto' }} onClick={() => setOpen((v) => !v)}>
          {open ? '收起链路' : '展开链路'}
        </button>
      </div>
      {open &&
        (group.links?.length ? (
          <div className="table-wrap" style={{ marginTop: 10 }}>
            <table>
              <thead>
                <tr>
                  <th>Prefill</th>
                  <th>Decode</th>
                  <th style={{ textAlign: 'right' }}>带宽 Gbps</th>
                  <th style={{ textAlign: 'right' }}>在途 KV 传输</th>
                </tr>
              </thead>
              <tbody>
                {group.links.map((l) => (
                  <tr key={`${l.prefill}-${l.decode}`}>
                    <td className="mono">{l.prefill}</td>
                    <td className="mono">{l.decode}</td>
                    <td className="num" style={{ textAlign: 'right' }}>
                      {num(l.bandwidth_gbps, 1)}
                    </td>
                    <td className="num" style={{ textAlign: 'right' }}>
                      {l.inflight}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div style={{ marginTop: 10 }}>
            <Notice>该组暂无链路状态上报。</Notice>
          </div>
        ))}
    </div>
  )
}

function RolloutTable({ rollouts, onDone }: { rollouts: RolloutView[]; onDone: () => void }) {
  const { toast, show } = useToast()
  const [resetting, setResetting] = useState<RolloutView | null>(null)
  const [busy, setBusy] = useState(false)

  const columns: Column<RolloutView>[] = [
    { key: 'model', title: '模型', sortValue: (r) => r.model, render: (r) => <span className="mono">{r.model}</span> },
    { key: 'canary', title: '金丝雀池', render: (r) => <span className="mono">{r.canary}</span> },
    {
      key: 'state',
      title: '状态',
      render: (r) => {
        const cls =
          r.state === 'failed' ? 'err' : r.state === 'completed' ? 'ok' : r.state === 'running' ? 'accent' : 'muted'
        return <span className={`badge ${cls}`}>{r.state}</span>
      },
    },
    {
      key: 'step',
      title: '阶段',
      align: 'right',
      render: (r) => (
        <span className="num">
          {r.step_index + 1} / {r.steps?.length || 1}
        </span>
      ),
    },
    {
      key: 'weight',
      title: '分流权重',
      align: 'right',
      sortValue: (r) => r.canary_weight,
      render: (r) => <span className="num">{pct(r.canary_weight)}</span>,
    },
    {
      key: 'err',
      title: '错误率 金丝雀/稳定',
      align: 'right',
      render: (r) => (
        <span className="num">
          {pct(r.canary_error_rate, 2)} / {pct(r.stable_error_rate, 2)}
        </span>
      ),
    },
    {
      key: 'ttft',
      title: 'TTFT P95 金丝雀/稳定',
      align: 'right',
      render: (r) => (
        <span className="num">
          {ms(r.canary_ttft_p95 * 1000)} / {ms(r.stable_ttft_p95 * 1000)}
        </span>
      ),
    },
    { key: 'since', title: '阶段开始', render: (r) => <span className="hint">{stamp(r.step_since)}</span> },
    {
      key: 'actions',
      title: '操作',
      align: 'right',
      render: (r) => (
        <span className="cell-actions">
          <button
            className="btn sm"
            onClick={(e) => {
              e.stopPropagation()
              setResetting(r)
            }}
          >
            重新发布
          </button>
        </span>
      ),
    },
  ]

  return (
    <>
      <DataTable columns={columns} rows={rollouts} rowKey={(r) => r.model} empty="暂无发布计划" />
      {resetting && (
        <ConfirmDialog
          title="重新开始金丝雀发布"
          confirmText="确认重置"
          busy={busy}
          onCancel={() => setResetting(null)}
          onConfirm={async () => {
            setBusy(true)
            try {
              await api.resetRollout(resetting.model)
              show('ok', `${resetting.model} 已从第一阶段重新发布`)
              setResetting(null)
              onDone()
            } catch (e) {
              show('err', e instanceof Error ? e.message : String(e))
            } finally {
              setBusy(false)
            }
          }}
        >
          <div>
            模型 <code className="mono">{resetting.model}</code> 的发布将回到第一阶段，分流权重按首个阶段重设。
          </div>
          <div>权重是每实例内存态，集群下各网关实例会各自按同一份发布配置重新推进。</div>
        </ConfirmDialog>
      )}
      <Toast toast={toast} />
    </>
  )
}
