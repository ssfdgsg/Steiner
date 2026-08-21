// 调度策略可视化构建器模型。
// 构建器只覆盖安全、常用、可双向解析的 Expr 子集；复杂函数、动态 map 与括号组合
// 保留在「表达式」模式，绝不做有损反向转换。

export type ValueType = 'number' | 'ratio' | 'bool' | 'string' | 'engine'

export interface FilterMetric {
  id: string
  label: string
  type: ValueType
  description: string
}

export const FILTER_METRICS: FilterMetric[] = [
  { id: 'healthy', label: '健康状态', type: 'bool', description: '主动健康检查是否通过' },
  { id: 'kv_usage', label: 'KV 占用率', type: 'ratio', description: '后端 KV cache 使用率' },
  { id: 'hit_rate', label: '前缀命中率', type: 'ratio', description: '后端前缀缓存命中率' },
  { id: 'prefix_match', label: '请求前缀匹配率', type: 'ratio', description: '当前请求与该实例缓存的匹配率' },
  { id: 'inflight', label: '网关在途请求', type: 'number', description: '网关侧尚未完成的请求数' },
  { id: 'running', label: '引擎运行请求', type: 'number', description: '引擎当前执行中的请求数' },
  { id: 'waiting', label: '引擎排队请求', type: 'number', description: '引擎内部排队请求数' },
  { id: 'gen_tps', label: '生成吞吐', type: 'number', description: '实例生成吞吐 token/s' },
  { id: 'ttft_ewma', label: 'TTFT EWMA', type: 'number', description: '首 token 延迟滑动均值（秒）' },
  { id: 'preempt_rate', label: '抢占速率', type: 'number', description: '引擎请求抢占次数/秒' },
  { id: 'weight', label: '实例权重', type: 'number', description: '后端静态权重' },
  { id: 'engine', label: '推理引擎', type: 'engine', description: 'vLLM / SGLang 及 Omni 变体' },
  { id: 'engine_family', label: '引擎家族', type: 'string', description: 'vllm 或 sglang' },
  { id: 'stream', label: '流式请求', type: 'bool', description: '请求是否为 stream=true' },
  { id: 'is_multimodal', label: '多模态请求', type: 'bool', description: '请求是否包含图片、音频或视频' },
  { id: 'prompt_tokens', label: 'Prompt Token 估算', type: 'number', description: '网关估算的输入 token 数' },
  { id: 'priority', label: '请求优先级', type: 'number', description: '请求 priority 字段' },
]

export interface Operator {
  id: string
  label: string
}

const ORDER_OPS: Operator[] = [
  { id: '<', label: '小于' },
  { id: '<=', label: '小于等于' },
  { id: '>', label: '大于' },
  { id: '>=', label: '大于等于' },
  { id: '==', label: '等于' },
  { id: '!=', label: '不等于' },
]

const EQUAL_OPS: Operator[] = [
  { id: '==', label: '等于' },
  { id: '!=', label: '不等于' },
]

export function operatorsFor(type: ValueType): Operator[] {
  return type === 'number' || type === 'ratio' ? ORDER_OPS : EQUAL_OPS
}

export interface FilterRule {
  id: string
  metric: string
  operator: string
  /** ratio 在 UI 中以百分比保存（90），生成 Expr 时转 0.9。 */
  value: string
}

export interface ScoreTerm {
  id: string
  metric: string
  coefficient: string
}

export interface ScoreMetric {
  id: string
  label: string
  unit: string
  description: string
}

export const SCORE_METRICS: ScoreMetric[] = [
  { id: 'running', label: '引擎运行请求', unit: '请求', description: '正系数避开繁忙实例' },
  { id: 'waiting', label: '引擎排队请求', unit: '请求', description: '正系数惩罚排队拥塞' },
  { id: 'inflight', label: '网关在途请求', unit: '请求', description: '正系数均衡网关实时负载' },
  { id: 'kv_usage', label: 'KV 占用率', unit: '0–1', description: '正系数避开高水位实例' },
  { id: 'prefix_match', label: '请求前缀匹配率', unit: '0–1', description: '负系数奖励缓存亲和' },
  { id: 'hit_rate', label: '历史前缀命中率', unit: '0–1', description: '负系数偏向缓存效率高的实例' },
  { id: 'ttft_ewma', label: 'TTFT EWMA', unit: '秒', description: '正系数避开首 token 慢的实例' },
  { id: 'preempt_rate', label: '抢占速率', unit: '次/秒', description: '正系数强惩罚显存压力' },
  { id: 'gen_tps', label: '生成吞吐', unit: 'tok/s', description: '负系数偏向吞吐更高的实例' },
  { id: 'weight', label: '实例权重', unit: '权重', description: '负系数偏向高权重实例' },
]

