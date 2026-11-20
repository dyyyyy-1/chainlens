import type { Coverage } from '../api'
import { formatBlockTime, groupNumber } from '../format'

/**
 * CoverageBar 展示「这份数据覆盖到哪里」。
 *
 * 不是装饰：空结果必须能区分「确实没有记录」与「这段历史还没索引」，
 * 否则使用者会把未索引当成「地址没有历史」。
 */
export function CoverageBar({ coverage }: { coverage: Coverage }) {
  const blocks = coverage.to_block - coverage.from_block + 1

  return (
    <div className="coverage">
      <span>
        已索引 <code>{groupNumber(coverage.from_block)}</code> …{' '}
        <code>{groupNumber(coverage.to_block)}</code>
      </span>
      <span>
        共 <code>{groupNumber(blocks)}</code> 块
      </span>
      {coverage.latest_block_time ? (
        <span>
          最新区块时间 <code>{formatBlockTime(coverage.latest_block_time)}</code>
        </span>
      ) : null}
    </div>
  )
}
