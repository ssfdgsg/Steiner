// 共享展示组件：面板、KPI、表格、状态块、抽屉、确认弹窗与状态徽标。
// 所有页面只允许通过本文件的组件构建骨架，保证跨页面视觉与交互一致。
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import type { PollState, Toast as ToastData } from './hooks'
import { useEscape } from './hooks'
import { ago, level, pct, type Level } from './format'
import { Icon, type IconName } from './icons'

/** Toast 操作结果提示（右下角浮层）。 */
export function Toast({ toast }: { toast: ToastData | null }) {
  if (!toast) return null
  return (
    <div className={`toast ${toast.kind}`} role="status" aria-live="polite">
      {toast.text}
    </div>
  )
}

/** Panel 数据面板：标题 + 工具区 + 内容。禁止嵌套 Panel。 */
export function Panel({
  title,
  subtitle,
  tools,
  children,
  flush,
}: {
  title: string
  subtitle?: string
  tools?: ReactNode
  children: ReactNode
  /** flush 内容自带内边距（表格、筛选栏）时传 true。 */
  flush?: boolean
}) {
  return (
    <section className="panel">
      <div className="panel-head">
        <h2 className="panel-title">
          {title}
          {subtitle && <small>{subtitle}</small>}
        </h2>
        {tools && <div className="panel-tools">{tools}</div>}
      </div>
      <div className={flush ? 'panel-body flush' : 'panel-body'}>{children}</div>
    </section>
  )
}

/** MetricCard KPI 卡片。value 已格式化；趋势与脚注仅在有真实数据时传入。 */
export function MetricCard({
  label,
  value,
  unit,
  icon,
  tone = 'accent',
  foot,
  hint,
}: {
  label: string
  value: string
  unit?: string
  icon: IconName
  tone?: 'accent' | 'success' | 'warning' | 'danger' | 'info'
  foot?: ReactNode
  /** hint 指标口径说明，挂在标题的 title 上（不承载必要信息）。 */
  hint?: string
}) {
  return (
    <div className="kpi">
      <div className="kpi-head">
        <span className={`kpi-icon ${tone}`}>
          <Icon name={icon} />
        </span>
        <span title={hint}>{label}</span>
      </div>
      <div className="kpi-value">
        {value}
        {unit && <span className="unit">{unit}</span>}
      </div>
      <div className="kpi-foot">{foot ?? <span>—</span>}</div>
    </div>
  )
}

/** UsageBar 占用率条：宽度固定，同时给出数值与档位配色。 */
export function UsageBar({ value, label }: { value: number | undefined; label?: string }) {
  const ratio = Math.max(0, Math.min(1, value ?? 0))
  const lv: Level = level(value)
  const cls = lv === 'danger' ? 'bar danger' : lv === 'warn' ? 'bar warn' : 'bar'
  return (
    <span className="bar-cell">
      <span
        className={cls}
        role="img"
        aria-label={`${label ?? '占用率'} ${pct(value)}`}
        title={`${label ?? '占用率'} ${pct(value)}`}
      >
        <span style={{ width: `${ratio * 100}%` }} />
      </span>
      <span className="num">{pct(value)}</span>
    </span>
  )
}

/** StatusBadge 后端主状态：隔离 > 熔断 > 不健康 > 可用，同时用图标与文本表达。 */
export function StatusBadge({
  healthy,
  cordoned,
  ejected,
}: {
  healthy: boolean
  cordoned: boolean
  ejected: boolean
}) {
  if (cordoned)
    return (
      <span className="badge warn">
        <Icon name="pause" size={12} />
        已隔离
      </span>
    )
  if (ejected)
    return (
      <span className="badge err">
        <Icon name="alert" size={12} />
        熔断中
      </span>
    )
  if (!healthy)
    return (
      <span className="badge err">
        <Icon name="critical" size={12} />
        不健康
      </span>
    )
  return (
    <span className="badge ok">
      <Icon name="check" size={12} />
      可用
    </span>
  )
}

export type Severity = 'critical' | 'warning' | 'info'

/** severityOf 归一化后端告警级别到三档（未知按 warning 处理，避免漏看）。 */
export function severityOf(raw: string | undefined): Severity {
  const s = (raw ?? '').toLowerCase()
  if (s === 'critical' || s === 'crit' || s === 'fatal' || s === 'page') return 'critical'
  if (s === 'info' || s === 'none' || s === 'notice') return 'info'
  return 'warning'
}

const SEVERITY_TEXT: Record<Severity, string> = {
  critical: '严重',
  warning: '警告',
  info: '信息',
}

const SEVERITY_ICON: Record<Severity, IconName> = {
  critical: 'critical',
  warning: 'alert',
  info: 'info',
}

/** SeverityBadge 告警级别徽标。 */
export function SeverityBadge({ severity }: { severity: Severity }) {
  const cls = severity === 'critical' ? 'err' : severity === 'warning' ? 'warn' : 'info'
  return (
    <span className={`badge ${cls}`}>
      <Icon name={SEVERITY_ICON[severity]} size={12} />
      {SEVERITY_TEXT[severity]}
    </span>
  )
}

