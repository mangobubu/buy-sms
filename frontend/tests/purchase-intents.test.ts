import assert from 'node:assert/strict'
import test from 'node:test'
import {
  clearPurchaseIntent,
  createPurchaseIntentStore,
  createPurchaseSignature,
  getOrCreatePurchaseIntentKey,
  normalizePurchaseSignature,
  PurchaseIntentConflictError,
  PurchaseIntentPersistenceError,
  type PurchaseIntentLockManager,
  type PurchaseIntentStorage,
} from '../src/utils/purchase-intents.ts'

class MemoryStorage implements PurchaseIntentStorage {
  private readonly items = new Map<string, string>()

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

function createStore(primary = new MemoryStorage(), migration = new MemoryStorage()) {
  let keySequence = 0
  let timestamp = 0
  const store = createPurchaseIntentStore({
    primaryStorage: primary,
    migrationStorage: migration,
    legacyStorageKey: 'purchase-intent',
    createKey: () => 'key-' + String(++keySequence),
    normalizeSignature: normalizePurchaseSignature,
    now: () => ++timestamp,
  })
  return { store, primary, migration }
}

test('相同签名复用幂等键并刷新更新时间', () => {
  const { store } = createStore()

  assert.equal(store.getOrCreate('purchase-intent:user-1', 'provider-a/service-a'), 'key-1')
  assert.equal(store.getOrCreate('purchase-intent:user-1', 'provider-a/service-a'), 'key-1')
  assert.equal(store.read('purchase-intent:user-1')['provider-a/service-a']?.updatedAt, 2)
})

test('不同签名和供应商使用不同幂等键', () => {
  const { store } = createStore()

  const first = store.getOrCreate('purchase-intent:user-1', 'provider-a/service-a')
  const second = store.getOrCreate('purchase-intent:user-1', 'provider-a/service-b')
  const third = store.getOrCreate('purchase-intent:user-1', 'provider-b/service-a')

  assert.notEqual(first, second)
  assert.notEqual(first, third)
  assert.equal(store.getOrCreate('purchase-intent:user-1', 'provider-a/service-a'), first)
})

test('清理只影响当前签名', () => {
  const { store } = createStore()
  const first = store.getOrCreate('purchase-intent:user-1', 'provider-a/service-a')
  const second = store.getOrCreate('purchase-intent:user-1', 'provider-b/service-a')

  store.clear('purchase-intent:user-1', 'provider-a/service-a')

  assert.notEqual(store.getOrCreate('purchase-intent:user-1', 'provider-a/service-a'), first)
  assert.equal(store.getOrCreate('purchase-intent:user-1', 'provider-b/service-a'), second)
})

test('读取时合并并清理用户级和旧全局 sessionStorage', () => {
  const primary = new MemoryStorage()
  const migration = new MemoryStorage()
  primary.setItem(
    'purchase-intent:user-1',
    JSON.stringify({ shared: { key: 'local-key', updatedAt: 1 } }),
  )
  migration.setItem(
    'purchase-intent:user-1',
    JSON.stringify({
      scoped: { key: 'scoped-key', updatedAt: 2 },
      shared: { key: 'stale-session-key', updatedAt: 2 },
    }),
  )
  migration.setItem(
    'purchase-intent',
    JSON.stringify({ signature: 'legacy', key: 'legacy-key' }),
  )
  const { store } = createStore(primary, migration)

  const intents = store.read('purchase-intent:user-1')

  assert.equal(intents.shared?.key, 'local-key')
  assert.deepEqual(intents.shared?.conflictingKeys, ['stale-session-key'])
  assert.equal(intents.scoped?.key, 'scoped-key')
  assert.equal(intents.legacy?.key, 'legacy-key')
  assert.equal(migration.getItem('purchase-intent:user-1'), null)
  assert.equal(migration.getItem('purchase-intent'), null)
  assert.deepEqual(
    JSON.parse(primary.getItem('purchase-intent:user-1') ?? '{}'),
    { version: 1, intents },
  )
  assert.throws(
    () => store.getOrCreate('purchase-intent:user-1', 'shared'),
    PurchaseIntentConflictError,
  )
})

test('历史字段顺序的签名迁移后仍复用原幂等键', () => {
  const primary = new MemoryStorage()
  const migration = new MemoryStorage()
  const legacySignature =
    '{"provider":"herosms","countryCode":"84","serviceCode":"momo","maxPrice":"0.09"}'
  migration.setItem(
    'purchase-intent',
    JSON.stringify({ signature: legacySignature, key: 'legacy-key' }),
  )
  const { store } = createStore(primary, migration)
  const currentSignature = createPurchaseSignature({
    provider: 'herosms',
    serviceCode: 'momo',
    countryCode: '84',
    maxPrice: '0.09',
  })

  assert.equal(store.getOrCreate('purchase-intent:user-1', currentSignature), 'legacy-key')
  assert.equal(Object.keys(store.read('purchase-intent:user-1')).length, 1)
})

test('新版主存储与旧标签页用户级会话键冲突时阻止提交', () => {
  const primary = new MemoryStorage()
  const migration = new MemoryStorage()
  const storageKey = 'purchase-intent:user-1'
  const signature = createPurchaseSignature({
    provider: 'herosms',
    serviceCode: 'momo',
    countryCode: '84',
    maxPrice: '0.09',
  })
  primary.setItem(
    storageKey,
    JSON.stringify({
      version: 1,
      intents: { [signature]: { key: 'key-a', updatedAt: 2 } },
    }),
  )
  migration.setItem(
    storageKey,
    JSON.stringify({ [signature]: { key: 'key-b', updatedAt: 1 } }),
  )
  const { store } = createStore(primary, migration)

  const intents = store.read(storageKey)

  assert.equal(intents[signature]?.key, 'key-a')
  assert.deepEqual(intents[signature]?.conflictingKeys, ['key-b'])
  assert.throws(() => store.getOrCreate(storageKey, signature), PurchaseIntentConflictError)
  assert.equal(migration.getItem(storageKey), null)
})

class ThrowingStorage implements PurchaseIntentStorage {
  getItem(): string | null {
    throw new Error('get failed')
  }

