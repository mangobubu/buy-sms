import axios, { AxiosError, type AxiosResponse } from 'axios'
import { currentAdminPath } from '@/utils/admin-path'
import type { ApiEnvelope } from '@/types/api'

const TOKEN_KEY = 'buy_sms_access_token'

export const tokenSession = {
  get: () => sessionStorage.getItem(TOKEN_KEY) || '',
  set: (token: string) => sessionStorage.setItem(TOKEN_KEY, token),
  clear: () => sessionStorage.removeItem(TOKEN_KEY),
}

export const http = axios.create({
  baseURL: '/api',
  timeout: 15_000,
  withCredentials: true,
  headers: {
    'Content-Type': 'application/json',
  },
})

http.interceptors.request.use((config) => {
  const token = tokenSession.get()
  if (token) config.headers.Authorization = `Bearer ${token}`

  const adminPath = currentAdminPath()
  if (adminPath) config.headers['X-Admin-Path'] = adminPath
  return config
})

http.interceptors.response.use(
  (response) => response,
  (error: AxiosError) => {
    if (error.response?.status === 401 && tokenSession.get()) {
      tokenSession.clear()
      window.dispatchEvent(new CustomEvent('auth:unauthorized'))
    }
    return Promise.reject(error)
  },
)

export function unwrap<T>(response: AxiosResponse<ApiEnvelope<T> | T>): T {
  const payload = response.data
  if (payload && typeof payload === 'object' && 'data' in payload) {
    return (payload as ApiEnvelope<T>).data
  }
  return payload as T
}

export function errorMessage(error: unknown, fallback = '操作失败，请稍后重试'): string {
  if (axios.isAxiosError(error)) {
    const data = error.response?.data as { message?: string; error?: string } | undefined
    return data?.message || data?.error || fallback
  }
  return error instanceof Error ? error.message : fallback
}
