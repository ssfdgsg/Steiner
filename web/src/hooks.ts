// 通用 hooks：轮询拉取与轻量提示。
import { useCallback, useEffect, useRef, useState } from 'react'

export interface PollState<T> {
  data: T | null
  error: string | null
  loading: boolean
  /** refresh 立即重新拉取（操作成功后调用，避免等下一个轮询周期）。 */
  refresh: () => void
}

/**
 * usePoll 周期拉取只读接口。
 * 首次加载显示 loading，之后的轮询静默刷新（避免界面闪烁）；
 * 组件卸载后丢弃在途响应，防止 setState 泄漏。
 */
export function usePoll<T>(fetcher: () => Promise<T>, intervalMs = 3000): PollState<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)
  const aliveRef = useRef(true)
  // fetcher 通常是内联箭头函数，用 ref 固定，避免作为 effect 依赖导致轮询重启。
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  const refresh = useCallback(() => setTick((n) => n + 1), [])

  useEffect(() => {
    aliveRef.current = true
    let timer: number | undefined

    const run = async () => {
      try {
        const d = await fetcherRef.current()
        if (!aliveRef.current) return
        setData(d)
        setError(null)
      } catch (e) {
        if (!aliveRef.current) return
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        if (aliveRef.current) setLoading(false)
      }
    }

    void run()
    if (intervalMs > 0) timer = window.setInterval(run, intervalMs)
    return () => {
      aliveRef.current = false
      if (timer) window.clearInterval(timer)
    }
  }, [intervalMs, tick])

  return { data, error, loading, refresh }
}

export interface Toast {
  kind: 'ok' | 'err'
  text: string
}

/** useToast 操作结果提示，3 秒自动消失。 */
export function useToast() {
  const [toast, setToast] = useState<Toast | null>(null)
  const timerRef = useRef<number | undefined>(undefined)

  const show = useCallback((kind: Toast['kind'], text: string) => {
    setToast({ kind, text })
    if (timerRef.current) window.clearTimeout(timerRef.current)
    timerRef.current = window.setTimeout(() => setToast(null), 3000)
  }, [])

  useEffect(() => () => window.clearTimeout(timerRef.current), [])

  return { toast, show }
}
