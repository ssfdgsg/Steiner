// 总览：跨调度、实例、性能、缓存与告警的一屏健康摘要，控制台默认页。
import { api, type Sample } from '../api'
import { useInterval, usePoll, useRefreshControl } from '../hooks'
import { Async, MetricCard, Panel, SeverityBadge, StateBlock, severityOf } from '../components'
import { BarChart, LineChart, Sparkline } from '../charts'
import { ago, clock, compact, ms, num, pct } from '../format'

export function Overview() {
  const { nonce } = useRefreshControl()
  const base = useInterval()
  const stats = usePoll(api.stats, base, nonce)
  const history = usePoll(() => api.history(30), base, nonce)
  const models = usePoll(api.models, base * 3, nonce)
  const presets = usePoll(api.presets, base * 3, nonce)
  const alerts = usePoll(api.alerts, base, nonce)

  const samples = history.data?.samples ?? []
  const recent = samples.slice(-40)

  return (
    <>
      <p className="page-desc">
        网关运行状态汇总。KPI 为进程启动以来的累计口径，趋势图取自网关内存采样缓冲（约 6
        小时），长周期时序请查 Prometheus。
      </p>

      <div className="kpis">
        <MetricCard
          label="累计请求数"
          icon="overview"
          hint="进程启动以来经网关转发的请求总数，与 /metrics 的 gateway_requests_total 同源"
          value={compact(stats.data?.aggregate.requests_total)}
          foot={
            stats.data ? (
              <>
                <span>平均 {num(stats.data.avg_rps ?? 0, 2)} RPS</span>
                {recent.length > 1 && <Sparkline values={recent.map((s) => s.rps)} />}
              </>
            ) : undefined
          }
        />
        <MetricCard
          label="可用实例"
          icon="server"
          tone="success"
          hint="健康且未被隔离、未被熔断的后端实例数 / 已注册实例总数"
          value={stats.data ? `${stats.data.backends.available}` : '-'}
          unit={stats.data ? `/ ${stats.data.backends.total}` : undefined}
          foot={
            stats.data ? (
              <span>
                隔离 {stats.data.backends.cordoned} · 熔断 {stats.data.backends.ejected}
              </span>
            ) : undefined
          }
        />
        <MetricCard
          label="平均延迟"
          icon="latency"
          tone="info"
          hint="整请求时延均值，分位数由 Prometheus 直方图桶插值估算"
          value={ms(stats.data?.aggregate.latency.avg_ms)}
          foot={
            stats.data ? (
              <span>
                P95 {ms(stats.data.aggregate.latency.p95_ms)} · 样本{' '}
                {compact(stats.data.aggregate.latency.count)}
              </span>
            ) : undefined
          }
        />
        <MetricCard
          label="缓存命中率"
          icon="cache"
          tone="warning"
          hint="各后端引擎上报的前缀缓存命中率算术平均（直采失败的实例不计入）"
          value={pct(stats.data?.backends.hit_rate)}
          foot={
            stats.data ? (
              <span>
                取自 {stats.data.backends.samples} / {stats.data.backends.total} 个实例快照
              </span>
            ) : undefined
          }
        />
      </div>

      <div className="cols-7-5">
        <Panel
          title="实时吞吐量"
          subtitle={
            history.data?.interval_seconds
              ? `采样间隔 ${history.data.interval_seconds}s`
              : undefined
          }
        >
          <Async state={history}>
            {(d) =>
              !d.enabled ? (
                <StateBlock
                  kind="disabled"
                  title="时序采样未启用"
                  detail="网关未注入采样缓冲，吞吐趋势不可用。"
                />
              ) : (
                <LineChart
                  series={[
                    {
                      name: 'RPS',
                      color: 'var(--series-1)',
                      points: (d.samples ?? []).map(toPoint('rps')),
                    },
                  ]}
                  yFormat={(v) => num(v, 1)}
                  xFormat={(x) => clock(new Date(x).toISOString())}
                  summary={summarize(d.samples ?? [], 'rps', (v) => `${num(v, 2)} RPS`)}
                  empty="采样缓冲为空（网关刚启动或近 30 分钟无请求）"
                />
              )
            }
          </Async>
        </Panel>

        <Panel title="实例分布" subtitle="按模型路由">
          <Async state={models}>
            {(d) => (
              <BarChart
                data={d.map((m) => ({ label: m.name, value: m.total }))}
                yFormat={(v) => String(Math.round(v))}
                empty="未配置模型路由"
              />
            )}
          </Async>
        </Panel>
      </div>

      <div className="cols-7-5">
        <Panel title="调度策略槽位" flush>
          <Async state={presets}>
            {(d) => {
              const rows = Object.entries(d.policies)
              if (!rows.length) return <StateBlock title="未配置调度策略" />
              const byPolicy = new Map<string, string[]>()
              for (const m of models.data ?? []) {
                const list = byPolicy.get(m.policy) ?? []
                list.push(m.name)
                byPolicy.set(m.policy, list)
              }
              return (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>策略槽位</th>
                        <th>生效方案</th>
                        <th>关联模型</th>
                        <th>打分表达式</th>
                      </tr>
                    </thead>
                    <tbody>
                      {rows.map(([name, st]) => (
                        <tr key={name}>
                          <td className="mono">{name}</td>
                          <td>
                            {st.preset === 'custom' ? (
                              <span className="badge accent">自定义</span>
                            ) : (
                              <span className="badge ok">
                                {d.presets.find((p) => p.name === st.preset)?.title ?? st.preset}
                              </span>
                            )}
                          </td>
                          <td className="mono">{(byPolicy.get(name) ?? []).join(', ') || '—'}</td>
                          <td>
                            <code className="mono ellipsis" title={st.score}>
                              {st.score}
                            </code>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )
            }}
          </Async>
        </Panel>

        <Panel
          title="最近告警"
          tools={
            <a className="btn sm ghost" href="#/operations">
              查看全部
            </a>
          }
        >
          <Async state={alerts}>
            {(d) => {
              if (!d.enabled)
                return (
                  <StateBlock
                    kind="disabled"
                    title="告警未启用"
                    detail="网关配置中未开启 alerting，无告警数据。"
                  />
                )
              const list = [...(d.active ?? [])]
                .sort((a, b) => rank(a.severity) - rank(b.severity))
                .slice(0, 4)
              if (!list.length) return <StateBlock title="当前无活跃告警" />
              return (
                <div className="stack" style={{ gap: 8 }}>
                  {list.map((a) => {
                    const sv = severityOf(a.severity)
                    return (
                      <div className={`alert-card ${sv}`} key={`${a.rule}-${a.instance}`}>
                        <h3>
                          {a.rule}
                          <SeverityBadge severity={sv} />
                        </h3>
                        <p>{a.summary || `实例 ${a.instance || '—'} 触发规则`}</p>
                        <span className="hint">
                          {a.instance || '集群级'} · {a.status} · {ago(a.since)}
                        </span>
                      </div>
                    )
                  })}
                </div>
              )
            }}
          </Async>
        </Panel>
      </div>
    </>
  )
}

function rank(sev: string | undefined): number {
  const s = severityOf(sev)
  return s === 'critical' ? 0 : s === 'warning' ? 1 : 2
}

/** toPoint 采样点转图表坐标（x 为毫秒时间戳）。 */
export function toPoint(key: keyof Sample) {
  return (s: Sample) => ({ x: new Date(s.time).getTime(), y: Number(s[key]) || 0 })
}

/** summarize 序列的当前值与区间峰值文本摘要（读屏与无悬停设备可读）。 */
export function summarize(
  samples: Sample[],
  key: keyof Sample,
  fmt: (v: number) => string,
): string | undefined {
  if (!samples.length) return undefined
  const values = samples.map((s) => Number(s[key]) || 0)
  const last = values[values.length - 1]
  const max = Math.max(...values)
  return `当前 ${fmt(last)} · 区间峰值 ${fmt(max)} · ${samples.length} 个采样点`
}
