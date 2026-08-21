// 轻量 SVG 图表：折线（时序）、柱状（分布）、迷你趋势线。
// 刻意不引入图表库：控制台只有三种图形需求，原生 SVG 体积为零且可审计；
// 复杂交互（缩放、十字准线联动）留给 Grafana，此处只承担"看趋势、定位异常"。
import { useMemo, useState, type ReactNode } from 'react'
import { StateBlock } from './components'

export interface Series {
  name: string
  color: string
  points: Array<{ x: number; y: number }>
  /** dashed 参考线（当前容量、推荐容量）用虚线，与实测序列区分。 */
  dashed?: boolean
}

const PAD = { top: 12, right: 12, bottom: 24, left: 48 }

function niceMax(v: number): number {
  if (v <= 0) return 1
  const mag = Math.pow(10, Math.floor(Math.log10(v)))
  const n = v / mag
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10
  return step * mag
}

/**
 * LineChart 时序折线图。
 * 高度固定、宽度自适应（viewBox + preserveAspectRatio），无数据时给出空态说明。
 * 悬停显示同一时刻各序列取值；同时输出文本摘要供读屏与无悬停设备使用。
 */
export function LineChart({
  series,
  height = 240,
  yFormat,
  xFormat,
  summary,
  empty = '暂无采样数据',
}: {
  series: Series[]
  height?: number
  yFormat: (v: number) => string
  xFormat: (v: number) => string
  summary?: ReactNode
  empty?: string
}) {
  const width = 720
  const [hover, setHover] = useState<number | null>(null)

  const model = useMemo(() => {
    const all = series.flatMap((s) => s.points)
    if (!all.length) return null
    const xs = all.map((p) => p.x)
    const minX = Math.min(...xs)
    const maxX = Math.max(...xs)
    const maxY = niceMax(Math.max(...all.map((p) => p.y)))
    const spanX = maxX - minX || 1
    const px = (x: number) => PAD.left + ((x - minX) / spanX) * (width - PAD.left - PAD.right)
    const py = (y: number) => height - PAD.bottom - (y / maxY) * (height - PAD.top - PAD.bottom)
    return { minX, maxX, maxY, px, py }
  }, [series, height])

  if (!model) return <StateBlock title={empty} detail="采样缓冲为空或该指标暂未接入。" />

  const { minX, maxX, maxY, px, py } = model
  const yTicks = [0, 0.25, 0.5, 0.75, 1].map((r) => maxY * r)
  const xTicks = [0, 0.25, 0.5, 0.75, 1].map((r) => minX + (maxX - minX) * r)
  const hoverX = hover === null ? null : minX + ((maxX - minX) * hover) / 100

  return (
    <div>
      <div className="chart-legend" style={{ marginBottom: 8 }}>
        {series.map((s) => (
          <span className="key" key={s.name} style={{ color: s.color }}>
            <i style={{ background: s.color, height: s.dashed ? 0 : 2, borderTop: s.dashed ? `2px dashed ${s.color}` : undefined }} />
            <span style={{ color: 'var(--text-secondary)' }}>{s.name}</span>
          </span>
        ))}
      </div>
      <svg
        className="chart"
        viewBox={`0 0 ${width} ${height}`}
        height={height}
        preserveAspectRatio="none"
        role="img"
        aria-label={series.map((s) => s.name).join('、')}
        onMouseLeave={() => setHover(null)}
        onMouseMove={(e) => {
          const box = e.currentTarget.getBoundingClientRect()
          const ratio = (e.clientX - box.left) / box.width
          const inner = (ratio * width - PAD.left) / (width - PAD.left - PAD.right)
          setHover(Math.max(0, Math.min(100, inner * 100)))
        }}
      >
        {yTicks.map((t) => (
          <g key={t}>
            <line
              x1={PAD.left}
              x2={width - PAD.right}
              y1={py(t)}
              y2={py(t)}
              stroke="var(--grid-line)"
              strokeWidth="1"
            />
            <text x={PAD.left - 8} y={py(t) + 4} textAnchor="end" fontSize="10" fill="var(--text-muted)">
              {yFormat(t)}
            </text>
          </g>
        ))}
        {xTicks.map((t, i) => (
          <text
            key={i}
            x={px(t)}
            y={height - 8}
            textAnchor={i === 0 ? 'start' : i === xTicks.length - 1 ? 'end' : 'middle'}
            fontSize="10"
            fill="var(--text-muted)"
          >
            {xFormat(t)}
          </text>
        ))}
        {series.map((s) => (
          <polyline
            key={s.name}
            fill="none"
            stroke={s.color}
            strokeWidth="1.8"
            strokeDasharray={s.dashed ? '5 4' : undefined}
            points={s.points.map((p) => `${px(p.x)},${py(p.y)}`).join(' ')}
          />
        ))}
        {hoverX !== null && (
          <line
            x1={px(hoverX)}
            x2={px(hoverX)}
            y1={PAD.top}
            y2={height - PAD.bottom}
            stroke="var(--border-strong)"
            strokeWidth="1"
          />
        )}
      </svg>
      <p className="chart-summary">
        {hoverX !== null
          ? `${xFormat(hoverX)} · ${series
              .map((s) => `${s.name} ${yFormat(nearest(s, hoverX))}`)
              .join(' / ')}`
          : summary}
      </p>
    </div>
  )
}

