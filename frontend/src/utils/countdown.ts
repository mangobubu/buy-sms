export type OrderCountdownState = 'active' | 'confirming' | 'expired' | 'ended' | 'unknown'

export interface OrderCountdownDisplay {
  state: OrderCountdownState
  text: string
}

const terminalStatuses = new Set(['settled', 'completed', 'cancelled', 'expired', 'failed'])

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
): OrderCountdownDisplay {
  const normalizedStatus = status?.trim().toLowerCase() ?? ''
  if (normalizedStatus === 'expired') return { state: 'expired', text: '已到期' }
  if (isTerminalOrderStatus(normalizedStatus)) return { state: 'ended', text: '已结束' }

  if (!expiresAt) return { state: 'unknown', text: '等待平台同步' }
  const expiresAtMs = Date.parse(expiresAt)
  if (!Number.isFinite(expiresAtMs)) return { state: 'unknown', text: '等待平台同步' }

  const remainingMs = expiresAtMs - nowMs
  if (remainingMs <= 0) return { state: 'confirming', text: '状态确认中' }

  return {
    state: 'active',
    text: formatCountdownDuration(remainingMs / 1_000),
  }
}
