import { http, unwrap } from './http'
import type {
  AuthUser,
  CaptchaResponse,
  LoginPayload,
  LoginResult,
  LoginResponse,
  TwoFactorLoginPayload,
} from '@/types/api'

export const authApi = {
  captcha: () => http.get<CaptchaResponse>('/public/captcha').then(unwrap),
  login: (payload: LoginPayload) => http.post<LoginResult>('/public/login', payload).then(unwrap),
  verifyTwoFactor: (payload: TwoFactorLoginPayload) =>
    http.post<LoginResponse>('/public/login/two-factor', payload).then(unwrap),
  me: () => http.get<AuthUser>('/auth/me').then(unwrap),
  logout: () => http.post<void>('/auth/logout').then(unwrap),
  changePassword: (payload: { currentPassword: string; newPassword: string }) =>
    http.post<void>('/auth/change-password', payload).then(unwrap),
}