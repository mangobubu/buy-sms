import { http, unwrap } from './http'
import type { ProviderConfig, UpdateProviderPayload } from '@/types/api'

export const providersApi = {
  list: () => http.get<ProviderConfig[]>('/providers').then(unwrap),
  update: (id: string, payload: UpdateProviderPayload) =>
    http.put<ProviderConfig>(`/providers/${id}`, payload).then(unwrap),
}
