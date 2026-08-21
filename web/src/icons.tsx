// 图标：内联 SVG 精简集。
// 不引入图标库——控制台只需十余个图标，内联 path 比任何库的 tree-shaking 结果都小，
// 也不新增供应链依赖（与 web/README 的"无 CDN、可审计"约束一致）。
import type { SVGProps } from 'react'

const PATHS = {
  overview: 'M4 13h6V4H4v9Zm0 7h6v-5H4v5Zm9 0h7v-9h-7v9Zm0-16v5h7V4h-7Z',
  policy: 'M12 3 3 7v5c0 5 3.8 8.4 9 9 5.2-.6 9-4 9-9V7l-9-4Z',
  server: 'M4 5h16v5H4V5Zm0 9h16v5H4v-5Zm3-6.5h.01M7 16.5h.01',
  explain: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 5v4m0 4h.01',
  monitor: 'M3 17l5-7 4 4 3-5 6 8',
  cache: 'M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3Zm0 0v10c0 1.7 3.6 3 8 3s8-1.3 8-3V7',
  bell: 'M18 16V11a6 6 0 1 0-12 0v5l-2 3h16l-2-3Zm-6 6a2.5 2.5 0 0 0 2.5-2.5h-5A2.5 2.5 0 0 0 12 22Z',
  refresh: 'M20 12a8 8 0 1 1-2.5-5.8M20 4v4h-4',
  external: 'M14 4h6v6M20 4l-9 9M18 14v5H5V6h5',
  close: 'M6 6l12 12M18 6 6 18',
  chevronLeft: 'M14 6l-6 6 6 6',
  chevronRight: 'M10 6l6 6-6 6',
  chevronDown: 'M6 9l6 6 6-6',
  search: 'M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14Zm5 -2 5 5',
  plus: 'M12 5v14M5 12h14',
  trash: 'M5 7h14M9 7V5h6v2m-8 0 1 13h8l1-13',
  pause: 'M9 5v14M15 5v14',
  play: 'M8 5l11 7-11 7V5Z',
  alert: 'M12 3 2 20h20L12 3Zm0 6v5m0 3h.01',
  critical: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 4v6m0 3h.01',
  info: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 5h.01M12 11v6',
  check: 'M4 12l5 5 11-11',
  gauge: 'M12 20a8 8 0 1 1 8-8M12 12l4-3',
  latency: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18Zm0 4v5l4 2',
  queue: 'M4 6h16M4 12h16M4 18h10',
  expand: 'M4 9V4h5M20 15v5h-5M4 15v5h5M20 9V4h-5',
  scale: 'M6 20V10m6 10V4m6 16v-6',
} as const

export type IconName = keyof typeof PATHS

/** Icon 线性图标。size 默认 16，颜色继承 currentColor。 */
export function Icon({
  name,
  size = 16,
  ...rest
}: { name: IconName; size?: number } & SVGProps<SVGSVGElement>) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      {...rest}
    >
      <path d={PATHS[name]} />
    </svg>
  )
}
