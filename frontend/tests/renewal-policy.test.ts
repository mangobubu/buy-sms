import assert from 'node:assert/strict'
import test from 'node:test'

import type { NumberOrder } from '../src/types/api.ts'
import { formatRenewalDuration, isRenewalCandidate, renewalOptionKey } from '../src/utils/renewal-policy.ts'

function order(overrides: Partial<NumberOrder>): NumberOrder {
  return {
    id: 'order-1', provider: 'herosms', countryCode: '2', serviceCode: 'tg',
    phoneNumber: '79990001122', status: 'active', price: '1', currency: 'USD',
    messages: [], canCancel: false, createdAt: '2026-09-04T00:00:00Z',
    ...overrides,
  }
}

test('Hero 长租在活跃或过期时才查询续租，成功完成后查询重新启用', () => {
  assert.equal(isRenewalCandidate(order({ duration: '24', status: 'active' })), true)
  assert.equal(isRenewalCandidate(order({ duration: '24', status: 'expired' })), true)
  assert.equal(isRenewalCandidate(order({ duration: '', status: 'active' })), false)
  assert.equal(isRenewalCandidate(order({ duration: '', status: 'expired' })), false)
  assert.equal(isRenewalCandidate(order({ duration: '', status: 'completed' })), true)
  assert.equal(isRenewalCandidate(order({ duration: '24', status: 'cancelled' })), false)
})

test('SMSPool 仅对未收码的取消或过期订单查询重新启用', () => {
  assert.equal(isRenewalCandidate(order({ provider: 'smspool', status: 'cancelled' })), true)
  assert.equal(isRenewalCandidate(order({ provider: 'smspool', status: 'expired' })), true)
  assert.equal(isRenewalCandidate(order({ provider: 'smspool', status: 'completed' })), false)
  assert.equal(isRenewalCandidate(order({
    provider: 'smspool', status: 'expired',
    messages: [{ id: 'sms-1', content: 'code', receivedAt: '2026-09-04T00:01:00Z' }],
  })), false)
  assert.equal(isRenewalCandidate(order({ provider: 'smsbower', status: 'expired' })), false)
})

test('续期选项标签和键稳定', () => {
  assert.equal(renewalOptionKey({ value: 24, unit: 'hour' }), 'hour:24')
  assert.equal(formatRenewalDuration({ value: 24, unit: 'hour', minutes: 1440 }), '24 小时（1 天）')
  assert.equal(formatRenewalDuration({ value: 20, unit: 'minute', minutes: 20 }), '20 分钟')
  assert.equal(formatRenewalDuration({ value: 1, unit: 'activation', minutes: 0 }), '重新启用一次')
})
