import { http, unwrap } from './http'
import type { CountryOption, ProviderCode, Quote, ServiceOption, SmsBowerTier } from '@/types/api'

export const catalogApi = {
  services: (provider: ProviderCode) =>
    http.get<ServiceOption[]>('/catalog/services', { params: { provider } }).then(unwrap),
  countries: (provider: ProviderCode, service: string, tier?: SmsBowerTier) =>
    http.get<CountryOption[]>('/catalog/countries', {
      params: { provider, service, ...(tier ? { tier } : {}) },
    }).then(unwrap),
  quote: (provider: ProviderCode, country: string, service: string, tier?: SmsBowerTier) =>
    http.get<Quote>('/catalog/quote', {
      params: { provider, country, service, ...(tier ? { tier } : {}) },
    }).then(unwrap),
}
