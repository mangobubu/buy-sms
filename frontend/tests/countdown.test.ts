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

const smsBowerHint = '购买满25分钟后，收到短信自动完成，未收到短信自动取消；处理结果以平台确认为准。'

test('SMSBower 缺失或无效到期时间时从购买时间起算25分钟', () => {
  const createdAt = '2026-09-04T00:00:00.000Z'
  const now = Date.parse(createdAt)
  for (const expiresAt of [undefined, '', 'invalid', ' ']) {
    assert.deepEqual(getOrderCountdown('receiving', expiresAt, now, 'smsbower', createdAt), {
      state: 'active',
      text: '00:25:00',
      hint: smsBowerHint,
    })
  }
  assert.equal(getOrderCountdown('active', undefined, now, ' SMSBower ', createdAt).text, '00:25:00')
})

test('SMSBower 计时随时间递减，重新计算或刷新页面不重置起点', () => {
  const createdAt = '2026-09-04T00:00:00.000Z'
  const createdAtMs = Date.parse(createdAt)
  assert.equal(getOrderCountdown('active', undefined, createdAtMs + 1_000, 'smsbower', createdAt).text, '00:24:59')
  const now = createdAtMs + (8 * 60 + 45) * 1_000
  const beforeRefresh = getOrderCountdown('active', undefined, now, 'smsbower', createdAt)
  assert.deepEqual(beforeRefresh, { state: 'active', text: '00:16:15', hint: smsBowerHint })
  assert.deepEqual(getOrderCountdown('active', undefined, now, 'smsbower', createdAt), beforeRefresh)
  assert.equal(getOrderCountdown('active', undefined, now + 1_000, 'smsbower', createdAt).text, '00:16:14')
})

test('SMSBower 平台期限与25分钟规则取较早时间，后续同步不会延长规则期限', () => {
  const createdAt = '2026-09-04T00:00:00.000Z'
  const now = Date.parse(createdAt)
  assert.equal(getOrderCountdown('active', undefined, now, 'smsbower', createdAt).text, '00:25:00')
  assert.deepEqual(getOrderCountdown('active', '2026-09-04T00:20:00.000Z', now, 'smsbower', createdAt), {
    state: 'active',
    text: '00:20:00',
    hint: smsBowerHint,
  })
  assert.deepEqual(getOrderCountdown('active', '2026-09-04T00:30:00.000Z', now, 'smsbower', createdAt), {
    state: 'active',
    text: '00:25:00',
    hint: smsBowerHint,
  })
})

test('SMSBower 过期、完成与两种取消拼法均立即结束倒计时', () => {
  const createdAt = '2026-09-04T00:00:00.000Z'
  const now = Date.parse(createdAt)
  for (const expiresAt of [undefined, '2026-09-05T00:00:00.000Z']) {
    assert.deepEqual(getOrderCountdown('expired', expiresAt, now, 'smsbower', createdAt), {
      state: 'expired',
      text: '已到期',
    })
    for (const status of ['settled', 'completed', 'cancelled', 'canceled', ' CANCELED ', 'failed']) {
      assert.deepEqual(getOrderCountdown(status, expiresAt, now, 'smsbower', createdAt), {
        state: 'ended',
        text: '已结束',
      })
    }
  }
})

test('SMSBower 25分钟归零及之后只显示状态确认中，不在前端终结订单', () => {
  const createdAt = '2026-09-04T00:00:00.000Z'
  const deadline = Date.parse(createdAt) + 25 * 60 * 1_000
  assert.equal(getOrderCountdown('active', undefined, deadline - 1, 'smsbower', createdAt).text, '00:00:01')
  for (const now of [deadline, deadline + 60_000]) {
    assert.deepEqual(getOrderCountdown('active', undefined, now, 'smsbower', createdAt), {
      state: 'confirming',
      text: '状态确认中',
      hint: smsBowerHint,
    })
  }
  assert.deepEqual(getOrderCountdown('receiving', createdAt, Date.parse(createdAt), 'smsbower', createdAt), {
    state: 'confirming',
    text: '状态确认中',
    hint: smsBowerHint,
  })
})

test('SMSBower 创建时间缺失或无效时只使用有效平台期限，不虚构起算时间', () => {
  const now = Date.parse('2026-09-04T00:00:00.000Z')
  for (const createdAt of [undefined, '', 'invalid', ' ']) {
    assert.deepEqual(getOrderCountdown('active', '2026-09-04T00:20:00.000Z', now, 'smsbower', createdAt), {
      state: 'active',
      text: '00:20:00',
    })
    for (const expiresAt of [undefined, 'invalid']) {
      assert.deepEqual(getOrderCountdown('active', expiresAt, now, 'smsbower', createdAt), {
        state: 'unknown',
        text: '等待平台同步',
      })
    }
  }
})

test('其他供应商与未指定供应商不使用25分钟规则', () => {
  const createdAt = '2026-09-04T00:00:00.000Z'
  const now = Date.parse(createdAt)
  for (const provider of ['herosms', 'smspool', undefined]) {
    for (const expiresAt of [undefined, 'invalid']) {
      assert.deepEqual(getOrderCountdown('active', expiresAt, now, provider, createdAt), {
        state: 'unknown',
        text: '等待平台同步',
      })
    }
    assert.deepEqual(getOrderCountdown('active', '2026-09-04T01:00:00.000Z', now, provider, createdAt), {
      state: 'active',
      text: '01:00:00',
    })
  }
})