function nearest(s: Series, x: number): number {
  let best = s.points[0]
  let bestD = Infinity
  for (const p of s.points) {
    const d = Math.abs(p.x - x)
    if (d < bestD) {
      bestD = d
      best = p
    }
  }
  return best?.y ?? 0
}

export interface Bar {
  label: string
  value: number
  color?: string
}

/** BarChart 分布柱状图（按模型/引擎的实例数、队列深度等离散分布）。 */
export function BarChart({
  data,
  height = 240,
  yFormat = (v) => String(v),
  empty = '暂无分布数据',
}: {
  data: Bar[]
  height?: number
  yFormat?: (v: number) => string
  empty?: string
}) {
  if (!data.length) return <StateBlock title={empty} />
  const width = 520
  const maxY = niceMax(Math.max(...data.map((d) => d.value)))
  const inner = width - PAD.left - PAD.right
  const slot = inner / data.length
  const barW = Math.min(48, slot * 0.55)
  const py = (v: number) => height - PAD.bottom - (v / maxY) * (height - PAD.top - PAD.bottom)
  const palette = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)', 'var(--series-4)']

  return (
    <svg
      className="chart"
      viewBox={`0 0 ${width} ${height}`}
      height={height}
      preserveAspectRatio="none"
      role="img"
      aria-label={data.map((d) => `${d.label} ${yFormat(d.value)}`).join('，')}
    >
      {[0, 0.5, 1].map((r) => (
        <g key={r}>
          <line
            x1={PAD.left}
            x2={width - PAD.right}
            y1={py(maxY * r)}
            y2={py(maxY * r)}
            stroke="var(--grid-line)"
          />
          <text
            x={PAD.left - 8}
            y={py(maxY * r) + 4}
            textAnchor="end"
            fontSize="10"
            fill="var(--text-muted)"
          >
            {yFormat(maxY * r)}
          </text>
        </g>
      ))}
      {data.map((d, i) => {
        const cx = PAD.left + slot * i + slot / 2
        return (
          <g key={d.label}>
            <rect
              x={cx - barW / 2}
              y={py(d.value)}
              width={barW}
              height={Math.max(1, height - PAD.bottom - py(d.value))}
              rx="3"
              fill={d.color ?? palette[i % palette.length]}
            />
            <text x={cx} y={py(d.value) - 5} textAnchor="middle" fontSize="10" fill="var(--text-secondary)">
              {yFormat(d.value)}
            </text>
            <text x={cx} y={height - 8} textAnchor="middle" fontSize="10" fill="var(--text-muted)">
              {d.label.length > 14 ? `${d.label.slice(0, 13)}…` : d.label}
            </text>
          </g>
        )
      })}
    </svg>
  )
}

/** Sparkline KPI 卡片内的迷你趋势线，仅在存在真实历史序列时渲染。 */
export function Sparkline({
  values,
  color = 'var(--series-1)',
  width = 96,
  height = 26,
}: {
  values: number[]
  color?: string
  width?: number
  height?: number
}) {
  if (values.length < 2) return null
  const max = Math.max(...values)
  const min = Math.min(...values)
  const span = max - min || 1
  const pts = values
    .map((v, i) => {
      const x = (i / (values.length - 1)) * width
      const y = height - ((v - min) / span) * (height - 3) - 1.5
      return `${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
  return (
    <svg width={width} height={height} aria-hidden="true" focusable="false">
      <polyline fill="none" stroke={color} strokeWidth="1.5" points={pts} />
    </svg>
  )
}