export interface BuilderState {
  join: '&&' | '||'
  rules: FilterRule[]
  baseScore: string
  terms: ScoreTerm[]
}

let nextID = 0
export function uid(prefix: string): string {
  nextID += 1
  return `${prefix}-${Date.now().toString(36)}-${nextID}`
}

export function defaultBuilder(): BuilderState {
  return {
    join: '&&',
    rules: [
      { id: uid('rule'), metric: 'healthy', operator: '==', value: 'true' },
      { id: uid('rule'), metric: 'kv_usage', operator: '<', value: '98' },
    ],
    baseScore: '0',
    terms: [
      { id: uid('term'), metric: 'running', coefficient: '2' },
      { id: uid('term'), metric: 'waiting', coefficient: '6' },
      { id: uid('term'), metric: 'inflight', coefficient: '1' },
      { id: uid('term'), metric: 'kv_usage', coefficient: '8' },
      { id: uid('term'), metric: 'prefix_match', coefficient: '-10' },
    ],
  }
}

function quote(v: string): string {
  return JSON.stringify(v)
}

function metricDef(id: string): FilterMetric | undefined {
  return FILTER_METRICS.find((m) => m.id === id)
}

export function buildFilter(state: BuilderState): string {
  const parts = state.rules.map((r) => {
    const def = metricDef(r.metric)
    if (!def) return 'true'
    let value: string
    if (def.type === 'bool') value = r.value === 'false' ? 'false' : 'true'
    else if (def.type === 'ratio') value = String((Number(r.value) || 0) / 100)
    else if (def.type === 'number') value = normalizeNumber(r.value, '0')
    else value = quote(r.value)
    return `${r.metric} ${r.operator} ${value}`
  })
  return parts.length ? parts.join(` ${state.join} `) : 'healthy'
}

export function buildScore(state: BuilderState): string {
  const base = normalizeNumber(state.baseScore, '0')
  const parts: string[] = []
  if (Number(base) !== 0 || state.terms.length === 0) parts.push(base)
  for (const t of state.terms) {
    const c = Number(t.coefficient)
    if (!Number.isFinite(c) || c === 0) continue
    const abs = trimNumber(Math.abs(c))
    const term = abs === '1' ? t.metric : `${t.metric} * ${abs}`
    if (!parts.length) parts.push(c < 0 ? `-${term}` : term)
    else parts.push(`${c < 0 ? '-' : '+'} ${term}`)
  }
  return parts.join(' ') || '0'
}

function normalizeNumber(v: string, fallback: string): string {
  const n = Number(v)
  return Number.isFinite(n) ? trimNumber(n) : fallback
}

function trimNumber(n: number): string {
  return Number.isInteger(n) ? String(n) : String(Number(n.toFixed(6)))
}

/**
 * parseBuilder 尝试无损解析构建器生成的常用 Expr 子集。
 * 返回 null 表示包含复杂函数、括号、map、乘方或其它无法无损表达的语法。
 */
export function parseBuilder(filter: string, score: string): BuilderState | null {
  const parsedFilter = parseFilter(filter || 'healthy')
  const parsedScore = parseScore(score)
  if (!parsedFilter || !parsedScore) return null
  return { ...parsedFilter, ...parsedScore }
}

