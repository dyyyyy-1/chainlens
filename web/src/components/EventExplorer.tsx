import { useCallback, useEffect, useState, type FormEvent } from 'react'

import {
  ApiError,
  fetchEvents,
  fetchExport,
  isAddress,
  type Coverage,
  type EventItem,
} from '../api'
import { formatBlockTime, formatUnits, groupNumber, shortHex } from '../format'
import { CoverageBar } from './CoverageBar'

interface Filters {
  contract: string
  from: string
  to: string
  limit: string
}

const DEFAULT_LIMIT = '50'

/** MAX_UINT256 是 2^256-1，ERC20 用它表示无限额度。 */
const MAX_UINT256 = '115792089237316195423570985008687907853269984665640564039457584007913129639935'

/** formatByEvents 按事件类型挑字段，避免把整段 JSON 摔在表格里。 */
function payloadSummary(item: EventItem): string {
  const payload = item.payload
  if (!payload) {
    return '—'
  }

  const { from, to, owner, spender, value } = payload

  // Transfer：from → to 金额
  if (typeof from === 'string' && typeof to === 'string' && typeof value === 'string') {
    return `${shortHex(from)} → ${shortHex(to)}  ${formatUnits(value)}`
  }

  // Approval：owner → spender 额度（无限额度单独标注，否则会是一串 78 位数字）
  if (typeof owner === 'string' && typeof spender === 'string' && typeof value === 'string') {
    const amount = value === MAX_UINT256 ? '无限额度' : formatUnits(value)
    return `${shortHex(owner)} → ${shortHex(spender)}  ${amount}`
  }

  const json = JSON.stringify(payload)
  return json.length > 90 ? `${json.slice(0, 90)}…` : json
}

interface EventExplorerProps {
  initialFilters?: Partial<Filters>
  onFiltersChange?: (filters: Filters) => void
}