/** FeatureBadge 能力开关状态（后端 enabled 字段）。 */
export function FeatureBadge({ enabled }: { enabled: boolean | undefined }) {
  return enabled ? (
    <span className="badge ok">已启用</span>
  ) : (
    <span className="badge muted">未启用</span>
  )
}

/** StateBlock 空态 / 错误态 / 加载态的统一展示。 */
export function StateBlock({
  kind = 'empty',
  title,
  detail,
  action,
}: {
  kind?: 'empty' | 'error' | 'loading' | 'disabled'
  title: string
  detail?: string
  action?: ReactNode
}) {
  return (
    <div className={kind === 'error' ? 'state error' : 'state'}>
      <strong>{title}</strong>
      {detail && <span>{detail}</span>}
      {action}
    </div>
  )
}

/** SkeletonRows 首屏骨架，保留布局骨骼避免整页 Spinner。 */
export function SkeletonRows({ rows = 4, height = 22 }: { rows?: number; height?: number }) {
  return (
    <div className="stack" style={{ gap: 8, padding: 4 }}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton" style={{ height }} />
      ))}
    </div>
  )
}

/** Notice 页面内提示条（接口错误、能力未接入说明）。 */
export function Notice({
  kind = 'info',
  children,
  action,
}: {
  kind?: 'info' | 'warn' | 'err'
  children: ReactNode
  action?: ReactNode
}) {
  return (
    <div className={kind === 'info' ? 'notice' : `notice ${kind}`} role="status">
      <Icon name={kind === 'err' ? 'critical' : kind === 'warn' ? 'alert' : 'info'} />
      <span>{children}</span>
      {action}
    </div>
  )
}

/**
 * Async 按 PollState 渲染异步态：首屏骨架 → 错误（带重试）→ 内容。
 * 轮询失败时若已有旧数据，保留内容并在上方提示，避免图表被清空。
 */
export function Async<T>({
  state,
  children,
  skeletonRows,
}: {
  state: PollState<T>
  children: (data: T) => ReactNode
  skeletonRows?: number
}) {
  if (state.loading && !state.data) return <SkeletonRows rows={skeletonRows} />
  if (state.error && !state.data)
    return (
      <StateBlock
        kind="error"
        title="接口请求失败"
        detail={state.error}
        action={
          <button className="btn sm" onClick={state.refresh}>
            <Icon name="refresh" size={13} />
            重试
          </button>
        }
      />
    )
  if (!state.data) return <StateBlock title="暂无数据" />
  return (
    <>
      {state.error && (
        <div style={{ marginBottom: 12 }}>
          <Notice kind="warn" action={<button className="btn sm" onClick={state.refresh}>重试</button>}>
            数据可能已过期（最近一次刷新失败：{state.error}）
          </Notice>
        </div>
      )}
      {children(state.data)}
    </>
  )
}

/** Freshness 最后更新时间与刷新中指示。 */
export function Freshness({ state }: { state: PollState<unknown> }) {
  return (
    <span className="hint" title={state.updatedAt?.toLocaleString('zh-CN', { hour12: false })}>
      {state.refreshing ? '刷新中…' : state.updatedAt ? `更新于 ${ago(state.updatedAt.toISOString())}` : '尚未获取'}
    </span>
  )
}

export interface Column<T> {
  key: string
  title: string
  /** align 数值列右对齐，提升可比性。 */
  align?: 'left' | 'right'
  render: (row: T) => ReactNode
  /** sortValue 提供后该列可排序。 */
  sortValue?: (row: T) => number | string
  /** width 表头最小宽度，用于关键列不被压缩。 */
  width?: number
}

/**
 * DataTable 通用只读表格：点击列头排序，行可选中（打开详情抽屉）。
 * 行内按钮需自行 stopPropagation，组件不代劳（避免掩盖误绑定）。
 */
