// 运行态页：KV 前缀树、容量排队、PD 组与 NCCL 链路、告警、扩缩容建议、集群成员。
import { api, type AlertsResp, type AutoscaleResp, type ClusterResp, type KVCacheResp, type PDGroup, type QueueResp } from '../api'
import { usePoll } from '../hooks'
import { Empty, Section } from '../components'
import { bytes, num } from '../format'

export function Runtime() {
  const kv = usePoll<KVCacheResp>(() => api.kvcache(), 4000)
  const queue = usePoll<QueueResp>(() => api.queue(), 2000)
  const pd = usePoll<Record<string, PDGroup>>(() => api.pd(), 4000)
  const alerts = usePoll<AlertsResp>(() => api.alerts(), 5000)
  const scale = usePoll<AutoscaleResp>(() => api.autoscale(), 5000)
  const cluster = usePoll<ClusterResp>(() => api.cluster(), 5000)

  const pdGroups = Object.entries(pd.data ?? {})

  return (
    <>
      <div className="page-head">
        <div>
          <h1>运行态</h1>
          <p className="page-desc">
            KV 前缀树、容量排队、PD 分离链路、告警与扩缩容建议、集群成员的实时视图。
          </p>
        </div>
      </div>

      <div className="grid">
        <Section title="KV 前缀树">
          {kv.data?.enabled ? (
            <table>
              <tbody>
                <tr>
                  <th>占用字节</th>
                  <td className="num">{bytes(kv.data.stats?.bytes)}</td>
                </tr>
                <tr>
                  <th>节点数</th>
                  <td className="num">{num(kv.data.stats?.nodes)}</td>
                </tr>
              </tbody>
            </table>
          ) : (
            <Empty text="未启用（kv_cache.enabled=false）" />
          )}
        </Section>

        <Section title="容量排队">
          {queue.data?.enabled ? (
            <>
              <table>
                <tbody>
                  <tr>
                    <th>当前深度</th>
                    <td className="num">{num(queue.data.depth)}</td>
                  </tr>
                  <tr>
                    <th>深度上限</th>
                    <td className="num">{num(queue.data.max_depth)}</td>
                  </tr>
                  <tr>
                    <th>等待上限</th>
                    <td className="num">{queue.data.max_wait ?? '-'}</td>
                  </tr>
                </tbody>
              </table>
              {Object.keys(queue.data.by_model ?? {}).length > 0 && (
                <>
                  <p className="hint" style={{ marginBottom: 4 }}>按模型：</p>
                  <table>
                    <tbody>
                      {Object.entries(queue.data.by_model ?? {}).map(([m, d]) => (
                        <tr key={m}>
                          <th>{m}</th>
                          <td className="num">{d}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </>
              )}
            </>
          ) : (
            <Empty text="未启用（queue.enabled=false）" />
          )}
        </Section>

        <Section title="集群">
          {cluster.data?.enabled ? (
            <table>
              <thead>
                <tr>
                  <th>实例</th>
                  <th>角色</th>
                </tr>
              </thead>
              <tbody>
                {(cluster.data.members ?? []).map((m) => (
                  <tr key={m.id}>
                    <td>
                      {m.id}
                      {m.id === cluster.data?.self && <span className="badge"> 本机</span>}
                    </td>
                    <td>
                      {m.leader ? <span className="badge accent">leader</span> : <span className="badge">follower</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <Empty text="单机部署（cluster.enabled=false）" />
          )}
        </Section>
      </div>

      <div className="card">
        <h2>PD 分离组与 NCCL 链路</h2>
        {pdGroups.length === 0 ? (
          <Empty text="未配置 PD 分离组" />
        ) : (
          pdGroups.map(([name, g]) => (
            <div key={name} style={{ marginBottom: 16 }}>
              <div className="row" style={{ marginBottom: 8 }}>
                <strong>{name}</strong>
                <span className="badge">策略 {g.strategy}</span>
                {g.strategy === 'expression' && <span className="badge">{g.policy}</span>}
                <span className="hint">
                  prefill: {g.prefill.join(', ')} ｜ decode: {g.decode.join(', ')}
                </span>
              </div>
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>prefill</th>
                      <th>decode</th>
                      <th>带宽 Gbps</th>
                      <th>在途 KV 传输</th>
                    </tr>
                  </thead>
                  <tbody>
                    {g.links.map((l) => (
                      <tr key={`${l.prefill}->${l.decode}`}>
                        <td>{l.prefill}</td>
                        <td>{l.decode}</td>
                        <td className="num">{num(l.bandwidth_gbps)}</td>
                        <td className="num">{l.inflight}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          ))
        )}
      </div>

      <div className="grid">
        <Section title="告警">
          {!alerts.data?.enabled ? (
            <Empty text="未启用（alerting.enabled=false）" />
          ) : (alerts.data.active ?? []).length === 0 ? (
            <Empty text="当前无活跃告警" />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>规则</th>
                    <th>实例</th>
                    <th>状态</th>
                    <th>说明</th>
                  </tr>
                </thead>
                <tbody>
                  {(alerts.data.active ?? []).map((a, i) => (
                    <tr key={`${a.rule}-${a.instance}-${i}`}>
                      <td>{a.rule}</td>
                      <td>{a.instance}</td>
                      <td>
                        <span className={a.status === 'firing' ? 'badge err' : 'badge warn'}>
                          {a.status}
                        </span>
                      </td>
                      <td className="hint">{a.summary ?? ''}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Section>

        <Section title="扩缩容建议">
          {!scale.data?.enabled ? (
            <Empty text="未启用（autoscale.enabled=false）" />
          ) : (scale.data.recommendations ?? []).length === 0 ? (
            <Empty text="暂无建议" />
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>模型</th>
                    <th>当前</th>
                    <th>建议</th>
                    <th>方向</th>
                    <th>原因</th>
                  </tr>
                </thead>
                <tbody>
                  {(scale.data.recommendations ?? []).map((r) => (
                    <tr key={r.model}>
                      <td>{r.model}</td>
                      <td className="num">{num(r.current_replicas)}</td>
                      <td className="num">{num(r.desired_replicas)}</td>
                      <td>
                        <span className="badge">{r.direction ?? '-'}</span>
                      </td>
                      <td className="hint">{r.reason ?? ''}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Section>
      </div>
    </>
  )
}
