export type ProviderCode = 'herosms' | 'smsbower' | 'smspool'
export type SmsBowerTier = 'bronze' | 'silver' | 'gold'

export interface ApiEnvelope<T> {
  code?: number
  message?: string
  data: T
}

export interface PageResult<T> {
  items: T[]
  total: number
  page: number
  pageSize: number
}

export interface AuthUser {
  id: string
  username: string
  displayName?: string
  role: 'admin' | 'operator' | string
}

export interface CaptchaResponse {
  id: string
  image: string
}

export interface LoginPayload {
  username: string
  password: string
  captchaId: string
  captcha: string
  adminPath: string
}

export interface LoginResponse {
  token: string
  user: AuthUser
  expiresAt?: string
}

export interface DashboardOverview {
  activeOrders: number
  todayOrders: number
  todayMessages: number
  todaySpend: string
  currency: string
  providerHealthy: number
  providerTotal: number
  recentOrders: NumberOrder[]
  providers: ProviderHealth[]
}

export interface ProviderHealth {
  code: ProviderCode
  name: string
  enabled: boolean
  healthy: boolean
  balance?: string
  currency?: string
  lastCheckedAt?: string
  message?: string
}

export interface ProviderConfig {
  id: string
  code: ProviderCode
  name: string
  apiBaseUrl: string
  enabled: boolean
  pollingIntervalSeconds: number
  webhookSupported: boolean
  webhookEnabled?: boolean
  hasApiKey: boolean
  hasWebhookToken: boolean
  webhookUrl?: string
  updatedAt?: string
}

export interface UpdateProviderPayload {
  apiBaseUrl: string
  apiKey?: string
  webhookToken?: string
  enabled: boolean
  webhookEnabled: boolean
  pollingIntervalSeconds: number
}

export interface CountryOption {
  code: string
  name: string
  flag?: string
  available?: number
  priceFrom?: string
}

export interface ServiceOption {
  code: string
  name: string
  available?: number
  price?: string
}

export interface Quote {
  provider: ProviderCode
  providerName: string
  countryCode: string
  serviceCode: string
  tier?: SmsBowerTier
  price: string
  currency: string
  available: number
  priceOptions?: Array<{
    price: string
    available: number
  }>
}

export interface PurchasePayload {
  provider: ProviderCode
  countryCode: string
  serviceCode: string
  tier?: SmsBowerTier
  maxPrice: string
}

export type PurchaseAttemptStatus = 'provisioning' | 'succeeded' | 'failed' | 'unknown'

export interface PurchaseAttempt {
  provider: ProviderCode
  countryCode: string
  countryName?: string
  serviceCode: string
  serviceName?: string
  tier?: SmsBowerTier
  maxPrice: string
  status: PurchaseAttemptStatus | string
  errorCode: string
  message: string
  createdAt: string
  updatedAt: string
}

export type OrderStatus =
  | 'pending'
  | 'active'
  | 'receiving'
  | 'settled'
  | 'completed'
  | 'cancelled'
  | 'expired'
  | 'failed'

export interface SmsMessage {
  id: string
  code?: string
  content: string
  receivedAt: string
}

export interface NumberOrder {
  id: string
  provider: ProviderCode
  providerName?: string
  countryCode: string
  countryName?: string
  serviceCode: string
  serviceName?: string
  tier?: SmsBowerTier
  phoneNumber: string
  status: OrderStatus | string
  price: string
  currency: string
  messages: SmsMessage[]
  webhookEnabled?: boolean
  lastPolledAt?: string
  createdAt: string
  updatedAt?: string
}

export interface OrderQuery {
  page: number
  pageSize: number
  status?: string
  provider?: string
  keyword?: string
}

export interface SystemUser {
  id: string
  username: string
  displayName?: string
  role: 'admin' | 'operator' | string
  enabled: boolean
  lastLoginAt?: string
  createdAt: string
}

export interface SaveUserPayload {
  username: string
  displayName?: string
  role: string
  enabled: boolean
  password?: string
}