export function DataTable<T>({
  columns,
  rows,
  rowKey,
  selectedKey,
  onSelect,
  empty = '暂无数据',
  defaultSort,
}: {
  columns: Column<T>[]
  rows: T[]
  rowKey: (row: T) => string
  selectedKey?: string | null
  onSelect?: (row: T) => void
  empty?: string
  defaultSort?: { key: string; desc?: boolean }
}) {
  const [sort, setSort] = useState<{ key: string; desc: boolean } | null>(
    defaultSort ? { key: defaultSort.key, desc: !!defaultSort.desc } : null,
  )

  const sorted = useMemo(() => {
    if (!sort) return rows
    const col = columns.find((c) => c.key === sort.key)
    if (!col?.sortValue) return rows
    const pick = col.sortValue
    return [...rows].sort((a, b) => {
      const va = pick(a)
      const vb = pick(b)
      const r = typeof va === 'number' && typeof vb === 'number' ? va - vb : String(va).localeCompare(String(vb))
      return sort.desc ? -r : r
    })
  }, [rows, sort, columns])

  if (!rows.length) return <StateBlock title={empty} />

  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            {columns.map((c) => (
              <th
                key={c.key}
                className={c.sortValue ? 'sortable' : undefined}
                style={{
                  textAlign: c.align === 'right' ? 'right' : 'left',
                  minWidth: c.width,
                }}
                aria-sort={sort?.key === c.key ? (sort.desc ? 'descending' : 'ascending') : undefined}
                onClick={() => {
                  if (!c.sortValue) return
                  setSort((prev) =>
                    prev?.key === c.key ? { key: c.key, desc: !prev.desc } : { key: c.key, desc: true },
                  )
                }}
              >
                {c.title}
                {sort?.key === c.key && (sort.desc ? ' ↓' : ' ↑')}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((row) => {
            const key = rowKey(row)
            const cls = [onSelect ? 'selectable' : '', selectedKey === key ? 'selected' : '']
              .filter(Boolean)
              .join(' ')
            return (
              <tr
                key={key}
                className={cls || undefined}
                onClick={onSelect ? () => onSelect(row) : undefined}
                tabIndex={onSelect ? 0 : undefined}
                onKeyDown={
                  onSelect
                    ? (e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault()
                          onSelect(row)
                        }
                      }
                    : undefined
                }
              >
                {columns.map((c) => (
                  <td key={c.key} style={{ textAlign: c.align === 'right' ? 'right' : 'left' }}>
                    {c.render(row)}
                  </td>
                ))}
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

/** Drawer 右侧详情抽屉：Esc 关闭，关闭后焦点回到触发元素。 */
export function Drawer({
  title,
  subtitle,
  onClose,
  children,
}: {
  title: string
  subtitle?: string
  onClose: () => void
  children: ReactNode
}) {
  useEscape(onClose)
  const closeRef = useRef<HTMLButtonElement>(null)
  const openerRef = useRef<Element | null>(null)
  useEffect(() => {
    openerRef.current = document.activeElement
    closeRef.current?.focus()
    return () => {
      if (openerRef.current instanceof HTMLElement) openerRef.current.focus()
    }
  }, [])
  return (
    <>
      <div className="scrim" onClick={onClose} />
      <aside className="drawer" role="dialog" aria-label={title} aria-modal="true">
        <div className="drawer-head">
          <div style={{ minWidth: 0 }}>
            <h2>{title}</h2>
            {subtitle && <div className="hint mono">{subtitle}</div>}
          </div>
          <button
            ref={closeRef}
            className="btn icon sm ghost"
            onClick={onClose}
            aria-label="关闭详情"
            style={{ marginLeft: 'auto' }}
          >
            <Icon name="close" size={14} />
          </button>
        </div>
        <div className="drawer-body">{children}</div>
      </aside>
    </>
  )
}

/** DrawerSection 抽屉内的分组。 */
export function DrawerSection({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="drawer-section">
      <h3>{title}</h3>
      {children}
    </div>
  )
}

/** KVList 键值明细列表。 */
export function KVList({ items }: { items: Array<[string, ReactNode]> }) {
  return (
    <dl className="kv">
      {items.map(([k, v]) => (
        <div key={k} style={{ display: 'contents' }}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  )
}

/** ConfirmDialog 危险操作确认：说明影响范围，主按钮明确后果。 */
export function ConfirmDialog({
  title,
  confirmText,
  danger,
  busy,
  onConfirm,
  onCancel,
  children,
}: {
  title: string
  confirmText: string
  danger?: boolean
  busy?: boolean
  onConfirm: () => void
  onCancel: () => void
  children: ReactNode
}) {
  useEscape(() => {
    if (!busy) onCancel()
  })
  const ref = useRef<HTMLButtonElement>(null)
  useEffect(() => ref.current?.focus(), [])
  return (
    <>
      <div className="scrim" onClick={busy ? undefined : onCancel} />
      <div className="dialog" role="dialog" aria-modal="true" aria-label={title}>
        <div className="dialog-head">{title}</div>
        <div className="dialog-body">{children}</div>
        <div className="dialog-foot">
          <button ref={ref} className="btn ghost" onClick={onCancel} disabled={busy}>
            取消
          </button>
          <button
            className={danger ? 'btn danger' : 'btn primary'}
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? '执行中…' : confirmText}
          </button>
        </div>
      </div>
    </>
  )
}

/** Segmented 分段选择器（时间范围、分组维度）。 */
export function Segmented<T extends string | number>({
  value,
  options,
  onChange,
  ariaLabel,
}: {
  value: T
  options: Array<{ value: T; label: string }>
  onChange: (v: T) => void
  ariaLabel: string
}) {
  return (
    <div className="seg" role="group" aria-label={ariaLabel}>
      {options.map((o) => (
        <button
          key={String(o.value)}
          className={o.value === value ? 'active' : undefined}
          aria-pressed={o.value === value}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}