  setItem(): void {
    throw new Error('set failed')
  }

  removeItem(): void {
    throw new Error('remove failed')
  }
}

test('主存储不可读时阻止创建新的购买请求', () => {
  const migration = new MemoryStorage()
  const storageKey = 'purchase-intent:user-1'
  const signature = createPurchaseSignature({
    provider: 'herosms',
    serviceCode: 'momo',
    countryCode: '84',
    maxPrice: '0.09',
  })
  migration.setItem(
    storageKey,
    JSON.stringify({ [signature]: { key: 'session-key', updatedAt: 1 } }),
  )
  const store = createPurchaseIntentStore({
    primaryStorage: new ThrowingStorage(),
    migrationStorage: migration,
    legacyStorageKey: 'purchase-intent',
    createKey: () => 'new-key',
    normalizeSignature: normalizePurchaseSignature,
    now: () => 2,
  })

  assert.throws(
    () => store.getOrCreate(storageKey, signature),
    PurchaseIntentPersistenceError,
  )
  assert.notEqual(migration.getItem(storageKey), null)
})

class WriteFailingStorage extends MemoryStorage {
  override setItem(): void {
    throw new Error('set failed')
  }
}

test('主存储写入失败时保留 session 降级副本并阻止请求', () => {
  const migration = new MemoryStorage()
  const storageKey = 'purchase-intent:user-1'
  const signature = createPurchaseSignature({
    provider: 'herosms',
    serviceCode: 'momo',
    countryCode: '84',
    maxPrice: '0.09',
  })
  const store = createPurchaseIntentStore({
    primaryStorage: new WriteFailingStorage(),
    migrationStorage: migration,
    legacyStorageKey: 'purchase-intent',
    createKey: () => 'protected-key',
    normalizeSignature: normalizePurchaseSignature,
    now: () => 1,
  })

  assert.throws(() => store.getOrCreate(storageKey, signature), PurchaseIntentPersistenceError)
  assert.deepEqual(JSON.parse(migration.getItem(storageKey) ?? '{}'), {
    version: 1,
    intents: {
      [signature]: { key: 'protected-key', updatedAt: 1 },
    },
  })
})

class SerialLockManager implements PurchaseIntentLockManager {
  readonly names: string[] = []
  private tail: Promise<void> = Promise.resolve()

  async request<T>(name: string, callback: () => T | Promise<T>): Promise<T> {
    this.names.push(name)
    const previous = this.tail
    let release = () => {}
    this.tail = new Promise<void>((resolve) => {
      release = resolve
    })
    await previous
    try {
      return await callback()
    } finally {
      release()
    }
  }
}

test('同用户同签名通过同一锁复用幂等键', async () => {
  const primary = new MemoryStorage()
  const migration = new MemoryStorage()
  const lockManager = new SerialLockManager()
  const signature = createPurchaseSignature({
    provider: 'herosms',
    serviceCode: 'momo',
    countryCode: '84',
    maxPrice: '0.09',
  })
  const storeA = createPurchaseIntentStore({
    primaryStorage: primary,
    migrationStorage: migration,
    createKey: () => 'key-a',
    normalizeSignature: normalizePurchaseSignature,
  })
  const storeB = createPurchaseIntentStore({
    primaryStorage: primary,
    migrationStorage: migration,
    createKey: () => 'key-b',
    normalizeSignature: normalizePurchaseSignature,
  })

  const keys = await Promise.all([
    getOrCreatePurchaseIntentKey(storeA, 'purchase-intent:user-1', signature, lockManager),
    getOrCreatePurchaseIntentKey(storeB, 'purchase-intent:user-1', signature, lockManager),
  ])

  assert.deepEqual(keys, ['key-a', 'key-a'])
  assert.equal(new Set(lockManager.names).size, 1)
})

test('同用户不同签名的创建和清理共用锁且不丢记录', async () => {
  const primary = new MemoryStorage()
  const migration = new MemoryStorage()
  const lockManager = new SerialLockManager()
  const storageKey = 'purchase-intent:user-1'
  const storeA = createPurchaseIntentStore({
    primaryStorage: primary,
    migrationStorage: migration,
    createKey: () => 'key-a',
  })
  const storeB = createPurchaseIntentStore({
    primaryStorage: primary,
    migrationStorage: migration,
    createKey: () => 'key-b',
  })
  storeA.getOrCreate(storageKey, 'signature-a')

  const [, keyB] = await Promise.all([
    clearPurchaseIntent(storeA, storageKey, 'signature-a', lockManager),
    getOrCreatePurchaseIntentKey(storeB, storageKey, 'signature-b', lockManager),
  ])

  assert.equal(keyB, 'key-b')
  assert.equal(storeA.read(storageKey)['signature-a'], undefined)
  assert.equal(storeA.read(storageKey)['signature-b']?.key, 'key-b')
  assert.equal(new Set(lockManager.names).size, 1)
})
