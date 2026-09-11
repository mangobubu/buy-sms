export type OrderCountdownState = 'active' | 'confirming' | 'expired' | 'ended' | 'unknown'

export interface OrderCountdownDisplay {
  state: OrderCountdownState
  text: string
  hint?: string
}

const terminalStatuses = new Set(['settled', 'completed', 'cancelled', 'canceled', 'expired', 'failed'])

export function isTerminalOrderStatus(status?: string): boolean {
  return terminalStatuses.has(status?.trim().toLowerCase() ?? '')
}

export function formatCountdownDuration(totalSeconds: number): string {
  const safeSeconds = Number.isFinite(totalSeconds) ? Math.max(0, Math.ceil(totalSeconds)) : 0
  const days = Math.floor(safeSeconds / 86_400)
  const hours = Math.floor((safeSeconds % 86_400) / 3_600)
  const minutes = Math.floor((safeSeconds % 3_600) / 60)
  const seconds = safeSeconds % 60
  const clock = [hours, minutes, seconds].map((part) => String(part).padStart(2, '0')).join(':')

  return days > 0 ? `${days}天 ${clock}` : clock
}

export function getOrderCountdown(
  status: string | undefined,
  expiresAt: string | undefined,
  nowMs = Date.now(),
  provider?: string,
  createdAt?: string,
): OrderCountdownDisplay {
  const normalizedStatus = status?.trim().toLowerCase() ?? ''
  if (normalizedStatus === 'expired') return { state: 'expired', text: '已到期' }
  if (isTerminalOrderStatus(normalizedStatus)) return { state: 'ended', text: '已结束' }

  let expiresAtMs = Date.parse(expiresAt ?? '')
  let hint: string | undefined
  if (provider?.trim().toLowerCase() === 'smsbower') {
    const createdAtMs = Date.parse(createdAt ?? '')
    if (Number.isFinite(createdAtMs)) {
      const ruleDeadlineMs = createdAtMs + 25 * 60 * 1_000
      expiresAtMs = Number.isFinite(expiresAtMs) ? Math.min(expiresAtMs, ruleDeadlineMs) : ruleDeadlineMs
      hint = '购买满25分钟后，收到短信自动完成，未收到短信自动取消；处理结果以平台确认为准。'
    }
  }
  if (!Number.isFinite(expiresAtMs)) return { state: 'unknown', text: '等待平台同步' }

  const remainingMs = expiresAtMs - nowMs
  const countdown: OrderCountdownDisplay = remainingMs <= 0
    ? { state: 'confirming', text: '状态确认中' }
    : { state: 'active', text: formatCountdownDuration(remainingMs / 1_000) }
  if (hint) countdown.hint = hint
  return countdown
}
