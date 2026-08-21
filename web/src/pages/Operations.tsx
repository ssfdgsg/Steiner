// 告警与扩缩容：当前风险、容量状态与扩缩容建议。
// 扩缩容执行与历史记录由外部控制器落地（网关只产出建议），故本页只读。
import { useMemo } from 'react'
import { api } from '../api'
import { useInterval, usePoll, useRefreshControl } from '../hooks'
import {
  Async,
  FeatureBadge,
  MetricCard,
  Notice,
  Panel,
  SeverityBadge,
  StateBlock,
  UsageBar,
  severityOf,
  type Severity,
} from '../components'
import { LineChart } from '../charts'
import { toPoint } from './Overview'
import { ago, clock, num, pct, stamp } from '../format'

const ORDER: Record<Severity, number> = { critical: 0, warning: 1, info: 2 }

export function Operations() {
  const { nonce } = useRefreshControl()
  const base = useInterval()
  const alerts = usePoll(api.alerts, base, nonce)
  const scale = usePoll(api.autoscale, base, nonce)
  const stats = usePoll(api.stats, base, nonce)
  const history = usePoll(() => api.history(360), base * 2, nonce)

  const active = useMemo(
    () =>
      [...(alerts.data?.active ?? [])].sort(
        (a, b) => ORDER[severityOf(a.severity)] - ORDER[severityOf(b.severity)],
      ),
    [alerts.data],
  )
  const counts = useMemo(() => {
    const c: Record<Severity, number> = { critical: 0, warning: 0, info: 0 }
    for (const a of active) c[severityOf(a.severity)]++
    return c
  }, [active])

  const recs = scale.data?.recommendations ?? []
  const actionable = recs.filter((r) => r.direction === 'up' || r.direction === 'down')

  return (
    <>
      <p className="page-desc">
        告警由网关规则引擎求值（集群模式下仅 leader 求值以避免重复通知）。扩缩容建议经 webhook
        推送给外部控制器执行，网关自身不改变副本数。
      </p>

      <div className="kpis three">
        <MetricCard
          label="严重告警"
          icon="critical"
          tone="danger"
          value={alerts.data?.enabled ? String(counts.critical) : '未启用'}
          foot={
            alerts.data?.enabled ? (
              <span>
                警告 {counts.warning} · 信息 {counts.info}
              </span>
            ) : (
              <span>alerting.enabled = false</span>
            )
          }
        />
        <MetricCard
          label="可用容量"
          icon="server"
          tone={stats.data && stats.data.backends.available < stats.data.backends.total ? 'warning' : 'success'}
          value={stats.data ? String(stats.data.backends.available) : '-'}
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
          label="待处理扩缩容建议"
          icon="scale"
          tone="info"
          value={scale.data?.enabled ? String(actionable.length) : '未启用'}
          foot={
            scale.data?.enabled ? (
              <span>共 {recs.length} 个模型有建议记录</span>
            ) : (
              <span>autoscale.enabled = false</span>
            )
          }
        />
      </div>

      <Panel title="活跃告警" tools={<FeatureBadge enabled={alerts.data?.enabled} />} flush>
        <Async state={alerts} skeletonRows={3}>
          {(d) => {
            if (!d.enabled)
              return (
                <StateBlock
                  kind="disabled"
                  title="告警未启用"
                  detail="在配置中开启 alerting 并声明规则后，此处展示 pending / firing 的告警实例。"
                />
              )
            if (!active.length) return <StateBlock title="当前无活跃告警" detail="所有规则均未触发。" />
            if (active.length <= 3)
              return (
                <div className="panel-body">
                  <div className="grid-auto">
                    {active.map((a) => {
                      const sv = severityOf(a.severity)
                      return (
                        <div className={`alert-card ${sv}`} key={`${a.rule}-${a.instance}`}>
                          <h3>
                            {a.rule}
                            <SeverityBadge severity={sv} />
                          </h3>
                          <p>{a.summary || '规则条件已满足'}</p>
                          <span className="hint">
                            {a.instance || '集群级'} · {a.status} · 持续 {ago(a.since)}
                          </span>
                        </div>
                      )
                    })}
                  </div>
                </div>
              )
            return (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>级别</th>
                      <th>规则</th>
                      <th>对象</th>
                      <th>状态</th>
                      <th>说明</th>
                      <th>开始时间</th>
                    </tr>
                  </thead>
                  <tbody>
                    {active.map((a) => (
                      <tr key={`${a.rule}-${a.instance}`}>
                        <td>
                          <SeverityBadge severity={severityOf(a.severity)} />
                        </td>
                        <td className="mono">{a.rule}</td>
                        <td className="mono">{a.instance || '集群级'}</td>
                        <td>
                          <span className={a.status === 'firing' ? 'badge err' : 'badge warn'}>
                            {a.status}
                          </span>
                        </td>
                        <td>{a.summary || '—'}</td>
                        <td className="hint" title={stamp(a.since)}>
                          {ago(a.since)}
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

      <div className="cols-7-5">
        <Panel title="容量趋势" subtitle="近 6 小时可用实例数">
          <Async state={history}>
            {(d) => {
              if (!d.enabled) return <StateBlock kind="disabled" title="时序采样未启用" />
              const samples = d.samples ?? []
              const desired = recs.reduce((sum, r) => sum + (r.desired_replicas ?? 0), 0)
              const series = [
                {
                  name: '可用实例',
                  color: 'var(--series-1)',
                  points: samples.map(toPoint('backends_healthy')),
                },
                {
                  name: '已注册实例',
                  color: 'var(--series-4)',
                  points: samples.map(toPoint('backends_total')),
                  dashed: true,
                },
              ]
              if (desired > 0 && samples.length > 1) {
                // 建议副本数没有历史序列，作为当前建议值的水平参考线展示（虚线区分）。
                series.push({
                  name: '当前建议副本合计',
                  color: 'var(--series-3)',
                  dashed: true,
                  points: [
                    { x: new Date(samples[0].time).getTime(), y: desired },
                    { x: new Date(samples[samples.length - 1].time).getTime(), y: desired },
                  ],
                })
              }
              return (
                <LineChart
                  height={250}
                  series={series}
                  yFormat={(v) => String(Math.round(v))}
                  xFormat={(x) => clock(new Date(x).toISOString())}
                  summary="实例数为网关视角的健康计数，副本编排结果以 Kubernetes 为准。"
                />
              )
            }}
          </Async>
        </Panel>

        <Panel title="集群负载" subtitle="即时快照">
          <Async state={stats}>
            {(d) => (
              <div className="stack" style={{ gap: 14 }}>
                <div>
                  <div className="row" style={{ justifyContent: 'space-between' }}>
                    <span>实例可用率</span>
                    <span className="num">
                      {d.backends.available} / {d.backends.total}
                    </span>
                  </div>
                  <UsageBar
                    value={d.backends.total ? d.backends.available / d.backends.total : 0}
                    label="实例可用率"
                  />
                </div>
                <div>
                  <div className="row" style={{ justifyContent: 'space-between' }}>
                    <span>平均 KV 占用</span>
                    <span className="num">{pct(d.backends.kv_usage)}</span>
                  </div>
                  <UsageBar value={d.backends.kv_usage} label="平均 KV 占用" />
                </div>
                <div>
                  <div className="row" style={{ justifyContent: 'space-between' }}>
                    <span>平均前缀命中率</span>
                    <span className="num">{pct(d.backends.hit_rate)}</span>
                  </div>
                  <UsageBar value={d.backends.hit_rate} label="平均前缀命中率" />
                </div>
                <dl className="kv">
                  <dt>在途请求</dt>
                  <dd className="num">{d.backends.inflight}</dd>
                  <dt>引擎运行中</dt>
                  <dd className="num">{num(d.backends.running)}</dd>
                  <dt>引擎排队</dt>
                  <dd className="num">{num(d.backends.waiting)}</dd>
                  <dt>生成吞吐</dt>
                  <dd className="num">{num(d.backends.gen_tok_per_sec, 1)} tok/s</dd>
                </dl>
                <span className="hint">
                  GPU / CPU / 内存 / 网络的主机级利用率不在网关采集范围，请查节点监控。
                </span>
              </div>
            )}
          </Async>
        </Panel>
      </div>

      <Panel title="自动扩缩容建议" tools={<FeatureBadge enabled={scale.data?.enabled} />} flush>
        <Async state={scale} skeletonRows={3}>
          {(d) => {
            if (!d.enabled)
              return (
                <StateBlock
                  kind="disabled"
                  title="扩缩容建议未启用"
                  detail="开启 autoscale 后，网关会按表达式产出期望副本数并经 webhook 推送。"
                />
              )
            if (!recs.length) return <StateBlock title="暂无扩缩容建议" />
            return (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>模型</th>
                      <th>方向</th>
                      <th style={{ textAlign: 'right' }}>当前副本</th>
                      <th style={{ textAlign: 'right' }}>建议副本</th>
                      <th>判据</th>
                    </tr>
                  </thead>
                  <tbody>
                    {recs.map((r) => (
                      <tr key={r.model}>
                        <td className="mono">{r.model}</td>
                        <td>
                          {r.direction === 'up' ? (
                            <span className="badge warn">扩容</span>
                          ) : r.direction === 'down' ? (
                            <span className="badge info">缩容</span>
                          ) : (
                            <span className="badge muted">保持</span>
                          )}
                        </td>
                        <td className="num" style={{ textAlign: 'right' }}>
                          {r.current_replicas ?? '—'}
                        </td>
                        <td className="num" style={{ textAlign: 'right' }}>
                          {r.desired_replicas ?? '—'}
                        </td>
                        <td>{r.reason || '—'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )
          }}
        </Async>
      </Panel>

      <Notice kind="warn">
        网关不提供执行扩缩容的管理接口，也不记录扩缩容历史与告警历史。副本变更由订阅 webhook
        的外部控制器（HPA / KEDA / 自研 operator）落地，执行记录请在该控制器或 Kubernetes 事件中查看。
      </Notice>
    </>
  )
}
