import type { ProviderCode } from '@/types/api'

export const providerNames: Record<ProviderCode, string> = {
  herosms: 'HeroSMS',
  smsbower: 'SMSBower',
  smspool: 'SMSPool',
}

export function providerName(code?: string): string {
  return providerNames[code as ProviderCode] || code || '未知平台'
}

export function formatDateTime(value?: string): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date)
}

export function formatMoney(value?: string | number, currency = 'USD'): string {
  const amount = Number(value ?? 0)
  if (!Number.isFinite(amount)) return `${value ?? '0'} ${currency}`
  try {
    return new Intl.NumberFormat('zh-CN', {
      style: 'currency',
      currency,
      minimumFractionDigits: 2,
    }).format(amount)
  } catch {
    return `${amount.toFixed(2)} ${currency}`
  }
}

export function maskPhone(phone?: string): string {
  return phone || '号码分配中'
}
