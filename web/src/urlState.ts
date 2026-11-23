// 把界面状态同步到 URL，让刷新、分享、新标签页都能落到同一个视图。
//
// 不引入路由库：三个标签页、几个过滤条件，URLSearchParams + replaceState 足够。
// 用 replaceState 而不是 pushState：输入框每敲一个字符都写历史会让后退键变成灾难。

export type TabId = 'stats' | 'address' | 'events'

export interface AppState {
  tab: TabId
  address: string
  contract: string
  from: string
  to: string
}

export const DEFAULT_APP_STATE: AppState = {
  tab: 'stats',
  address: '',
  contract: '',
  from: '',
  to: '',
}

function parseTab(raw: string | null): TabId {
  if (raw === 'address' || raw === 'events') {
    return raw
  }
  return 'stats'
}

/** readAppState 从当前 URL 还原界面状态；无法识别的值一律回落到默认值。 */
export function readAppState(): AppState {
  if (typeof window === 'undefined') {
    return DEFAULT_APP_STATE
  }

  const params = new URLSearchParams(window.location.search)
  return {
    tab: parseTab(params.get('tab')),
    address: params.get('address') ?? '',
    contract: params.get('contract') ?? '',
    from: params.get('from') ?? '',
    to: params.get('to') ?? '',
  }
}

/**
 * writeAppState 把状态写回 URL。
 *
 * 只写与当前标签页相关的字段：从地址页切到统计页后，URL 里不该还挂着
 * `address=0x…`——那会让分享出去的链接看起来指向一个并不存在的视图。
 */
export function writeAppState(state: AppState): void {
  if (typeof window === 'undefined') {
    return
  }

  const params = new URLSearchParams()
  if (state.tab !== 'stats') {
    params.set('tab', state.tab)
  }
  if (state.tab === 'address' && state.address) {
    params.set('address', state.address)
  }
  if (state.tab === 'events') {
    if (state.contract) params.set('contract', state.contract)
    if (state.from) params.set('from', state.from)
    if (state.to) params.set('to', state.to)
  }

  const search = params.toString()
  const url = search ? `${window.location.pathname}?${search}` : window.location.pathname
  window.history.replaceState(null, '', url)
}
