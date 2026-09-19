import { reactive, readonly } from 'vue'
import type { Router } from 'vue-router'
import { authApi } from '@/api/auth'
import { tokenSession } from '@/api/http'
import type { AuthUser, LoginPayload, LoginResponse, LoginResult, TwoFactorLoginPayload } from '@/types/api'
import { currentAdminPath } from '@/utils/admin-path'

interface AuthState {
  user: AuthUser | null
  loading: boolean
  hydrated: boolean
}

const state = reactive<AuthState>({
  user: null,
  loading: false,
  hydrated: false,
})

let hydratePromise: Promise<boolean> | null = null

async function hydrate(): Promise<boolean> {
  if (!tokenSession.get()) {
    state.user = null
    state.hydrated = true
    return false
  }
  if (state.user) return true
  if (hydratePromise) return hydratePromise

  state.loading = true
  hydratePromise = authApi
    .me()
    .then((user) => {
      state.user = user
      state.hydrated = true
      return true
    })
    .catch(() => {
      tokenSession.clear()
      state.user = null
      state.hydrated = true
      return false
    })
    .finally(() => {
      state.loading = false
      hydratePromise = null
    })
  return hydratePromise
}

function acceptLogin(result: LoginResponse): void {
  tokenSession.set(result.token)
  state.user = result.user
  state.hydrated = true
}

async function login(payload: LoginPayload): Promise<LoginResult> {
  const result = await authApi.login(payload)
  // 验证密码后返回的 challenge 仅代表还需验证，不是已登录会话。
  if ('twoFactorRequired' in result) {
    clear()
  } else {
    acceptLogin(result)
  }
  return result
}

async function verifyTwoFactor(payload: TwoFactorLoginPayload): Promise<AuthUser> {
  const result = await authApi.verifyTwoFactor(payload)
  acceptLogin(result)
  return result.user
}

async function logout(): Promise<void> {
  try {
    if (tokenSession.get()) await authApi.logout()
  } catch {
    // 即使服务端会话已失效或网络中断，也必须完成本地退出。
  } finally {
    clear()
  }
}

function clear(): void {
  tokenSession.clear()
  state.user = null
  state.hydrated = true
}

function bindUnauthorizedHandler(router: Router): void {
  window.addEventListener('auth:unauthorized', () => {
    clear()
    const adminPath = currentAdminPath()
    const loginPath = adminPath ? `${adminPath}/login` : '/404'
    void router.replace({ path: loginPath, query: { expired: '1' } })
  })
}

export const authSession = {
  state: readonly(state),
  hasToken: () => Boolean(tokenSession.get()),
  hydrate,
  login,
  verifyTwoFactor,
  logout,
  clear,
  bindUnauthorizedHandler,
}