// 管理面 API 客户端与类型定义。
// 类型与后端响应结构一一对应（internal/server/server.go 为唯一权威来源）。
import { getToken, UNAUTHORIZED_EVENT } from './auth'

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

/** 策略表达式只读校验结果（POST /admin/policies/validate）。 */
export interface PolicyValidation {
  valid: boolean
  filter: string
  score: string
  errors: Array<{ field: 'filter' | 'score'; message: string }>
  warning?: string
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

/** 金丝雀发布状态（rollout.StatusView）。 */
export interface RolloutView {
  model: string
  canary: string
  state: string
  step_index: number
  steps: number[]
  canary_weight: number
  step_since: string
  canary_requests: number
  canary_error_rate: number
  canary_ttft_p95: number
  stable_requests: number
  stable_error_rate: number
  stable_ttft_p95: number
}

export interface RolloutsResp {
  enabled: boolean
  rollouts?: RolloutView[]
}

/** 时延分布（metrics.Dist），分位数由直方图桶插值估算。 */
export interface Dist {
  count: number
  avg_ms: number
  p50_ms: number
  p90_ms: number
  p95_ms: number
  p99_ms: number
}

/** 按模型 / 后端维度的请求统计（metrics.LabelStat）。 */
export interface LabelStat {
  name: string
  requests: number
  errors: number
  avg_ms: number
}

/** 网关级累计统计（metrics.Aggregate），口径为进程启动以来。 */
export interface Aggregate {
  requests_total: number
  errors_total: number
  error_rate: number
  retries_total: number
  rate_limited_total: number
  pick_errors_total: number
  prompt_tokens_total: number
  completion_tokens_total: number
  latency: Dist
  ttft: Dist
  by_code: Record<string, number>
  by_model: LabelStat[]
  by_backend: LabelStat[]
}

/** 后端池即时汇总（server.backendSummary）。 */
export interface BackendSummary {
  total: number
  healthy: number
  available: number
  cordoned: number
  ejected: number
  inflight: number
  running: number
  waiting: number
  kv_usage: number
  hit_rate: number
  gen_tok_per_sec: number
  samples: number
}

export interface StatsResp {
  since: string
  uptime_seconds: number
  avg_rps?: number
  aggregate: Aggregate
  backends: BackendSummary
  queue: {
    enabled: boolean
    depth?: number
    max_depth?: number
    by_model?: Record<string, number>
  }
  history_interval_seconds?: number
}

/** 时序采样点（metrics.Sample），速率字段为相邻采样差分值。 */
export interface Sample {
  time: string
  rps: number
  error_rate: number
  latency_avg_ms: number
  latency_p95_ms: number
  ttft_p95_ms: number
  queue_depth: number
  backends_healthy: number
  backends_total: number
  kv_bytes: number
  kv_nodes: number
  kv_usage: number
  hit_rate: number
  gen_tok_per_sec: number
}

export interface HistoryResp {
  enabled: boolean
  interval_seconds?: number
  capacity?: number
  samples?: Sample[]
}

/** 权重子池视图（server.splitView）。 */
export interface SplitView {
  name: string
  strategy?: string
  policy?: string
  weight: number
  hits: number
  backends: string[]
}

/** 模型路由视图（server.modelView）：模型 → 实例的权威映射。 */
export interface ModelView {
  name: string
  strategy: string
  policy: string
  pd_group?: string
  rewrite_model?: string
  rate_limit_qps?: number
  backends: string[]
  total: number
  available: number
  inflight: number
  kv_usage: number
  hit_rate: number
  splits?: SplitView[]
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
 * 便于各页面统一展示中文错误提示。携带当前管理面令牌（见 auth.ts），
 * 401（令牌缺失/失效）时派发 unauthorized 事件，App 回到登录态。
 */
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const token = getToken()
  const resp = await fetch(path, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(init?.headers ?? {}),
    },
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
    if (resp.status === 401) {
      window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT))
    }
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

/** checkAuth 用当前令牌探测管理面连通性（登录表单验证用）。 */
export function checkAuth(): Promise<unknown> {
  return request('/admin/backends')
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
  validatePolicy: (filter: string, score: string) =>
    request<PolicyValidation>('/admin/policies/validate', {
      method: 'POST',
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
  rollouts: () => request<RolloutsResp>('/admin/rollouts'),
  resetRollout: (model: string) =>
    request<unknown>(`/admin/rollouts/${encodeURIComponent(model)}/reset`, { method: 'POST' }),

  stats: () => request<StatsResp>('/admin/stats'),
  history: (minutes?: number) =>
    request<HistoryResp>(`/admin/stats/history${minutes ? `?minutes=${minutes}` : ''}`),
  models: () => request<ModelView[]>('/admin/models'),
}
