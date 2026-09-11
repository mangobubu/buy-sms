export function completionNotice(status: string | undefined): {
  type: 'success' | 'warning'
  message: string
} {
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
