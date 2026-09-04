import assert from 'node:assert/strict'
import test from 'node:test'
import { formatCountdownDuration, getOrderCountdown } from '../src/utils/countdown.ts'

test('倒计时按天、时、分、秒稳定格式化', () => {
  assert.equal(formatCountdownDuration(3_661), '01:01:01')
  assert.equal(formatCountdownDuration(90_061), '1天 01:01:01')
  assert.equal(formatCountdownDuration(0.001), '00:00:01')
})

test('活动订单使用 expiresAt 计算剩余时间', () => {
  const now = Date.parse('2026-09-04T00:00:00.000Z')
  assert.deepEqual(getOrderCountdown('active', '2026-09-04T01:01:01.000Z', now), {
    state: 'active',
    text: '01:01:01',
  })
})

test('过期与其他终态显示明确结果', () => {
  const now = Date.parse('2026-09-04T00:00:00.000Z')
  assert.deepEqual(getOrderCountdown('expired', '2026-09-05T00:00:00.000Z', now), {
    state: 'expired',
    text: '已到期',
  })
  assert.deepEqual(getOrderCountdown('completed', '2026-09-05T00:00:00.000Z', now), {
    state: 'ended',
    text: '已结束',
  })
  assert.deepEqual(getOrderCountdown('cancelled', '2026-09-05T00:00:00.000Z', now), {
    state: 'ended',
    text: '已结束',
  })
  assert.deepEqual(getOrderCountdown('receiving', '2026-09-03T23:59:59.000Z', now), {
    state: 'confirming',
    text: '状态确认中',
  })
  assert.deepEqual(getOrderCountdown('active', '2026-09-04T00:00:00.000Z', now), {
    state: 'confirming',
    text: '状态确认中',
  })
})

test('缺失或无效时间不产生 NaN', () => {
  assert.deepEqual(getOrderCountdown('pending', undefined, 0), {
    state: 'unknown',
    text: '等待平台同步',
  })
  assert.deepEqual(getOrderCountdown('active', 'invalid', 0), {
    state: 'unknown',
    text: '等待平台同步',
  })
})
