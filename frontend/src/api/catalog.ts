import { http, unwrap } from './http'
import type { CountryOption, ProviderCode, Quote, ServiceOption } from '@/types/api'

export const catalogApi = {
  countries: (provider: ProviderCode) =>
    http.get<CountryOption[]>('/catalog/countries', { params: { provider } }).then(unwrap),
  services: (provider: ProviderCode, country: string) =>
    http.get<ServiceOption[]>('/catalog/services', { params: { provider, country } }).then(unwrap),
  quote: (provider: ProviderCode, country: string, service: string) =>
    http.get<Quote>('/catalog/quote', { params: { provider, country, service } }).then(unwrap),
}
