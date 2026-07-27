// 展示层格式化工具。

/** pct 比率转百分比字符串（0.4231 -> "42.3%"）。 */
export function pct(v: number | undefined): string {
  if (v === undefined || Number.isNaN(v)) return '-'
  return `${(v * 100).toFixed(1)}%`
}

/** num 数值定点展示，去掉无意义的尾随零。 */
export function num(v: number | undefined, digits = 2): string {
  if (v === undefined || Number.isNaN(v)) return '-'
  if (Number.isInteger(v)) return String(v)
  return v.toFixed(digits)
}

/** bytes 字节数转可读单位。 */
export function bytes(v: number | undefined): string {
  if (v === undefined) return '-'
  const units = ['B', 'KiB', 'MiB', 'GiB']
  let n = v
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

/** meterClass 按占用率给出配色档位：<70% 正常、<90% 警告、其余危险。 */
export function meterClass(ratio: number): string {
  if (ratio >= 0.9) return 'meter err'
  if (ratio >= 0.7) return 'meter warn'
  return 'meter'
}
