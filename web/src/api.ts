// 与 cmd/api 的响应 DTO 一一对应。
//
// 金额字段一律是**十进制字符串**：uint256 超出 JS 安全整数范围，
// 用 number 承载会静默丢精度。所以这里保持 string，展示时再做格式化。

export interface Coverage {
  from_block: number
  to_block: number
  finalized_block: number
  next_block: number
  latest_block_time?: string
}

export interface Envelope<T> {
  coverage: Coverage
  data: T
}

export interface EventsEnvelope extends Envelope<EventItem[]> {
  next_cursor?: string
}

export interface EventItem {
  block_number: number
  block_time: string
  tx_hash: string
  log_index: number
  contract: string
  event?: string
  status: string
  payload?: Record<string, unknown>
}

export interface Holding {
  contract: string
  net: string
  log_count: number
  /** decimals 为 null 表示后端没取到精度，展示时不得用默认值冒充。 */
  decimals: number | null
}

/**
 * OnchainBalance 是链上余额快照。
 *
 * `decimals` 为 null 表示后端不知道小数位——此时**不得**退回默认值，
 * 否则金额会差 10^N。界面应显示「精度未知」而不是一个看起来正确的错误数字。
 */
export interface OnchainBalance {
  value?: string
  decimals: number | null
  fetched_at?: string
  cached: boolean
  status: 'ok' | 'unavailable' | string
}

/** Onchain 是可选能力：enabled=false 表示查询服务没配 RPC，不是「余额为 0」。 */
export interface Onchain {
  enabled: boolean
  balances?: Record<string, OnchainBalance>
  error?: string
}

export interface BalancesEnvelope extends Envelope<Holding[]> {
  onchain: Onchain
}

export interface Approval {
  contract: string
  spender: string
  value: string
  is_unlimited: boolean
  block_number: number
  block_time: string
  tx_hash: string
}

export interface Interaction {
  contract: string
  event_count: number
  last_block: number
  last_time: string
}

export interface StatsPayload {
  total_logs: number
  decoded_count: number
  undecodable_count: number
  stale_seconds: number | null
}

export interface EventQuery {
  contract?: string
  from?: string
  to?: string
  limit?: number
  cursor?: string
  offset?: number
}

/** ApiError 保留后端给出的错误码，便于界面区分「参数错」与「超时/限流」。 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

let authToken = ''

/** setAuthToken 设置后续请求携带的 Bearer token；空串表示不鉴权。 */
export function setAuthToken(token: string): void {
  authToken = token.trim()
}

async function request<T>(path: string): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (authToken) {
    headers.Authorization = `Bearer ${authToken}`
  }

  let response: Response
  try {
    response = await fetch(path, { headers })
  } catch {
    throw new ApiError(0, 'network_error', '无法连接查询服务，确认 cmd/api 是否在运行')
  }

  const text = await response.text()
  let body: unknown = null
  if (text) {
    try {
      body = JSON.parse(text) as unknown
    } catch {
      throw new ApiError(response.status, 'invalid_json', '响应不是合法 JSON')
    }
  }

  if (!response.ok) {
    const failure = body as { error?: { code?: string; message?: string } } | null
    throw new ApiError(
      response.status,
      failure?.error?.code ?? 'unknown',
      failure?.error?.message ?? `HTTP ${response.status}`,
    )
  }

  return body as T
}

export function fetchStats(): Promise<Envelope<StatsPayload>> {
  return request<Envelope<StatsPayload>>('/api/stats')
}

export function fetchBalances(address: string, limit = 50): Promise<BalancesEnvelope> {
  return request<BalancesEnvelope>(`/api/address/${address}/balances?limit=${limit}`)
}

export function fetchApprovals(address: string, limit = 50): Promise<Envelope<Approval[]>> {
  return request<Envelope<Approval[]>>(`/api/address/${address}/approvals?limit=${limit}`)
}

export function fetchInteractions(address: string, limit = 50): Promise<Envelope<Interaction[]>> {
  return request<Envelope<Interaction[]>>(`/api/address/${address}/interactions?limit=${limit}`)
}

export function fetchEvents(query: EventQuery): Promise<EventsEnvelope> {
  const params = new URLSearchParams()
  if (query.contract) params.set('contract', query.contract)
  if (query.from) params.set('from', query.from)
  if (query.to) params.set('to', query.to)
  if (query.limit) params.set('limit', String(query.limit))
  if (query.cursor) params.set('cursor', query.cursor)
  if (query.offset !== undefined) params.set('offset', String(query.offset))

  const suffix = params.toString()
  return request<EventsEnvelope>(`/api/events${suffix ? `?${suffix}` : ''}`)
}

/** isAddress 做与后端一致的宽松校验：0x + 40 位 hex。 */
export function isAddress(value: string): boolean {
  return /^0x[0-9a-fA-F]{40}$/.test(value.trim())
}
