import { http, unwrap } from './http'
import type { SaveUserPayload, SystemUser } from '@/types/api'

export const usersApi = {
  list: () => http.get<SystemUser[]>('/users').then(unwrap),
  create: (payload: SaveUserPayload) => http.post<SystemUser>('/users', payload).then(unwrap),
  update: (id: string, payload: SaveUserPayload) =>
    http.put<SystemUser>(`/users/${id}`, payload).then(unwrap),
}
