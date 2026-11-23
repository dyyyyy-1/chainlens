import { useEffect, useState } from 'react'

import { setAuthToken } from './api'
import { AddressProfile } from './components/AddressProfile'
import { EventExplorer } from './components/EventExplorer'
import { StatsPanel } from './components/StatsPanel'
import { readAppState, writeAppState, type AppState, type TabId } from './urlState'

const TOKEN_STORAGE_KEY = 'chainlens.apiToken'

const TABS: { id: TabId; label: string }[] = [
  { id: 'stats', label: '索引统计' },
  { id: 'address', label: '地址画像' },
  { id: 'events', label: '事件检索' },
]

export default function App() {
  const [state, setState] = useState<AppState>(() => readAppState())
  const tab: TabId = state.tab
  const [token, setToken] = useState<string>(() => {
    try {
      return localStorage.getItem(TOKEN_STORAGE_KEY) ?? ''
    } catch {
      // 隐私模式下 localStorage 可能不可用，退化为不保存。
      return ''
    }
  })

  // 状态变化后同步到 URL；replaceState 幂等，重复调用不会累积历史。
  useEffect(() => {
    writeAppState(state)
  }, [state])

  // 浏览器前进/后退时从 URL 还原，保证地址栏与界面一致。
  useEffect(() => {
    function handlePopState() {
      setState(readAppState())
    }
    window.addEventListener('popstate', handlePopState)
    return () => window.removeEventListener('popstate', handlePopState)
  }, [])

  function update(patch: Partial<AppState>) {
    setState((previous) => ({ ...previous, ...patch }))
  }

  useEffect(() => {
    setAuthToken(token)
    try {
      localStorage.setItem(TOKEN_STORAGE_KEY, token)
    } catch {
      // 忽略写入失败：token 仍在当前会话内生效。
    }
  }, [token])

  return (
    <div className="shell">
      <header className="topbar">
        <div className="brand">
          <span className="brand-mark">◈</span>
          <span>ChainLens</span>
          <span className="brand-sub">USDC 事件索引</span>
        </div>

        <nav className="tabs">
          {TABS.map((item) => (
            <button
              key={item.id}
              className="tab"
              data-active={tab === item.id}
              onClick={() => update({ tab: item.id })}
            >
              {item.label}
            </button>
          ))}
        </nav>

        <div className="topbar-right">
          <input
            className="token-input"
            value={token}
            onChange={(event) => setToken(event.target.value)}
            placeholder="API token（未启用可留空）"
            spellCheck={false}
            autoComplete="off"
          />
        </div>
      </header>

      <main>
        {/* 用 token 做 key：改 token 时重新挂载，自动按新凭据重取数据。 */}
        <div key={token}>
          {tab === 'stats' ? <StatsPanel /> : null}
          {tab === 'address' ? (
            <AddressProfile
              initialAddress={state.address}
              onAddressChange={(address) => update({ address })}
            />
          ) : null}
          {tab === 'events' ? (
            <EventExplorer
              initialFilters={{ contract: state.contract, from: state.from, to: state.to }}
              onFiltersChange={(filters) =>
                update({ contract: filters.contract, from: filters.from, to: filters.to })
              }
            />
          ) : null}
        </div>

        <div className="footer">
          <span>数据来自本地索引库，不是链上实时状态</span>
          <span>金额以十进制字符串传输，前端不做浮点转换</span>
        </div>
      </main>
    </div>
  )
}
