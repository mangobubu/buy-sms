import { http, unwrap } from './http'
import type { SaveUserPayload, SystemUser, TwoFactorConfiguration, TwoFactorSetup } from '@/types/api'

export const usersApi = {
  list: () => http.get<SystemUser[]>('/users').then(unwrap),
  create: (payload: SaveUserPayload) => http.post<SystemUser>('/users', payload).then(unwrap),
  update: (id: string, payload: SaveUserPayload) =>
    http.put<SystemUser>(`/users/${id}`, payload).then(unwrap),
  prepareTwoFactor: (payload: { username: string; userId?: string }) =>
    http.post<TwoFactorSetup>('/users/two-factor/setup', payload).then(unwrap),
  getTwoFactor: (id: string) =>
    http.get<TwoFactorConfiguration>(`/users/${id}/two-factor`).then(unwrap),
}