function parseFilter(filter: string): Pick<BuilderState, 'join' | 'rules'> | null {
  const hasAnd = /\s&&\s/.test(filter)
  const hasOr = /\s\|\|\s/.test(filter)
  if (hasAnd && hasOr) return null
  const join: '&&' | '||' = hasOr ? '||' : '&&'
  const parts = filter.split(join === '&&' ? /\s+&&\s+/ : /\s+\|\|\s+/)
  const rules: FilterRule[] = []
  for (const p of parts) {
    const m = p.trim().match(/^([a-z_][a-z0-9_]*)\s*(==|!=|<=|>=|<|>)\s*(.+)$/i)
    // 单独 healthy 是 Set 的缺省形式，等价于 healthy == true。
    if (p.trim() === 'healthy') {
      rules.push({ id: uid('rule'), metric: 'healthy', operator: '==', value: 'true' })
      continue
    }
    if (!m) return null
    const def = metricDef(m[1])
    if (!def || !operatorsFor(def.type).some((o) => o.id === m[2])) return null
    let value = m[3].trim()
    if (def.type === 'ratio') {
      const n = Number(value)
      if (!Number.isFinite(n)) return null
      value = trimNumber(n * 100)
    } else if (def.type === 'number') {
      if (!Number.isFinite(Number(value))) return null
      value = trimNumber(Number(value))
    } else if (def.type === 'bool') {
      if (value !== 'true' && value !== 'false') return null
    } else {
      try {
        const parsed = JSON.parse(value)
        if (typeof parsed !== 'string') return null
        value = parsed
      } catch {
        return null
      }
    }
    rules.push({ id: uid('rule'), metric: m[1], operator: m[2], value })
  }
  return { join, rules }
}

function parseScore(score: string): Pick<BuilderState, 'baseScore' | 'terms'> | null {
  if (!score.trim() || /[()\[\]^?]/.test(score)) return null
  // 把二元 +/- 变成统一片段；科学计数法不在构建器输出中，避免误拆 e-3。
  const normalized = score.trim().replace(/\s+([+-])\s+/g, '|$1')
  const chunks = normalized.split('|')
  let baseScore = '0'
  const terms: ScoreTerm[] = []
  for (let i = 0; i < chunks.length; i++) {
    let chunk = chunks[i].trim()
    let sign = 1
    if (chunk.startsWith('+')) chunk = chunk.slice(1).trim()
    else if (chunk.startsWith('-')) {
      sign = -1
      chunk = chunk.slice(1).trim()
    }
    if (/^-?\d+(?:\.\d+)?$/.test(chunk)) {
      if (i !== 0) return null
      baseScore = trimNumber(Number(chunk) * sign)
      continue
    }
    const m = chunk.match(/^([a-z_][a-z0-9_]*)(?:\s*\*\s*(-?\d+(?:\.\d+)?))?$/i)
    if (!m || !SCORE_METRICS.some((d) => d.id === m[1])) return null
    const coefficient = sign * (m[2] === undefined ? 1 : Number(m[2]))
    terms.push({ id: uid('term'), metric: m[1], coefficient: trimNumber(coefficient) })
  }
  return { baseScore, terms }
}

export const VARIABLE_GROUPS = [
  {
    title: '实例负载',
    items: [
      ['running', '引擎运行请求数'],
      ['waiting', '引擎排队请求数'],
      ['inflight', '网关在途请求数'],
      ['kv_usage', 'KV 使用率 0–1'],
      ['hit_rate', '前缀命中率 0–1'],
      ['gen_tps', '生成吞吐 tok/s'],
      ['ttft_ewma', 'TTFT 滑动均值（秒）'],
      ['preempt_rate', '抢占速率（次/秒）'],
      ['prefix_match', '当前请求前缀匹配率'],
      ['weight', '实例静态权重'],
    ],
  },
  {
    title: '实例属性',
    items: [
      ['healthy', '健康检查结果'],
      ['backend', '实例 ID'],
      ['engine', '引擎类型'],
      ['engine_family', 'vllm / sglang'],
      ['labels["key"]', '自定义字符串标签'],
      ['vars["name"]', 'Prometheus 注入变量'],
      ['raw["metric"]', '后端原始指标'],
    ],
  },
  {
    title: '请求上下文',
    items: [
      ['model', '模型名'],
      ['stream', '是否流式'],
      ['prompt_tokens', 'Prompt token 估算'],
      ['priority', '请求优先级'],
      ['session', '会话 ID'],
      ['is_multimodal', '是否多模态'],
      ['image_count', '图片数量'],
      ['audio_count', '音频数量'],
      ['video_count', '视频数量'],
    ],
  },
] as const
