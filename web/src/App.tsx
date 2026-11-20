import { useEffect, useState } from 'react'

import { setAuthToken } from './api'
import { AddressProfile } from './components/AddressProfile'
import { EventExplorer } from './components/EventExplorer'
import { StatsPanel } from './components/StatsPanel'

const TOKEN_STORAGE_KEY = 'chainlens.apiToken'

type TabId = 'stats' | 'address' | 'events'

const TABS: { id: TabId; label: string }[] = [
  { id: 'stats', label: '索引统计' },
  { id: 'address', label: '地址画像' },
  { id: 'events', label: '事件检索' },
]

export default function App() {
  const [tab, setTab] = useState<TabId>('stats')
  const [token, setToken] = useState<string>(() => {
    try {
      return localStorage.getItem(TOKEN_STORAGE_KEY) ?? ''
    } catch {
      // 隐私模式下 localStorage 可能不可用，退化为不保存。
      return ''
    }
  })

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
              onClick={() => setTab(item.id)}
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
          {tab === 'address' ? <AddressProfile initialAddress="" /> : null}
          {tab === 'events' ? <EventExplorer /> : null}
        </div>

        <div className="footer">
          <span>数据来自本地索引库，不是链上实时状态</span>
          <span>金额以十进制字符串传输，前端不做浮点转换</span>
        </div>
      </main>
    </div>
  )
}
