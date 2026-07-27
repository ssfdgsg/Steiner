// 共享展示组件。
import type { Toast as ToastData } from './hooks'
import { meterClass, pct } from './format'

/** Toast 操作结果提示（右下角浮层）。 */
export function Toast({ toast }: { toast: ToastData | null }) {
  if (!toast) return null
  return <div className={`toast ${toast.kind}`}>{toast.text}</div>
}

/** Meter 占用率条 + 百分比文本，按档位配色（<70% 正常 / <90% 警告 / 其余危险）。 */
export function Meter({ value }: { value: number }) {
  const ratio = Math.max(0, Math.min(1, value || 0))
  return (
    <span>
      <span className={meterClass(ratio)}>
        <span style={{ width: `${ratio * 100}%` }} />
      </span>
      <span className="num">{pct(value)}</span>
    </span>
  )
}

/** StateBadge 后端状态徽标：隔离/熔断优先于健康展示（更贴近运维关注点）。 */
export function StateBadge({
  healthy,
  cordoned,
  ejected,
}: {
  healthy: boolean
  cordoned: boolean
  ejected: boolean
}) {
  if (cordoned) return <span className="badge warn">已隔离</span>
  if (ejected) return <span className="badge err">熔断中</span>
  if (!healthy) return <span className="badge err">不健康</span>
  return <span className="badge ok">健康</span>
}

/** Section 带标题的卡片容器。 */
export function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="card">
      <h2>{title}</h2>
      {children}
    </div>
  )
}

/** Empty 空态/加载态/错误态的统一展示。 */
export function Empty({ text }: { text: string }) {
  return <div className="empty">{text}</div>
}
