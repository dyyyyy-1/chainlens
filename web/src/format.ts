// 金额与地址的展示格式。
//
// 关键约束：金额来自后端的十进制字符串，**任何环节都不能转成 number**。
// uint256 的量级会超过 Number.MAX_SAFE_INTEGER，转换是不可逆的精度丢失。

/** USDC_DECIMALS 是当前的换算基准。 */
export const USDC_DECIMALS = 6

/**
 * formatUnits 把整数字符串按小数位格式化，全程字符串运算。
 *
 * 例：("1234567", 6) → "1.234567"；("1000000", 6) → "1"。
 */
export function formatUnits(raw: string, decimals: number = USDC_DECIMALS): string {
  const negative = raw.startsWith('-')
  const digits = (negative ? raw.slice(1) : raw).padStart(decimals + 1, '0')

  const wholePart = digits.slice(0, digits.length - decimals)
  const fractionPart = digits.slice(digits.length - decimals).replace(/0+$/, '')
  const grouped = wholePart.replace(/\B(?=(\d{3})+(?!\d))/g, ',')

  return `${negative ? '-' : ''}${grouped}${fractionPart ? `.${fractionPart}` : ''}`
}

/** formatAmount 在需要保留原始数值时同时给出可读值与精确值。 */
export function formatAmount(raw: string, decimals: number = USDC_DECIMALS): {
  display: string
  exact: string
} {
  return { display: formatUnits(raw, decimals), exact: raw }
}

/** shortHex 截断长 hash，保留首尾便于人工核对。 */
export function shortHex(value: string, head = 10, tail = 8): string {
  if (value.length <= head + tail + 1) {
    return value
  }
  return `${value.slice(0, head)}…${value.slice(-tail)}`
}

/** formatBlockTime 把 RFC3339 转成本地时间，失败时原样返回。 */
export function formatBlockTime(value: string): string {
  if (!value) {
    return '—'
  }
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    return value
  }
  return parsed.toLocaleString('zh-CN', { hour12: false })
}

/** formatStaleness 把秒差转成「x 分钟前」这类可读文本。 */
export function formatStaleness(seconds: number | null): string {
  if (seconds === null) {
    return '未知'
  }
  if (seconds < 60) {
    return `${seconds} 秒前`
  }
  if (seconds < 3600) {
    return `${Math.floor(seconds / 60)} 分钟前`
  }
  if (seconds < 86400) {
    return `${Math.floor(seconds / 3600)} 小时前`
  }
  return `${Math.floor(seconds / 86400)} 天前`
}

/** groupNumber 给普通计数加千分位。 */
export function groupNumber(value: number): string {
  return value.toLocaleString('zh-CN')
}
