import { useState, type FormEvent } from 'react'

import {
  ApiError,
  fetchApprovals,
  fetchBalances,
  fetchInteractions,
  isAddress,
  type Approval,
  type Coverage,
  type Holding,
  type Interaction,
  type Onchain,
  type OnchainBalance,
} from '../api'
import { formatAmount, formatBlockTime, formatUnits, groupNumber, shortHex } from '../format'
import { CoverageBar } from './CoverageBar'

interface ProfileState {
  coverage: Coverage
  holdings: Holding[]
  approvals: Approval[]
  interactions: Interaction[]
  onchain: Onchain
}

export function AddressProfile({ initialAddress }: { initialAddress: string }) {
  const [address, setAddress] = useState(initialAddress)
  const [state, setState] = useState<ProfileState | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [isLoading, setIsLoading] = useState(false)

  async function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setError(null)

    const trimmed = address.trim()
    if (!isAddress(trimmed)) {
      setError('地址格式不对：需要 0x 开头的 40 位十六进制')
      return
    }

    setIsLoading(true)
    try {
      // 三个查询互不依赖，并行发起；任一失败都能立刻看到原因。
      const [balances, approvals, interactions] = await Promise.all([
        fetchBalances(trimmed),
        fetchApprovals(trimmed),
        fetchInteractions(trimmed),
      ])
      setState({
        coverage: balances.coverage,
        holdings: balances.data,
        approvals: approvals.data,
        interactions: interactions.data,
        // 旧后端没有 onchain 字段，按「未启用」处理，不能当成余额为零。
        onchain: balances.onchain ?? { enabled: false },
      })
    } catch (failure) {
      setState(null)
      setError(failure instanceof ApiError ? failure.message : String(failure))
    } finally {
      setIsLoading(false)
    }
  }

  return (
    <div className="stack">
      <form className="panel" onSubmit={(event) => void handleSubmit(event)}>
        <div className="panel-head">
          <span className="panel-title">查询地址</span>
          <span className="panel-hint">持仓、授权、交互都来自已索引区间</span>
        </div>

        <div className="field-row">
          <div className="field field-grow">
            <label htmlFor="address">地址</label>
            <input
              id="address"
              value={address}
              onChange={(event) => setAddress(event.target.value)}
              placeholder="0x…"
              spellCheck={false}
              autoComplete="off"
            />
          </div>
          <button className="btn btn-primary" type="submit" disabled={isLoading}>
            {isLoading ? '查询中…' : '查询'}
          </button>
        </div>
      </form>

      {error ? <div className="error">{error}</div> : null}

      {state ? (
        <>
          <CoverageBar coverage={state.coverage} />

          <div className="panel">
            <div className="panel-head">
              <span className="panel-title">代币余额与净变化</span>
              <span className="panel-hint">
                {state.onchain.enabled ? '净变化按索引区间聚合，余额实时查链' : '净变化按索引区间聚合'}
              </span>
            </div>
            {state.holdings.length === 0 ? (
              <div className="empty">已索引区间内没有该地址的 Transfer</div>
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>合约</th>
                      <th style={{ textAlign: 'right' }}>净变化</th>
                      {state.onchain.enabled ? <th style={{ textAlign: 'right' }}>链上余额</th> : null}
                      <th style={{ textAlign: 'right' }}>日志条数</th>
                      <th>方向</th>
                    </tr>
                  </thead>
                  <tbody>
                    {state.holdings.map((holding) => {
                      const amount = formatAmount(holding.net)
                      const isOutflow = holding.net.startsWith('-')
                      return (
                        <tr key={holding.contract}>
                          <td className="mono">{shortHex(holding.contract)}</td>
                          <td className="num" title={`原始整数：${amount.exact}`}>
                            {amount.display}
                          </td>
                          {state.onchain.enabled ? (
                            <OnchainCell entry={state.onchain.balances?.[holding.contract]} />
                          ) : null}
                          <td className="num">{groupNumber(holding.log_count)}</td>
                          <td>
                            <span className={`tag ${isOutflow ? 'tag-outflow' : 'tag-inflow'}`}>
                              {isOutflow ? '净流出' : '净流入'}
                            </span>
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}
            {state.onchain.enabled && state.onchain.error ? (
              <div className="error">链上余额暂时取不到（RPC 不可用或超时），净变化不受影响。</div>
            ) : null}
            <div className="note">
              <strong>净变化</strong>是已索引区间内的累加，不是余额：区间外的流水看不到，活跃地址完全可能为负。
              {state.onchain.enabled ? (
                <>
                  <strong>链上余额</strong>由查询服务实时调用 <code>balanceOf()</code> 取得，
                  悬停可见抓取时间；它不受索引区间限制，但可能落后链头几个区块。
                </>
              ) : (
                <>
                  真实余额要查链上 <code>balanceOf()</code>，当前查询服务未配置 RPC。
                </>
              )}
              {state.holdings.some((holding) => state.onchain.balances?.[holding.contract]?.decimals === null)
                ? ' 部分代币未取到小数位，金额按 6 位显示，仅供参考。'
                : ''}
            </div>
          </div>

          <div className="panel">
            <div className="panel-head">
              <span className="panel-title">授权</span>
              <span className="panel-hint">每个 (合约, spender) 取区间内最新一条</span>
            </div>
            {state.approvals.length === 0 ? (
              <div className="empty">已索引区间内没有该地址作为 owner 的 Approval</div>
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>spender</th>
                      <th style={{ textAlign: 'right' }}>额度</th>
                      <th>区块</th>
                      <th>交易</th>
                    </tr>
                  </thead>
                  <tbody>
                    {state.approvals.map((approval) => (
                      <tr key={`${approval.spender}-${approval.block_number}-${approval.tx_hash}`}>
                        <td className="mono">{shortHex(approval.spender)}</td>
                        <td className="num">
                          {approval.is_unlimited ? (
                            <span className="tag tag-warn">无限额度</span>
                          ) : (
                            formatAmount(approval.value).display
                          )}
                        </td>
                        <td className="mono">{groupNumber(approval.block_number)}</td>
                        <td className="mono" title={approval.tx_hash}>
                          {shortHex(approval.tx_hash)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <div className="note">
              这是区间内<strong>最新一条授权日志</strong>，不代表链上当前 allowance：
              区间之外可能已经撤销或被覆盖。要判断真实额度必须查链上 <code>allowance()</code>。
            </div>
          </div>

          <div className="panel">
            <div className="panel-head">
              <span className="panel-title">交互合约</span>
              <span className="panel-hint">按事件数降序，不限事件类型</span>
            </div>
            {state.interactions.length === 0 ? (
              <div className="empty">已索引区间内该地址没有出现在任何日志的 topic1/topic2 中</div>
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>合约</th>
                      <th style={{ textAlign: 'right' }}>事件数</th>
                      <th style={{ textAlign: 'right' }}>最近区块</th>
                      <th>最近时间</th>
                    </tr>
                  </thead>
                  <tbody>
                    {state.interactions.map((interaction) => (
                      <tr key={interaction.contract}>
                        <td className="mono">{shortHex(interaction.contract)}</td>
                        <td className="num">{groupNumber(interaction.event_count)}</td>
                        <td className="num">{groupNumber(interaction.last_block)}</td>
                        <td className="mono">{formatBlockTime(interaction.last_time)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <div className="note">
              当前索引只覆盖一个合约，所以这张表通常只有一行。等索引器支持多合约，它才反映真实的交互分布。
            </div>
          </div>
        </>
      ) : null}
    </div>
  )
}

/** OnchainCell 渲染单条链上余额：拿不到就明说，绝不显示 0。 */
function OnchainCell({ entry }: { entry: OnchainBalance | undefined }) {
  if (!entry) {
    return <td className="num">—</td>
  }

  if (entry.status !== 'ok' || entry.value === undefined) {
    return (
      <td className="num">
        <span className="tag tag-warn" title="RPC 不可用或查询超时">
          获取失败
        </span>
      </td>
    )
  }

  // 精度未知时不猜：错一个数量级的金额，比一句「精度未知」更糟。
  if (entry.decimals === null) {
    return (
      <td className="num" title={`原始整数：${entry.value}`}>
        精度未知
      </td>
    )
  }

  const fetched = entry.fetched_at ? `，抓取于 ${formatBlockTime(entry.fetched_at)}` : ''
  return (
    <td className="num" title={`原始整数：${entry.value}${fetched}`}>
      {formatUnits(entry.value, entry.decimals)}
    </td>
  )
}
