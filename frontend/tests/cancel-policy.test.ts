import assert from 'node:assert/strict'
import test from 'node:test'
import { presentCancelPolicy, type CancelPolicyInput } from '../src/utils/cancel-policy.ts'

const availableAt = '2026-09-04T00:02:00.000Z'
const base: CancelPolicyInput = {
  status: 'active',
  hasMessages: false,
  canCancel: false,
  cancelAvailableAt: availableAt,
  cancelUnavailableReason: '购买后需要等待 2 分钟',
}

test('取消等待时间按秒向上取整并在精确边界开放', () => {
  const boundary = Date.parse(availableAt)
  const before = presentCancelPolicy(base, boundary - 1)
  assert.equal(before.disabled, true)
  assert.equal(before.remainingSeconds, 1)
  assert.equal(before.buttonText, '取消（1秒）')

  const exact = presentCancelPolicy(base, boundary)
  assert.equal(exact.disabled, false)
  assert.equal(exact.remainingSeconds, 0)
  assert.equal(presentCancelPolicy(base, boundary + 1).disabled, false)
})

test('服务端允许且未提供时间时直接开放取消', () => {
  const display = presentCancelPolicy({
    status: 'active',
    hasMessages: false,
    canCancel: true,
  }, 0)

  assert.equal(display.showButton, true)
  assert.equal(display.disabled, false)
  assert.equal(display.hint, '')
})

test('没有取消时间且服务端不允许时保持禁用并展示原因', () => {
  const display = presentCancelPolicy({
    status: 'pending',
    hasMessages: false,
    canCancel: false,
    cancelWaitSeconds: 12.1,
    cancelUnavailableReason: '上游暂未开放取消',
  }, 0)

  assert.equal(display.disabled, true)
  assert.equal(display.remainingSeconds, 13)
  assert.equal(display.buttonText, '取消（13秒）')
  assert.equal(display.hint, '上游暂未开放取消')
})

test('无效取消时间保守禁用且不产生 NaN', () => {
  const display = presentCancelPolicy({ ...base, cancelAvailableAt: 'invalid' }, 0)

  assert.equal(display.disabled, true)
  assert.equal(display.remainingSeconds, null)
  assert.doesNotMatch(display.buttonText, /NaN/)
})

test('已收短信和终态隐藏取消入口并解释原因', () => {
  const withMessage = presentCancelPolicy({
    status: 'active',
    hasMessages: true,
    canCancel: false,
  }, 0)
  assert.equal(withMessage.showButton, false)
  assert.match(withMessage.hint, /收到短信/)

  const completed = presentCancelPolicy({
    status: 'completed',
    hasMessages: false,
    canCancel: false,
  }, 0)
  assert.equal(completed.showButton, false)
  assert.match(completed.hint, /已结束/)
})

test('服务端明确允许时优先于过期的等待展示字段', () => {
  const display = presentCancelPolicy({ ...base, canCancel: true }, Date.parse(availableAt) - 30_000)

  assert.equal(display.disabled, false)
  assert.equal(display.remainingSeconds, null)
})
test('服务端当前周期允许取消时不受上一周期历史短信影响', () => {
  const display = presentCancelPolicy({
    status: 'active',
    hasMessages: true,
    canCancel: true,
  }, 0)

  assert.equal(display.showButton, true)
  assert.equal(display.disabled, false)
})
