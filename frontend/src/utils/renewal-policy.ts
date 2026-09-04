import type { NumberOrder, RenewalOption } from '../types/api'

export function isRenewalCandidate(order: NumberOrder): boolean {
  if (order.provider === 'herosms') {
    if (order.status === 'completed') return true
    return Boolean(order.duration?.trim()) && (order.status === 'active' || order.status === 'expired')
  }
  if (order.provider === 'smspool') {
    const endedWithoutSms = (order.status === 'cancelled' || order.status === 'expired')
      && !order.messages?.length
    return endedWithoutSms
  }
  return false
}

export function renewalOptionKey(option: Pick<RenewalOption, 'value' | 'unit'>): string {
  return `${option.unit}:${option.value}`
}

export function formatRenewalDuration(option: Pick<RenewalOption, 'value' | 'unit' | 'minutes'>): string {
  if (option.unit === 'activation') return '重新启用一次'
  if (option.unit === 'hour') {
    if (option.value >= 24 && option.value % 24 === 0) {
      return `${option.value} 小时（${option.value / 24} 天）`
    }
    return `${option.value} 小时`
  }
  return `${option.minutes || option.value} 分钟`
}
