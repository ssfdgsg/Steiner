// 通用 hooks：轮询拉取、全局刷新上下文与轻量提示。
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react'

export interface PollState<T> {
  data: T | null
  error: string | null
  /** loading 仅首次加载为 true，后续轮询静默刷新。 */
  loading: boolean
  /** refreshing 存在在途请求（含轮询），用于顶栏「刷新中」指示。 */
  refreshing: boolean
  /** updatedAt 最近一次成功拉取的时刻，null 表示尚无成功数据。 */
  updatedAt: Date | null
  /** refresh 立即重新拉取（写操作成功后调用，避免等下一个轮询周期）。 */
  refresh: () => void
}

/**
 * usePoll 周期拉取只读接口。
 * - 首次加载显示 loading，之后静默刷新，失败时保留上一次数据（避免图表清空）；
 * - 页面不可见时暂停轮询，重新可见立即补一次，避免后台标签页持续打管理接口；
 * - nonce 变化视为外部强制刷新（顶栏「立即刷新」按钮）；
 * - 组件卸载后丢弃在途响应，防止 setState 泄漏。
 */
export function usePoll<T>(fetcher: () => Promise<T>, intervalMs = 5000, nonce = 0): PollState<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  const [tick, setTick] = useState(0)
  const aliveRef = useRef(true)
  const inflightRef = useRef(false)
  // fetcher 通常是内联箭头函数，用 ref 固定，避免作为 effect 依赖导致轮询重启。
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  const refresh = useCallback(() => setTick((n) => n + 1), [])

  useEffect(() => {
    aliveRef.current = true
    let timer: number | undefined

    const run = async () => {
      // 同一时刻只允许一个在途请求，避免慢接口下轮询叠加。
      if (inflightRef.current) return
      inflightRef.current = true
      setRefreshing(true)
      try {
        const d = await fetcherRef.current()
        if (!aliveRef.current) return
        setData(d)
        setError(null)
        setUpdatedAt(new Date())
      } catch (e) {
        if (!aliveRef.current) return
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        inflightRef.current = false
        if (aliveRef.current) {
          setLoading(false)
          setRefreshing(false)
        }
      }
    }

    void run()
    if (intervalMs > 0) {
      timer = window.setInterval(() => {
        if (document.hidden) return
        void run()
      }, intervalMs)
    }
    const onVisible = () => {
      if (!document.hidden) void run()
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      aliveRef.current = false
      document.removeEventListener('visibilitychange', onVisible)
      if (timer) window.clearInterval(timer)
    }
  }, [intervalMs, tick, nonce])

  return { data, error, loading, refreshing, updatedAt, refresh }
}

/** RefreshControl 全局刷新配置：顶栏统一控制各页面的轮询频率与手动刷新。 */
export interface RefreshControl {
  /** intervalMs 生效轮询间隔，0 表示已关闭自动刷新。 */
  intervalMs: number
  /** selectedMs 用户选中的频率，关闭自动刷新时仍保留，供顶栏下拉框回显。 */
  selectedMs: number
  auto: boolean
  nonce: number
  setIntervalMs: (v: number) => void
  setAuto: (v: boolean) => void
  bump: () => void
}

const RefreshCtx = createContext<RefreshControl>({
  intervalMs: 5000,
  selectedMs: 5000,
  auto: true,
  nonce: 0,
  setIntervalMs: () => {},
  setAuto: () => {},
  bump: () => {},
})

export const RefreshProvider = RefreshCtx.Provider

export function useRefreshControl(): RefreshControl {
  return useContext(RefreshCtx)
}

/**
 * useRefreshState 构造全局刷新配置（仅 AppShell 使用）。
 * 各页面按数据性质对基准频率取倍率（实例负载 1x、KV 规模 2x 等），
 * 统一受顶栏的自动刷新开关与频率选择控制。
 */
export function useRefreshState(defaultInterval = 5000): RefreshControl {
  const [selectedMs, setIntervalMs] = useState(defaultInterval)
  const [auto, setAuto] = useState(true)
  const [nonce, setNonce] = useState(0)
  const bump = useCallback(() => setNonce((n) => n + 1), [])
  return useMemo(
    () => ({
      intervalMs: auto ? selectedMs : 0,
      selectedMs,
      auto,
      nonce,
      setIntervalMs,
      setAuto,
      bump,
    }),
    [auto, selectedMs, nonce, bump],
  )
}

/** useInterval 按倍率派生本页轮询间隔（自动刷新关闭时返回 0）。 */
export function useInterval(multiplier = 1): number {
  const { intervalMs } = useRefreshControl()
  return intervalMs > 0 ? Math.round(intervalMs * multiplier) : 0
}

export interface Toast {
  kind: 'ok' | 'err'
  text: string
}

/** useToast 操作结果提示，3.2 秒自动消失。 */
export function useToast() {
  const [toast, setToast] = useState<Toast | null>(null)
  const timerRef = useRef<number | undefined>(undefined)

  const show = useCallback((kind: Toast['kind'], text: string) => {
    setToast({ kind, text })
    if (timerRef.current) window.clearTimeout(timerRef.current)
    timerRef.current = window.setTimeout(() => setToast(null), 3200)
  }, [])

  useEffect(() => () => window.clearTimeout(timerRef.current), [])

  return { toast, show }
}

/** useEscape 绑定 Esc 关闭（抽屉与弹窗共用）。 */
export function useEscape(onClose: () => void, active = true) {
  const ref = useRef(onClose)
  ref.current = onClose
  useEffect(() => {
    if (!active) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') ref.current()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [active])
}
