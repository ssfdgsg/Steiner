// 高级策略编辑器：Grafana 风格「可视化构建 / 表达式」双模式。
// 可视化构建器只生成可无损解析的安全子集；完整 Expr DSL 始终可在表达式模式使用。
import { useEffect, useMemo, useState } from 'react'
import { api, type PolicyValidation } from '../api'
import { Icon } from '../icons'
import { Notice, Panel, Segmented } from '../components'
import {
  FILTER_METRICS,
  SCORE_METRICS,
  VARIABLE_GROUPS,
  buildFilter,
  buildScore,
  defaultBuilder,
  operatorsFor,
  parseBuilder,
  uid,
  type BuilderState,
  type FilterRule,
  type ScoreTerm,
} from '../policyBuilder'

type Mode = 'builder' | 'expression'

export function PolicyEditor({
  target,
  filter,
  score,
  onSaved,
  onError,
}: {
  target: string
  filter: string
  score: string
  onSaved: (msg: string) => void
  onError: (msg: string) => void
}) {
  const initialParsed = useMemo(() => parseBuilder(filter, score), [filter, score])
  const [mode, setMode] = useState<Mode>(initialParsed ? 'builder' : 'expression')
  const [builder, setBuilder] = useState<BuilderState>(initialParsed ?? defaultBuilder())
  const [f, setF] = useState(filter)
  const [s, setS] = useState(score)
  const [validation, setValidation] = useState<PolicyValidation | null>(null)
  const [validating, setValidating] = useState(false)
  const [saving, setSaving] = useState(false)
  const [switchBlocked, setSwitchBlocked] = useState(false)

  useEffect(() => {
    setF(filter)
    setS(score)
    const parsed = parseBuilder(filter, score)
    if (parsed) setBuilder(parsed)
    setMode(parsed ? 'builder' : 'expression')
    setSwitchBlocked(false)
  }, [filter, score])

  const generatedFilter = useMemo(() => buildFilter(builder), [builder])
  const generatedScore = useMemo(() => buildScore(builder), [builder])
  const effectiveFilter = mode === 'builder' ? generatedFilter : f
  const effectiveScore = mode === 'builder' ? generatedScore : s
  const dirty = effectiveFilter !== filter || effectiveScore !== score

  // 输入停止 450ms 后调用服务端只读编译接口；过期响应由 cancelled 丢弃。
  useEffect(() => {
    let cancelled = false
    setValidating(true)
    const timer = window.setTimeout(() => {
      void api
        .validatePolicy(effectiveFilter, effectiveScore)
        .then((result) => {
          if (!cancelled) setValidation(result)
        })
        .catch((e) => {
          if (!cancelled) {
            setValidation({
              valid: false,
              filter: effectiveFilter,
              score: effectiveScore,
              errors: [{ field: 'score', message: e instanceof Error ? e.message : String(e) }],
            })
          }
        })
        .finally(() => {
          if (!cancelled) setValidating(false)
        })
    }, 450)
    return () => {
      cancelled = true
      window.clearTimeout(timer)
    }
  }, [effectiveFilter, effectiveScore])

  const changeMode = (next: Mode) => {
    if (next === mode) return
    if (next === 'expression') {
      setF(generatedFilter)
      setS(generatedScore)
      setMode('expression')
      setSwitchBlocked(false)
      return
    }
    const parsed = parseBuilder(f, s)
    if (!parsed) {
      setSwitchBlocked(true)
      return
    }
    setBuilder(parsed)
    setMode('builder')
    setSwitchBlocked(false)
  }

  const reset = () => {
    setF(filter)
    setS(score)
    const parsed = parseBuilder(filter, score)
    if (parsed) {
      setBuilder(parsed)
      setMode('builder')
    } else {
      setMode('expression')
    }
    setSwitchBlocked(false)
  }

  const rebuild = () => {
    setBuilder(defaultBuilder())
    setMode('builder')
    setSwitchBlocked(false)
  }

  const save = async () => {
    setSaving(true)
    try {
      const check = await api.validatePolicy(effectiveFilter, effectiveScore)
      setValidation(check)
      if (!check.valid) return
      await api.putPolicy(target, check.filter, check.score)
      onSaved(`策略槽位 ${target} 已更新`)
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  const fieldError = (field: 'filter' | 'score') =>
    validation?.errors.find((e) => e.field === field)?.message

  return (
    <Panel
      title="高级策略编辑"
      subtitle={`策略槽位 ${target}`}
      tools={
        <>
          <Segmented
            ariaLabel="策略编辑模式"
            value={mode}
            options={[
              { value: 'builder', label: '可视化构建' },
              { value: 'expression', label: '表达式' },
            ]}
            onChange={changeMode}
          />
          <button className="btn ghost" disabled={!dirty || saving} onClick={reset}>
            还原
          </button>
          <button
            className="btn primary"
            disabled={!dirty || saving || validating || validation?.valid === false}
            onClick={() => void save()}
          >
            {saving ? '保存中…' : validating ? '校验中…' : '保存并生效'}
          </button>
        </>
      }
    >
      <div className="stack">
        {switchBlocked && (
          <Notice
            kind="warn"
            action={
              <button className="btn sm" onClick={rebuild}>
                使用均衡模板重建
              </button>
            }
          >
            当前表达式包含函数、括号、动态指标或混合逻辑，无法无损转换为可视化规则。表达式内容未被修改；可继续使用表达式模式，或从均衡模板重新构建。
          </Notice>
        )}

        {mode === 'builder' ? (
          <Builder state={builder} onChange={setBuilder} filter={generatedFilter} score={generatedScore} />
        ) : (
          <ExpressionMode
            filter={f}
            score={s}
            filterError={fieldError('filter')}
            scoreError={fieldError('score')}
            onFilter={setF}
            onScore={setS}
          />
        )}

        <div className={validation?.valid ? 'policy-validation ok' : validation ? 'policy-validation err' : 'policy-validation'}>
          {validating ? (
            <>
              <span className="dot" /> 服务端编译校验中…
            </>
          ) : validation?.valid ? (
            <>
              <Icon name="check" size={13} /> 语法与结果类型校验通过
            </>
          ) : validation ? (
            <>
              <Icon name="critical" size={13} /> 有 {validation.errors.length} 个编译问题，请修正后保存
            </>
          ) : (
            '等待校验'
          )}
        </div>
      </div>
    </Panel>
  )
}

function Builder({
  state,
  onChange,
  filter,
  score,
}: {
  state: BuilderState
  onChange: (s: BuilderState) => void
  filter: string
  score: string
}) {
  const updateRule = (id: string, patch: Partial<FilterRule>) => {
    onChange({ ...state, rules: state.rules.map((r) => (r.id === id ? { ...r, ...patch } : r)) })
  }
  const updateTerm = (id: string, patch: Partial<ScoreTerm>) => {
    onChange({ ...state, terms: state.terms.map((t) => (t.id === id ? { ...t, ...patch } : t)) })
  }

  return (
    <>
      <Notice>
        构建器生成 Expr 表达式并实时调用网关编译校验。评分是“代价”：分数越小越优；正系数惩罚该指标，负系数奖励该指标。
      </Notice>

      <div className="policy-builder-section">
        <div className="policy-builder-head">
          <div>
            <h3>候选过滤</h3>
            <p>不满足条件的实例不参与本次调度。</p>
          </div>
          <div className="seg" role="group" aria-label="过滤条件逻辑关系">
            <button className={state.join === '&&' ? 'active' : undefined} onClick={() => onChange({ ...state, join: '&&' })}>
              全部满足（AND）
            </button>
            <button className={state.join === '||' ? 'active' : undefined} onClick={() => onChange({ ...state, join: '||' })}>
              任一满足（OR）
            </button>
          </div>
        </div>

        <div className="rule-list">
          {state.rules.map((r, index) => (
            <FilterRuleRow
              key={r.id}
              rule={r}
              connector={index === 0 ? '当' : state.join === '&&' ? '并且' : '或者'}
              onChange={(patch) => updateRule(r.id, patch)}
              onRemove={() => onChange({ ...state, rules: state.rules.filter((x) => x.id !== r.id) })}
            />
          ))}
          <button
            className="btn sm ghost add-row"
            onClick={() =>
              onChange({
                ...state,
                rules: [
                  ...state.rules,
                  { id: uid('rule'), metric: 'waiting', operator: '<', value: '32' },
                ],
              })
            }
          >
            <Icon name="plus" size={13} /> 添加过滤条件
          </button>
        </div>
      </div>

      <div className="policy-builder-section">
        <div className="policy-builder-head">
          <div>
            <h3>实例代价评分</h3>
            <p>对通过过滤的实例计算代价，选择分数最小者。</p>
          </div>
          <label className="inline-number">
            基础分
            <input
              type="number"
              step="0.1"
              value={state.baseScore}
              onChange={(e) => onChange({ ...state, baseScore: e.target.value })}
            />
          </label>
        </div>

        <div className="score-list">
          <div className="score-row score-header" aria-hidden="true">
            <span>指标</span>
            <span>系数</span>
            <span>效果</span>
            <span />
          </div>
          {state.terms.map((t) => (
            <ScoreTermRow
              key={t.id}
              term={t}
              onChange={(patch) => updateTerm(t.id, patch)}
              onRemove={() => onChange({ ...state, terms: state.terms.filter((x) => x.id !== t.id) })}
            />
          ))}
          <button
            className="btn sm ghost add-row"
            onClick={() =>
              onChange({
                ...state,
                terms: [...state.terms, { id: uid('term'), metric: 'ttft_ewma', coefficient: '10' }],
              })
            }
          >
            <Icon name="plus" size={13} /> 添加评分因子
          </button>
        </div>
      </div>

      <div className="generated-preview">
        <div>
          <label>生成的 filter</label>
          <pre className="expr">{filter}</pre>
        </div>
        <div>
          <label>生成的 score（代价越小越优）</label>
          <pre className="expr">{score}</pre>
        </div>
      </div>
    </>
  )
}

function FilterRuleRow({
  rule,
  connector,
  onChange,
  onRemove,
}: {
  rule: FilterRule
  connector: string
  onChange: (p: Partial<FilterRule>) => void
  onRemove: () => void
}) {
  const def = FILTER_METRICS.find((m) => m.id === rule.metric) ?? FILTER_METRICS[0]
  const ops = operatorsFor(def.type)
  const metricChange = (metric: string) => {
    const next = FILTER_METRICS.find((m) => m.id === metric) ?? FILTER_METRICS[0]
    const value = next.type === 'bool' ? 'true' : next.type === 'ratio' ? '90' : next.type === 'engine' ? 'vllm' : next.type === 'string' ? '' : '0'
    onChange({ metric, operator: operatorsFor(next.type)[0].id, value })
  }

  return (
    <div className="rule-row">
      <span className="rule-connector">{connector}</span>
      <select aria-label="过滤指标" value={rule.metric} title={def.description} onChange={(e) => metricChange(e.target.value)}>
        {FILTER_METRICS.map((m) => (
          <option key={m.id} value={m.id}>{m.label}</option>
        ))}
      </select>
      <select aria-label="操作符" value={rule.operator} onChange={(e) => onChange({ operator: e.target.value })}>
        {ops.map((o) => (
          <option key={o.id} value={o.id}>{o.label}</option>
        ))}
      </select>
      <RuleValue def={def} value={rule.value} onChange={(value) => onChange({ value })} />
      <button className="btn icon sm ghost danger" aria-label="删除过滤条件" onClick={onRemove}>
        <Icon name="trash" size={13} />
      </button>
    </div>
  )
}

function RuleValue({ def, value, onChange }: { def: (typeof FILTER_METRICS)[number]; value: string; onChange: (v: string) => void }) {
  if (def.type === 'bool')
    return (
      <select aria-label="条件值" value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="true">是</option>
        <option value="false">否</option>
      </select>
    )
  if (def.type === 'engine')
    return (
      <select aria-label="引擎" value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="vllm">vLLM</option>
        <option value="vllm_omni">vLLM-Omni</option>
        <option value="sglang">SGLang</option>
        <option value="sglang_omni">SGLang-Omni</option>
      </select>
    )
  if (def.type === 'string')
    return <input aria-label="条件值" value={value} placeholder="vllm" onChange={(e) => onChange(e.target.value)} />
  return (
    <span className="number-input">
      <input
        aria-label="条件值"
        type="number"
        min={def.type === 'ratio' ? 0 : undefined}
        max={def.type === 'ratio' ? 100 : undefined}
        step={def.type === 'ratio' ? 1 : 0.1}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
      {def.type === 'ratio' && <span>%</span>}
    </span>
  )
}

function ScoreTermRow({ term, onChange, onRemove }: { term: ScoreTerm; onChange: (p: Partial<ScoreTerm>) => void; onRemove: () => void }) {
  const coefficient = Number(term.coefficient)
  const effect = coefficient > 0 ? '惩罚' : coefficient < 0 ? '奖励' : '忽略'
  const effectClass = coefficient > 0 ? 'warn' : coefficient < 0 ? 'ok' : 'muted'
  const def = SCORE_METRICS.find((m) => m.id === term.metric) ?? SCORE_METRICS[0]
  return (
    <div className="score-row">
      <select aria-label="评分指标" value={term.metric} title={def.description} onChange={(e) => onChange({ metric: e.target.value })}>
        {SCORE_METRICS.map((m) => (
          <option key={m.id} value={m.id}>{m.label}（{m.unit}）</option>
        ))}
      </select>
      <span className="coefficient-input">
        <span>×</span>
        <input type="number" step="0.1" aria-label="评分系数" value={term.coefficient} onChange={(e) => onChange({ coefficient: e.target.value })} />
      </span>
      <span className={`badge ${effectClass}`}>{effect}</span>
      <button className="btn icon sm ghost danger" aria-label="删除评分因子" onClick={onRemove}>
        <Icon name="trash" size={13} />
      </button>
    </div>
  )
}

function ExpressionMode({
  filter,
  score,
  filterError,
  scoreError,
  onFilter,
  onScore,
}: {
  filter: string
  score: string
  filterError?: string
  scoreError?: string
  onFilter: (v: string) => void
  onScore: (v: string) => void
}) {
  return (
    <>
      <Notice>
        完整 Expr DSL 模式。filter 返回 bool；score 返回 number 且分数越小越优。支持算术、比较、逻辑、三元表达式、内置函数与动态指标 map。
      </Notice>
      <div className="expression-layout">
        <div className="expression-editors">
          <div className="field">
            <label htmlFor="expr-filter">过滤表达式 filter（留空按 healthy 处理）</label>
            <textarea id="expr-filter" className={filterError ? 'input-error' : undefined} value={filter} spellCheck={false} onChange={(e) => onFilter(e.target.value)} />
            {filterError && <div className="field-error">{filterError}</div>}
          </div>
          <div className="field">
            <label htmlFor="expr-score">代价表达式 score（返回 number，取最小者）</label>
            <textarea id="expr-score" className={scoreError ? 'input-error' : undefined} value={score} spellCheck={false} onChange={(e) => onScore(e.target.value)} />
            {scoreError && <div className="field-error">{scoreError}</div>}
          </div>
        </div>
        <aside className="variable-reference" aria-label="可用变量">
          <h3>可用变量</h3>
          {VARIABLE_GROUPS.map((g) => (
            <div key={g.title} className="variable-group">
              <h4>{g.title}</h4>
              {g.items.map(([name, desc]) => (
                <button key={name} title={`点击复制 ${name}`} onClick={() => void navigator.clipboard?.writeText(name)}>
                  <code>{name}</code>
                  <span>{desc}</span>
                </button>
              ))}
            </div>
          ))}
          <p className="hint">函数：abs / ceil / floor / round / min / max。点击变量可复制。</p>
        </aside>
      </div>
    </>
  )
}