export function EventExplorer({ initialFilters, onFiltersChange }: EventExplorerProps) {
  const [filters, setFilters] = useState<Filters>({
    contract: initialFilters?.contract ?? '',
    from: initialFilters?.from ?? '',
    to: initialFilters?.to ?? '',
    limit: DEFAULT_LIMIT,
  })
  const [isExporting, setIsExporting] = useState(false)
  const [items, setItems] = useState<EventItem[]>([])
  const [coverage, setCoverage] = useState<Coverage | null>(null)
  const [cursor, setCursor] = useState<string>('')
  const [hasMore, setHasMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // 初次渲染即视为加载中：进入事件页会立刻拉第一页。
  const [isLoading, setIsLoading] = useState(true)

  const load = useCallback(
    async (nextCursor: string) => {
      setError(null)

      try {
        const contract = filters.contract.trim()
        if (contract && !isAddress(contract)) {
          setError('合约地址格式不对：需要 0x 开头的 40 位十六进制')
          return
        }

        const limit = Number(filters.limit)
        if (!Number.isInteger(limit) || limit <= 0) {
          setError('limit 必须是正整数')
          return
        }

        const response = await fetchEvents({
          contract: contract || undefined,
          from: filters.from.trim() || undefined,
          to: filters.to.trim() || undefined,
          limit,
          // 翻页只认游标：offset 在深翻页时会退化，界面上不再暴露。
          cursor: nextCursor || undefined,
        })

        setCoverage(response.coverage)
        setItems((previous) => (nextCursor ? [...previous, ...response.data] : response.data))
        setCursor(response.next_cursor ?? '')
        // 后端只在填满一页时给游标，所以「有游标」就等于「可能还有」。
        setHasMore(Boolean(response.next_cursor))
      } catch (failure) {
        setError(failure instanceof ApiError ? failure.message : String(failure))
      } finally {
        setIsLoading(false)
      }
    },
    [filters],
  )

  useEffect(() => {
    void load('')
    // 首次进入加载一次；后续由「查询」按钮或「加载更多」触发。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setIsLoading(true)
    // 只把过滤条件写进 URL：翻页游标是「读到哪了」，不是可分享的状态。
    onFiltersChange?.(filters)
    void load('')
  }

  async function handleExport(format: 'csv' | 'json') {
    setError(null)
    setIsExporting(true)
    try {
      const contract = filters.contract.trim()
      if (contract && !isAddress(contract)) {
        setError('合约地址格式不对：需要 0x 开头的 40 位十六进制')
        return
      }

      // 导出覆盖整个过滤范围，与页面上的分页无关。
      const { blob, filename } = await fetchExport(
        {
          contract: contract || undefined,
          from: filters.from.trim() || undefined,
          to: filters.to.trim() || undefined,
        },
        format,
      )

      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = filename
      document.body.appendChild(link)
      link.click()
      link.remove()
      URL.revokeObjectURL(url)
    } catch (failure) {
      setError(failure instanceof ApiError ? failure.message : String(failure))
    } finally {
      setIsExporting(false)
    }
  }

  return (
    <div className="stack">
      <form className="panel" onSubmit={handleSubmit}>
        <div className="panel-head">
          <span className="panel-title">事件检索</span>
          <span className="panel-hint">按区块倒序，游标翻页</span>
        </div>

        <div className="field-row">
          <div className="field field-grow">
            <label htmlFor="contract">合约（可空，空表示全部）</label>
            <input
              id="contract"
              value={filters.contract}
              onChange={(event) => setFilters({ ...filters, contract: event.target.value })}
              placeholder="0x…"
              spellCheck={false}
              autoComplete="off"
            />
          </div>
          <div className="field field-narrow">
            <label htmlFor="from">起始块</label>
            <input
              id="from"
              value={filters.from}
              onChange={(event) => setFilters({ ...filters, from: event.target.value })}
              placeholder="可选"
              inputMode="numeric"
            />
          </div>
          <div className="field field-narrow">
            <label htmlFor="to">结束块</label>
            <input
              id="to"
              value={filters.to}
              onChange={(event) => setFilters({ ...filters, to: event.target.value })}
              placeholder="可选"
              inputMode="numeric"
            />
          </div>
          <div className="field field-narrow">
            <label htmlFor="limit">每页条数</label>
            <input
              id="limit"
              value={filters.limit}
              onChange={(event) => setFilters({ ...filters, limit: event.target.value })}
              inputMode="numeric"
            />
          </div>
          <button className="btn btn-primary" type="submit" disabled={isLoading}>
            {isLoading ? '查询中…' : '查询'}
          </button>
        </div>
      </form>

      {error ? <div className="error">{error}</div> : null}
      {coverage ? <CoverageBar coverage={coverage} /> : null}

      <div className="panel">
        <div className="panel-head">
          <span className="panel-title">结果</span>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <span className="panel-hint">已加载 {groupNumber(items.length)} 条</span>
            <button
              className="btn"
              type="button"
              onClick={() => void handleExport('csv')}
              disabled={isExporting}
            >
              {isExporting ? '导出中…' : '导出 CSV'}
            </button>
            <button
              className="btn"
              type="button"
              onClick={() => void handleExport('json')}
              disabled={isExporting}
            >
              导出 JSON
            </button>
          </div>
        </div>

        {items.length === 0 ? (
          <div className="empty">
            {isLoading ? '读取中…' : '没有匹配的事件。注意：只覆盖已索引区间，且上限 200 条一页'}
          </div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>区块</th>
                  <th>时间</th>
                  <th>事件</th>
                  <th>参数</th>
                  <th>交易</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={`${item.tx_hash}-${item.log_index}`}>
                    <td className="mono">{groupNumber(item.block_number)}</td>
                    <td className="mono">{formatBlockTime(item.block_time)}</td>
                    <td>
                      {item.event ? (
                        <span className="tag">{item.event}</span>
                      ) : (
                        <span className="tag tag-muted">{item.status || 'unknown'}</span>
                      )}
                    </td>
                    <td className="mono">{payloadSummary(item)}</td>
                    <td className="mono" title={item.tx_hash}>
                      {shortHex(item.tx_hash)}#{item.log_index}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        {hasMore ? (
          <div style={{ marginTop: 14, textAlign: 'center' }}>
            <button
              className="btn"
              onClick={() => {
                setIsLoading(true)
                void load(cursor)
              }}
              disabled={isLoading}
            >
              {isLoading ? '加载中…' : '加载更多'}
            </button>
          </div>
        ) : null}

        <div className="note">
          事件参数里的金额同样按 6 位小数展示。翻页用键集游标：从上一页最后一行继续，
          不会因为并发写入而跳行。导出覆盖<strong>当前过滤条件下的全部事件</strong>（上限 20 万行），
          与页面上的分页无关；超出上限会提示缩小区块范围。
        </div>
      </div>
    </div>
  )
}
