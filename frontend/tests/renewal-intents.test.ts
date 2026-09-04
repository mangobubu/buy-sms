import assert from 'node:assert/strict'
import test from 'node:test'
import {
  createRenewalIdempotencyKey,
  createRenewalIntentSignature,
  createRenewalIntentStore,
  RenewalIntentPersistenceError,
  shouldRetainRenewalIntent,
  type RenewalIntentStorage,
} from '../src/utils/renewal-intents.ts'

class MemoryStorage implements RenewalIntentStorage {
  readonly items = new Map<string, string>()

  getItem(key: string): string | null {
    return this.items.get(key) ?? null
  }

  setItem(key: string, value: string): void {
    this.items.set(key, value)
  }

  removeItem(key: string): void {
    this.items.delete(key)
  }
}

function signature(overrides: Partial<Parameters<typeof createRenewalIntentSignature>[0]> = {}) {
  return createRenewalIntentSignature({
    orderId: 'order-1',
    mode: 'prolong',
    value: 24,
    unit: 'hour',
    ...overrides,
  })
}

function createStore(storage = new MemoryStorage()) {
  let sequence = 0
  return {
    storage,
    store: createRenewalIntentStore({
      storage,
      createKey: () => `00000000-0000-4000-8000-${String(++sequence).padStart(12, '0')}`,
    }),
  }
}

test('续期签名只由订单、模式、值和单位组成', () => {
  assert.equal(
    signature(),
    JSON.stringify({ orderId: 'order-1', mode: 'prolong', value: 24, unit: 'hour' }),
  )
})

test('相同续期方案复用会话幂等键', () => {
  const { store } = createStore()
  const currentSignature = signature()

  const first = store.getOrCreate(currentSignature)
  const second = store.getOrCreate(currentSignature)

  assert.equal(first, second)
  assert.equal(store.read()[currentSignature], first)
})

test('同一 sessionStorage 在视图重建后仍复用幂等键', () => {
  const storage = new MemoryStorage()
  const firstStore = createRenewalIntentStore({ storage, createKey: () => 'first-key' })
  const currentSignature = signature()
  assert.equal(firstStore.getOrCreate(currentSignature), 'first-key')

  const rebuiltStore = createRenewalIntentStore({ storage, createKey: () => 'unexpected-key' })
  assert.equal(rebuiltStore.getOrCreate(currentSignature), 'first-key')
})

test('订单、模式、值或单位变化时使用不同幂等键', () => {
  const { store } = createStore()
  const keys = [
    signature(),
    signature({ orderId: 'order-2' }),
    signature({ mode: 'reactivate' }),
    signature({ value: 48 }),
    signature({ unit: 'minute' }),
  ].map((currentSignature) => store.getOrCreate(currentSignature))

  assert.equal(new Set(keys).size, keys.length)
})

test('明确完成后只清除当前续期方案', () => {
  const { store, storage } = createStore()
  const firstSignature = signature()
  const secondSignature = signature({ value: 48 })
  const firstKey = store.getOrCreate(firstSignature)
  const secondKey = store.getOrCreate(secondSignature)

  store.clear(firstSignature)

  assert.equal(store.read()[firstSignature], undefined)
  assert.equal(store.getOrCreate(secondSignature), secondKey)
  assert.notEqual(store.getOrCreate(firstSignature), firstKey)
  store.clear(firstSignature)
  store.clear(secondSignature)
  assert.equal(storage.items.size, 0)
})

test('存储写入失败时阻止生成无法复用的键', () => {
  const storage: RenewalIntentStorage = {
    getItem: () => null,
    setItem: () => undefined,
    removeItem: () => undefined,
  }
  const store = createRenewalIntentStore({ storage, createKey: () => 'unpersisted-key' })

  assert.throws(() => store.getOrCreate(signature()), RenewalIntentPersistenceError)
})

test('断网和结果待确认错误保留续期幂等键', () => {
  assert.equal(shouldRetainRenewalIntent({ isAxiosError: true }), true)
  assert.equal(
    shouldRetainRenewalIntent({ response: { data: { code: 'renewal_in_progress' } } }),
    true,
  )
  assert.equal(
    shouldRetainRenewalIntent({ response: { data: { code: ' RENEWAL_RESULT_UNKNOWN ' } } }),
    true,
  )
})

test('有明确响应的其他业务失败清除续期幂等键', () => {
  assert.equal(
    shouldRetainRenewalIntent({
      isAxiosError: true,
      response: { status: 409, data: { code: 'renewal_price_changed' } },
    }),
    false,
  )
  assert.equal(shouldRetainRenewalIntent(new Error('validation failed')), false)
})

test('生成标准 UUID v4 续期幂等键', () => {
  assert.match(
    createRenewalIdempotencyKey(),
    /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i,
  )
})
