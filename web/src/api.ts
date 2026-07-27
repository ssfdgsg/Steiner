// 管理面 API 客户端与类型定义。
// 类型与后端响应结构一一对应（internal/server/server.go 为唯一权威来源）。

/** 后端指标快照（backend.Snapshot）。 */
export interface Snapshot {
  time: string
  running: number
  waiting: number
  kv_usage: number
  hit_rate: number
  gen_tok_per_sec: number
  err?: string
}

/** 后端状态视图（GET /admin/backends）。 */
export interface BackendView {
  id: string
  url: string
  engine: string
  weight: number
  healthy: boolean
  cordoned: boolean
  ejected: boolean
  inflight: number
  snapshot: Snapshot
  prom_vars?: Record<string, number>
  labels?: Record<string, string>
}

/** 内置调度方案预设（policy.Preset）。 */
export interface Preset {
  name: string
  title: string
  description: string
  filter: string
  score: string
}

/** 策略槽位当前状态：表达式源码 + 反查出的生效方案名（手写为 custom）。 */
export interface PolicyState {
  filter: string
  score: string
  preset: string
}

export interface PresetsResp {
  presets: Preset[]
  policies: Record<string, PolicyState>
}

/** 单后端打分明细（scheduler.ScoreDetail）。 */
export interface ScoreDetail {
  backend: string
  available: boolean
  pass: boolean
  score: number
  prefix_match: number
  inflight: number
  running: number
  waiting: number
  kv_usage: number
  err?: string
}

export interface ExplainResp {
  model: string
  route: string
  strategy: string
  policy: string
  scores: ScoreDetail[]
}

export interface KVCacheResp {
  enabled: boolean
  stats?: { bytes: number; nodes: number }
}

export interface PDLink {
  prefill: string
  decode: string
  bandwidth_gbps: number
  inflight: number
}

export interface PDGroup {
  strategy: string
  policy: string
  prefill: string[]
  decode: string[]
  links: PDLink[]
}

export interface QueueResp {
  enabled: boolean
  max_depth?: number
  max_wait?: string
  depth?: number
  by_model?: Record<string, number>
}

export interface AlertsResp {
  enabled: boolean
  active?: Array<{
    rule: string
    instance: string
    status: string
    severity?: string
    summary?: string
    since?: string
  }>
}

export interface AutoscaleResp {
  enabled: boolean
  recommendations?: Array<{
    model: string
    current_replicas?: number
    desired_replicas?: number
    direction?: string
    reason?: string
  }>
}

export interface ClusterResp {
  enabled: boolean
  self?: string
  leader?: boolean
  members?: Array<{ id: string; addr?: string; leader?: boolean }>
}

/** API 错误：携带 HTTP 状态码与后端返回的中文错误信息。 */
export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

/**
 * request 统一请求封装：非 2xx 时解析后端的 {"error": "..."} 结构抛 ApiError，
 * 便于各页面统一展示中文错误提示。
 */
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })
  const text = await resp.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!resp.ok) {
    const msg =
      (data && typeof data === 'object' && 'error' in data
        ? String((data as { error: unknown }).error)
        : typeof data === 'string' && data
          ? data
          : `请求失败（HTTP ${resp.status}）`)
    throw new ApiError(resp.status, msg)
  }
  return data as T
}

export const api = {
  backends: () => request<BackendView[]>('/admin/backends'),
  cordon: (id: string, on: boolean) =>
    request<unknown>(`/admin/backends/${encodeURIComponent(id)}/${on ? 'cordon' : 'uncordon'}`, {
      method: 'POST',
    }),
  removeBackend: (id: string) =>
    request<unknown>(`/admin/backends/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  addBackend: (body: Record<string, unknown>) =>
    request<unknown>('/admin/backends', { method: 'POST', body: JSON.stringify(body) }),

  presets: () => request<PresetsResp>('/admin/presets'),
  applyPreset: (name: string, policy?: string) => {
    const q = policy ? `?policy=${encodeURIComponent(policy)}` : ''
    return request<unknown>(`/admin/presets/${encodeURIComponent(name)}/apply${q}`, {
      method: 'POST',
    })
  },
  putPolicy: (name: string, filter: string, score: string) =>
    request<unknown>(`/admin/policies/${encodeURIComponent(name)}`, {
      method: 'PUT',
      body: JSON.stringify({ filter, score }),
    }),

  explain: (params: { model: string; prompt?: string; policy?: string; session?: string }) => {
    const q = new URLSearchParams()
    q.set('model', params.model)
    if (params.prompt) q.set('prompt', params.prompt)
    if (params.policy) q.set('policy', params.policy)
    if (params.session) q.set('session', params.session)
    return request<ExplainResp>(`/admin/explain?${q.toString()}`)
  },

  kvcache: () => request<KVCacheResp>('/admin/kvcache'),
  pd: () => request<Record<string, PDGroup>>('/admin/pd'),
  queue: () => request<QueueResp>('/admin/queue'),
  alerts: () => request<AlertsResp>('/admin/alerts'),
  autoscale: () => request<AutoscaleResp>('/admin/autoscale'),
  cluster: () => request<ClusterResp>('/admin/cluster'),
}
