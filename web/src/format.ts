// 展示层格式化工具。

/** pct 比率转百分比字符串（0.4231 -> "42.3%"）。 */
export function pct(v: number | undefined, digits = 1): string {
  if (v === undefined || v === null || Number.isNaN(v)) return '-'
  return `${(v * 100).toFixed(digits)}%`
}

/** num 数值定点展示，去掉无意义的尾随零。 */
export function num(v: number | undefined, digits = 2): string {
  if (v === undefined || v === null || Number.isNaN(v)) return '-'
  if (Number.isInteger(v)) return String(v)
  return v.toFixed(digits)
}

/** bytes 字节数转可读单位。 */
export function bytes(v: number | undefined): string {
  if (v === undefined || v === null || Number.isNaN(v)) return '-'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let n = v
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

/** compact 大数值紧凑展示（43210 -> "43.2K"），用于 KPI 主值。 */
export function compact(v: number | undefined): string {
  if (v === undefined || v === null || Number.isNaN(v)) return '-'
  const abs = Math.abs(v)
  if (abs >= 1e9) return `${(v / 1e9).toFixed(1)}B`
  if (abs >= 1e6) return `${(v / 1e6).toFixed(1)}M`
  if (abs >= 1e4) return `${(v / 1e3).toFixed(1)}K`
  if (Number.isInteger(v)) return String(v)
  return v.toFixed(abs >= 100 ? 0 : 1)
}

/** ms 毫秒数展示：<1s 用 ms，>=1s 换算为 s（避免 KPI 出现 5 位数字）。 */
export function ms(v: number | undefined): string {
  if (v === undefined || v === null || Number.isNaN(v)) return '-'
  if (v >= 1000) return `${(v / 1000).toFixed(2)}s`
  if (v >= 100) return `${v.toFixed(0)}ms`
  return `${v.toFixed(1)}ms`
}

/** duration 秒数转「3天4小时」形式（进程运行时长展示）。 */
export function duration(sec: number | undefined): string {
  if (sec === undefined || Number.isNaN(sec)) return '-'
  const s = Math.floor(sec)
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分`
  if (m > 0) return `${m} 分 ${s % 60} 秒`
  return `${s} 秒`
}

/** clock 时间戳转本地时刻（HH:MM:SS），解析失败返回原串。 */
export function clock(iso: string | undefined): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

/** stamp 时间戳转本地日期时间，用于表格列与 title 提示。 */
export function stamp(iso: string | undefined): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('zh-CN', { hour12: false })
}

/** ago 相对时间（"12 秒前"），用于「最后更新」与告警持续时长。 */
export function ago(iso: string | undefined): string {
  if (!iso) return '-'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return iso
  const sec = Math.max(0, Math.round((Date.now() - t) / 1000))
  if (sec < 60) return `${sec} 秒前`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`
  return `${Math.floor(sec / 86400)} 天前`
}

/** level 占用率档位：normal <70% / warn <90% / danger 其余。表格与进度条共用。 */
export type Level = 'normal' | 'warn' | 'danger'

export function level(ratio: number | undefined): Level {
  if (ratio === undefined || Number.isNaN(ratio)) return 'normal'
  if (ratio >= 0.9) return 'danger'
  if (ratio >= 0.7) return 'warn'
  return 'normal'
}
