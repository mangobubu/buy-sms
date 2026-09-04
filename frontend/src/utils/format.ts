import type { ProviderCode } from '../types/api'

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
      maximumFractionDigits: 4,
    }).format(amount)
  } catch {
    const [integer, fraction = ''] = amount.toFixed(4).split('.')
    const preciseFraction = fraction.replace(/0+$/, '').padEnd(2, '0')
    return `${integer}.${preciseFraction} ${currency}`
  }
}

export function formatPurchaseDuration(value?: string): string {
  const duration = value?.trim() || ''
  if (!duration) return '标准接码'
  const hours = Number(duration)
  if (!Number.isInteger(hours) || hours <= 0) return `时长 ${duration}`
  return hours % 24 === 0 ? `${hours / 24} 天（${hours} 小时）` : `${hours} 小时`
}

// E.164 的国际区号为 1～3 位；先匹配完整的一、二位区号，其余有效区号均为三位。
const singleDigitCallingCodes = new Set(['1', '7'])
const twoDigitCallingCodes = new Set([
  '20', '27', '30', '31', '32', '33', '34', '36', '39', '40', '41', '43', '44', '45', '46', '47',
  '48', '49', '51', '52', '53', '54', '55', '56', '57', '58', '60', '61', '62', '63', '64', '65', '66',
  '81', '82', '84', '86', '90', '91', '92', '93', '94', '95', '98',
])

export function formatPhoneNumber(phone?: string): string {
  if (!phone) return '号码分配中'

  const trimmed = phone.trim()
  let digits = trimmed.replace(/\D/g, '')
  if (!digits) return trimmed || '号码分配中'
  if (trimmed.startsWith('00')) digits = digits.slice(2)
  if (!digits) return trimmed || '号码分配中'

  const callingCodeLength = singleDigitCallingCodes.has(digits.charAt(0))
    ? 1
    : twoDigitCallingCodes.has(digits.slice(0, 2))
      ? 2
      : Math.min(3, digits.length)
  const callingCode = digits.slice(0, callingCodeLength)
  const localGroups = digits.slice(callingCodeLength).match(/.{1,3}/g) ?? []

  return [`+${callingCode}`, ...localGroups].join(' ')
}

export function maskPhone(phone?: string): string {
  return phone || '号码分配中'
}
