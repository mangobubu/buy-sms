import { http, unwrap } from './http'
import type { ProviderBalance, ProviderConfig, UpdateProviderPayload } from '@/types/api'

export const providersApi = {
  list: () => http.get<ProviderConfig[]>('/providers').then(unwrap),
  balances: (signal?: AbortSignal) =>
    http.get<ProviderBalance[]>('/providers/balances', { signal }).then(unwrap),
  update: (id: string, payload: UpdateProviderPayload) =>
    http.put<ProviderConfig>(`/providers/${id}`, payload).then(unwrap),
}
