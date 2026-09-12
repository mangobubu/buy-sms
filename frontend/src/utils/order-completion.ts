export interface LocalCompletionCandidate {
  provider: string
  status: string
  createdAt: string
  currentActivationHasMessages?: boolean
  renewalPending?: boolean
}

export function canOfferLocalCompletion(
  order: LocalCompletionCandidate,
  failureCode: string,
  nowMs = Date.now(),
): boolean {
  if (failureCode.trim().toLowerCase() !== 'complete_status_conflict' || order.provider.trim().toLowerCase() !== 'smsbower' ||
    order.status.trim().toLowerCase() !== 'active' || order.renewalPending || order.currentActivationHasMessages !== true) {
    return false
  }
  const createdAtMs = Date.parse(order.createdAt)
  return Number.isFinite(createdAtMs) && Number.isFinite(nowMs) &&
    nowMs >= createdAtMs + 25 * 60 * 1_000
}

export function completionNotice(status: string | undefined, localCompleted = false): {
  type: 'success' | 'warning'
  message: string
} {
  if (localCompleted && status?.trim().toLowerCase() === 'completed') {
    return { type: 'success', message: '已在本地结束订单，短信记录已保留' }
  }
  switch (status?.trim().toLowerCase()) {
    case 'completed':
    case 'settled':
      return { type: 'success', message: '订单已完成' }
    case 'canceled':
    case 'cancelled':
      return { type: 'warning', message: '供应商已取消该订单，已同步本地状态' }
    case 'expired':
      return { type: 'warning', message: '供应商确认该订单已过期，已同步本地状态' }
    default:
      return { type: 'warning', message: '完成结果尚未确认，请刷新订单状态' }
  }
}
