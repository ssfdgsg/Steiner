// KV Cache 与请求队列：缓存规模与效率、按模型的缓存与排队分布、后端缓存明细。
import { api } from '../api'
import { useInterval, usePoll, useRefreshControl } from '../hooks'
import {
  Async,
  FeatureBadge,
  MetricCard,
  Notice,
  Panel,
  StateBlock,
  UsageBar,
} from '../components'
import { BarChart, LineChart } from '../charts'
import { summarize, toPoint } from './Overview'
import { ago, bytes, clock, compact, num, pct } from '../format'

export function CacheQueue() {
  const { nonce } = useRefreshControl()
  const base = useInterval()
  const kv = usePoll(api.kvcache, base * 2, nonce)
  const queue = usePoll(api.queue, base, nonce)
  const backends = usePoll(api.backends, base, nonce)
  const models = usePoll(api.models, base * 3, nonce)
  const history = usePoll(() => api.history(60), base * 2, nonce)

  const pool = backends.data ?? []
  const withSnapshot = pool.filter((b) => b.snapshot && !b.snapshot.err)
  const avgHit = withSnapshot.length
    ? withSnapshot.reduce((s, b) => s + (b.snapshot?.hit_rate ?? 0), 0) / withSnapshot.length
    : undefined
  const avgKV = withSnapshot.length
    ? withSnapshot.reduce((s, b) => s + (b.snapshot?.kv_usage ?? 0), 0) / withSnapshot.length
    : undefined

  return (
    <>
      <p className="page-desc">
        网关侧 KV 前缀树用于缓存亲和选路；各实例的 KV 占用与前缀命中率来自引擎指标直采。容量排队为网关准入控制，与引擎内部排队（运行/等待）是两层不同队列。
      </p>

      <div className="kpis">
        <MetricCard
          label="前缀树占用"
          icon="cache"
          hint="网关内存中前缀树的字节数，超过上限由后台清理任务按 LRU 淘汰"
          value={kv.data?.enabled ? bytes(kv.data.stats?.bytes) : '未启用'}
          foot={
            kv.data?.enabled ? (
              <span>节点 {compact(kv.data.stats?.nodes)}</span>
            ) : (
              <span>kvcache.enabled = false</span>
            )
          }
        />
        <MetricCard
          label="平均前缀命中率"
          icon="gauge"
          tone="success"
          hint="各后端引擎上报命中率的算术平均"
          value={pct(avgHit)}
          foot={<span>取自 {withSnapshot.length} / {pool.length} 个实例快照</span>}
        />
        <MetricCard
          label="平均 KV 占用"
          icon="server"
          tone="warning"
          value={pct(avgKV)}
          foot={<span>超过 90% 将影响新请求准入</span>}
        />
        <MetricCard
          label="队列深度"
          icon="queue"
          tone="info"
          value={queue.data?.enabled ? String(queue.data.depth ?? 0) : '未启用'}
          foot={
            queue.data?.enabled ? (
              <span>
                上限 {queue.data.max_depth ?? '—'} · 最长等待 {queue.data.max_wait ?? '—'}
              </span>
            ) : (
              <span>queue.enabled = false</span>
            )
          }
        />
      </div>

      <Panel title="缓存与队列趋势" subtitle="近 1 小时">
        <Async state={history}>
          {(d) =>
            !d.enabled ? (
              <StateBlock kind="disabled" title="时序采样未启用" />
            ) : (
              <div className="cols-2">
                <LineChart
                  height={210}
                  series={[
                    {
                      name: '前缀命中率',
                      color: 'var(--series-2)',
                      points: (d.samples ?? []).map(toPoint('hit_rate')),
                    },
                    {
                      name: 'KV 占用',
                      color: 'var(--series-3)',
                      points: (d.samples ?? []).map(toPoint('kv_usage')),
                    },
                  ]}
                  yFormat={(v) => pct(v, 0)}
                  xFormat={(x) => clock(new Date(x).toISOString())}
                  summary={summarize(d.samples ?? [], 'hit_rate', (v) => pct(v))}
                />
                <LineChart
                  height={210}
                  series={[
                    {
                      name: '队列深度',
                      color: 'var(--series-1)',
                      points: (d.samples ?? []).map(toPoint('queue_depth')),
                    },
                  ]}
                  yFormat={(v) => String(Math.round(v))}
                  xFormat={(x) => clock(new Date(x).toISOString())}
                  summary={summarize(d.samples ?? [], 'queue_depth', (v) => `${Math.round(v)} 个`)}
                />
              </div>
            )
          }
        </Async>
      </Panel>

      <div className="cols-7-5">
        <Panel title="后端缓存明细" flush>
          <Async state={backends} skeletonRows={5}>
            {(list) =>
              list.length === 0 ? (
                <StateBlock title="尚未注册后端实例" />
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>实例</th>
                        <th>引擎</th>
                        <th style={{ minWidth: 150 }}>KV 占用</th>
                        <th style={{ textAlign: 'right' }}>前缀命中</th>
                        <th style={{ textAlign: 'right' }}>运行 / 排队</th>
                        <th>快照</th>
                      </tr>
                    </thead>
                    <tbody>
                      {list.map((b) => (
                        <tr key={b.id}>
                          <td className="mono">{b.id}</td>
                          <td className="mono">{b.engine}</td>
                          <td>
                            <UsageBar value={b.snapshot?.kv_usage} label="KV 占用" />
                          </td>
                          <td className="num" style={{ textAlign: 'right' }}>
                            {pct(b.snapshot?.hit_rate)}
                          </td>
                          <td className="num" style={{ textAlign: 'right' }}>
                            {num(b.snapshot?.running)} / {num(b.snapshot?.waiting)}
                          </td>
                          <td>
                            {b.snapshot?.err ? (
                              <span className="badge err" title={b.snapshot.err}>
                                采集失败
                              </span>
                            ) : (
                              <span className="hint">{ago(b.snapshot?.time)}</span>
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

        <div className="stack">
          <Panel title="按模型缓存命中率" subtitle="池内实例均值">
            <Async state={models}>
              {(list) => (
                <BarChart
                  height={200}
                  data={list.map((m) => ({ label: m.name, value: Number((m.hit_rate * 100).toFixed(1)) }))}
                  yFormat={(v) => `${Math.round(v)}%`}
                  empty="未配置模型路由"
                />
              )}
            </Async>
          </Panel>

          <Panel title="按模型排队深度" tools={<FeatureBadge enabled={queue.data?.enabled} />}>
            <Async state={queue}>
              {(d) => {
                if (!d.enabled)
                  return (
                    <StateBlock
                      kind="disabled"
                      title="容量排队未启用"
                      detail="开启 queue.enabled 后，超出容量的请求将排队而非直接拒绝。"
                    />
                  )
                const rows = Object.entries(d.by_model ?? {})
                if (!rows.length) return <StateBlock title="当前没有排队请求" />
                return (
                  <div className="stack" style={{ gap: 10 }}>
                    {rows.map(([model, depth]) => (
                      <div key={model}>
                        <div className="row" style={{ justifyContent: 'space-between' }}>
                          <span className="mono">{model}</span>
                          <span className="num">{depth}</span>
                        </div>
                        <UsageBar
                          value={d.max_depth ? depth / d.max_depth : 0}
                          label={`${model} 队列占用`}
                        />
                      </div>
                    ))}
                  </div>
                )
              }}
            </Async>
          </Panel>
        </div>
      </div>

      <Notice>
        请求处理阶段的完整流水线（已完成请求数、各阶段耗时分解）当前未由管理接口提供，相关统计请查
        Prometheus 的 gateway_requests_total 与 gateway_request_duration_seconds。
      </Notice>
    </>
  )
}
