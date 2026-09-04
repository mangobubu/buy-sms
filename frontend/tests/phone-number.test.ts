import assert from 'node:assert/strict'
import test from 'node:test'
import { formatPhoneNumber, getPhoneNumberParts } from '../src/utils/format.ts'

test('拆分国际区号与区号后面的号码', () => {
  assert.deepEqual(getPhoneNumberParts('+1 202 601 600 8'), {
    callingCode: '+1',
    localNumber: '2026016008',
    formattedLocalNumber: '202 601 600 8',
    fullNumber: '+12026016008',
  })
  assert.deepEqual(getPhoneNumberParts('+84 565 295 106'), {
    callingCode: '+84',
    localNumber: '565295106',
    formattedLocalNumber: '565 295 106',
    fullNumber: '+84565295106',
  })
})

test('复制值去除空白并把 00 国际前缀规范为加号', () => {
  assert.deepEqual(getPhoneNumberParts('0 0 84 996 515 059'), {
    callingCode: '+84',
    localNumber: '996515059',
    formattedLocalNumber: '996 515 059',
    fullNumber: '+84996515059',
  })
})

test('复制值会清理号码中的括号和连字符', () => {
  assert.deepEqual(getPhoneNumberParts('+7 (999) 000-11-22'), {
    callingCode: '+7',
    localNumber: '9990001122',
    formattedLocalNumber: '999 000 112 2',
    fullNumber: '+79990001122',
  })
})

test('号码显示继续沿用分组格式并处理空值', () => {
  assert.equal(formatPhoneNumber('+84 996 515 059'), '+84 996 515 059')
  assert.equal(formatPhoneNumber(), '号码分配中')
  assert.equal(getPhoneNumberParts('  '), null)
})
