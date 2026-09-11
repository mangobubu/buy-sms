import assert from 'node:assert/strict'
import test from 'node:test'
import { completionNotice } from '../src/utils/order-completion.ts'

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
