import { useCallback, useEffect, useState } from 'react'

import { ApiError, fetchStats, type Coverage, type StatsPayload } from '../api'
import { formatStaleness, groupNumber } from '../format'
import { CoverageBar } from './CoverageBar'

interface StatsState {
  stats: StatsPayload
  coverage: Coverage
}

export function StatsPanel() {
  const [state, setState] = useState<StatsState | null>(null)
  const [error, setError] = useState<string | null>(null)
  // 初次渲染即视为加载中：避免先闪一下「暂无数据」再出现数字。
  const [isLoading, setIsLoading] = useState(true)

  const load = useCallback(async () => {
    try {
      const response = await fetchStats()
      setState({ stats: response.data, coverage: response.coverage })
      setError(null)
    } catch (failure) {
      setError(failure instanceof ApiError ? failure.message : String(failure))
    } finally {
      setIsLoading(false)
    }
  }, [])

  useEffect(() => {
    // load 内部第一个 setState 发生在 await 之后，不存在「同步 setState 触发级联渲染」。
    // 这条规则无法穿透 useCallback 分析，故在此放行。
    // oxlint-disable-next-line react/set-state-in-effect
    void load()
  }, [load])

  return (
    <div className="stack">
      {error ? <div className="error">{error}</div> : null}

      {state ? <CoverageBar coverage={state.coverage} /> : null}

      <div className="panel">
        <div className="panel-head">
          <span className="panel-title">索引器状态</span>
          <span className="panel-hint">计数每 10 秒刷新一次（服务端缓存）</span>
          <button
            className="btn"
            style={{ marginLeft: 'auto' }}
            onClick={() => {
              setIsLoading(true)
              void load()
            }}
            disabled={isLoading}
          >
            {isLoading ? '刷新中…' : '刷新'}
          </button>
        </div>

        {state ? (
          <div className="metric-grid">
            <div className="metric">
              <div className="metric-label">日志总数</div>
              <div className="metric-value">{groupNumber(state.stats.total_logs)}</div>
              <div className="metric-note">chain_log 行数</div>
            </div>
            <div className="metric">
              <div className="metric-label">已解码</div>
              <div className="metric-value">{groupNumber(state.stats.decoded_count)}</div>
              <div className="metric-note">
                占比 {((state.stats.decoded_count / Math.max(state.stats.total_logs, 1)) * 100).toFixed(2)}%
              </div>
            </div>
            <div className="metric">
              <div className="metric-label">未解码</div>
              <div className="metric-value">{groupNumber(state.stats.undecodable_count)}</div>
              <div className="metric-note">内置 ABI 时代的存量，不随新数据增长</div>
            </div>
            <div className="metric">
              <div className="metric-label">已索引块数</div>
              <div className="metric-value">
                {groupNumber(state.coverage.to_block - state.coverage.from_block + 1)}
              </div>
              <div className="metric-note">
                确认水位 {groupNumber(state.coverage.finalized_block)}
              </div>
            </div>
            <div className="metric">
              <div className="metric-label">数据新鲜度</div>
              <div className="metric-value">{formatStaleness(state.stats.stale_seconds)}</div>
              <div className="metric-note">索引器停机时这个数字会持续变大</div>
            </div>
          </div>
        ) : (
          <div className="loading">{isLoading ? '读取中…' : '暂无数据'}</div>
        )}
      </div>

      <div className="note">
        全部计数都是「已索引区间内」的统计，不是链上全历史。想看某个地址的情况，切到地址画像。
      </div>
    </div>
  )
}
