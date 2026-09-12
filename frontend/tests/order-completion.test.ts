import assert from 'node:assert/strict'
import test from 'node:test'
import { canOfferLocalCompletion, completionNotice, type LocalCompletionCandidate } from '../src/utils/order-completion.ts'

test('完成结果只在订单确实完成时提示成功', () => {
  for (const status of ['completed', 'settled', ' COMPLETED ']) {
    assert.deepEqual(completionNotice(status), { type: 'success', message: '订单已完成' })
  }
})

test('同步取消状态不显示完成或退款成功', () => {
  for (const status of ['canceled', 'cancelled']) {
    const notice = completionNotice(status)
    assert.equal(notice.type, 'warning')
    assert.equal(notice.message, '供应商已取消该订单，已同步本地状态')
    assert.doesNotMatch(notice.message, /完成|退款/)
  }
})

test('同步过期状态显示实际结果', () => {
  assert.deepEqual(completionNotice('expired'), {
    type: 'warning', message: '供应商确认该订单已过期，已同步本地状态',
  })
})

test('未知或活动状态不能展示完成成功', () => {
  for (const status of ['active', 'received', '', 'unexpected', undefined]) {
    assert.deepEqual(completionNotice(status), {
      type: 'warning', message: '完成结果尚未确认，请刷新订单状态',
    })
  }
})

test('本地结束显示保留短信提示，不声称上游完成或退款', () => {
  assert.deepEqual(completionNotice('completed', true), {
    type: 'success', message: '已在本地结束订单，短信记录已保留',
  })
  for (const status of ['active', 'received', 'cancelled', 'expired', undefined]) {
    assert.deepEqual(completionNotice(status, true), completionNotice(status))
  }
  assert.deepEqual(completionNotice('completed', false), completionNotice('completed'))
})

const localCompletionOrder: LocalCompletionCandidate = {
  provider: 'smsbower',
  status: 'active',
  createdAt: '2026-09-11T10:00:00.000Z',
  currentActivationHasMessages: true,
}
const localCompletionDeadline = Date.parse(localCompletionOrder.createdAt) + 25 * 60 * 1_000

test('仅普通完成状态冲突后且25分钟已过才开放本地结束恢复', () => {
  assert.equal(canOfferLocalCompletion(localCompletionOrder, 'complete_status_conflict', localCompletionDeadline - 1), false)
  assert.equal(canOfferLocalCompletion(localCompletionOrder, 'complete_status_conflict', localCompletionDeadline), true)
  assert.equal(canOfferLocalCompletion(localCompletionOrder, 'complete_status_conflict', localCompletionDeadline + 1), true)
  for (const code of ['', 'BAD_STATUS', 'provider_error', 'TIMEOUT', 'local_complete_not_allowed']) {
    assert.equal(canOfferLocalCompletion(localCompletionOrder, code, localCompletionDeadline), false)
  }
})

test('本地结束恢复仅面向已收本次激活短信的SMSBower活动订单', () => {
  for (const change of [
    { provider: 'herosms' },
    { provider: 'smspool' },
    { status: 'completed' },
    { status: 'cancelled' },
    { status: 'expired' },
    { status: 'pending' },
    { currentActivationHasMessages: false },
    { currentActivationHasMessages: undefined },
    { renewalPending: true },
    { createdAt: 'invalid' },
    { createdAt: '' },
  ]) {
    assert.equal(canOfferLocalCompletion({ ...localCompletionOrder, ...change }, 'complete_status_conflict', localCompletionDeadline), false)
  }
  for (const now of [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
    assert.equal(canOfferLocalCompletion(localCompletionOrder, 'complete_status_conflict', now), false)
  }
})
