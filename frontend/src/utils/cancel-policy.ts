const TERMINAL_ORDER_STATUSES = new Set([
  'settled',
  'completed',
  'cancelled',
  'expired',
  'failed',
])

export interface CancelPolicyInput {
  status: string
  hasMessages: boolean
  canCancel: boolean
  cancelAvailableAt?: string
  cancelWaitSeconds?: number
  cancelUnavailableReason?: string
}

export interface CancelPresentation {
  showButton: boolean
  disabled: boolean
  buttonText: string
  remainingSeconds: number | null
  hint: string
}

function unavailable(
  hint: string,
  options: { showButton?: boolean; remainingSeconds?: number | null } = {},
): CancelPresentation {
  const remainingSeconds = options.remainingSeconds ?? null
  return {
    showButton: options.showButton ?? true,
    disabled: true,
    buttonText: remainingSeconds ? `取消（${remainingSeconds}秒）` : '取消',
    remainingSeconds,
    hint,
  }
}

export function presentCancelPolicy(
  policy: CancelPolicyInput,
  nowMs = Date.now(),
): CancelPresentation {
  const reason = policy.cancelUnavailableReason?.trim() ?? ''

  if (TERMINAL_ORDER_STATUSES.has(policy.status.trim().toLowerCase())) {
    return unavailable(reason || '订单已结束，无需再取消', { showButton: false })
  }

  // canCancel 是服务端按“当前激活周期”计算的事实源。重新启用的订单可能
  // 保留上一周期短信，因此必须优先于前端看到的全量历史消息。
  if (policy.canCancel) {
    return {
      showButton: true,
      disabled: false,
      buttonText: '取消',
      remainingSeconds: null,
      hint: '',
    }
  }

  if (policy.hasMessages) {
    return unavailable(reason || '号码已收到短信，取消入口已关闭', { showButton: false })
  }
  const availableAt = policy.cancelAvailableAt?.trim()
  if (availableAt) {
    const availableAtMs = Date.parse(availableAt)
    if (!Number.isFinite(availableAtMs)) {
      return unavailable(reason || '取消时间信息异常，请刷新后重试')
    }

    const remainingSeconds = Math.max(0, Math.ceil((availableAtMs - nowMs) / 1_000))
    if (remainingSeconds === 0) {
      return {
        showButton: true,
        disabled: false,
        buttonText: '取消',
        remainingSeconds: 0,
        hint: '',
      }
    }

    return unavailable(reason || '倒计时结束后即可取消号码', { remainingSeconds })
  }

  const waitSeconds = Number.isFinite(policy.cancelWaitSeconds)
    ? Math.max(0, Math.ceil(policy.cancelWaitSeconds ?? 0))
    : 0
  return unavailable(
    reason || (waitSeconds ? '请等待取消限制结束' : '该号码当前暂不可取消，请稍后刷新'),
    { remainingSeconds: waitSeconds || null },
  )
}
