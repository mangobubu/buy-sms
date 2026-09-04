export interface PurchaseIntentEntry {
  key: string
  updatedAt: number
  conflictingKeys?: string[]
}

export type PurchaseIntents = Record<string, PurchaseIntentEntry>

export interface PurchaseIntentStorage {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export class PurchaseIntentPersistenceError extends Error {
  constructor() {
    super('浏览器无法保存购买保护，请允许本站使用持久化存储后重试')
    this.name = 'PurchaseIntentPersistenceError'
  }
}

export class PurchaseIntentConflictError extends Error {
  constructor() {
    super(
      '检测到当前平台与购买条件存在多个历史购买编号，购买结果尚未确认。为避免重复扣费，本次不会自动提交。',
    )
    this.name = 'PurchaseIntentConflictError'
  }
}

export interface PurchaseIntentLockManager {
  request<T>(name: string, callback: () => T | Promise<T>): Promise<T>
}

export interface PurchaseIntentStore {
  read(storageKey: string): PurchaseIntents
  getOrCreate(storageKey: string, signature: string): string
  clear(storageKey: string, signature: string): boolean
}

interface PurchaseIntentStoreOptions {
  primaryStorage: PurchaseIntentStorage
  migrationStorage?: PurchaseIntentStorage
  legacyStorageKey?: string
  createKey: () => string
  normalizeSignature?: (signature: string) => string
  now?: () => number
}

interface ParsedPurchaseIntents {
  intents: PurchaseIntents
  authoritative: boolean
}

interface PurchaseIntentDocument {
  version: 1
  intents: PurchaseIntents
}

export interface PurchaseSignatureFields {
  provider: string
  serviceCode: string
  tier?: string
  countryCode: string
  duration?: string
  maxPrice: string
}

export function createPurchaseSignature(fields: PurchaseSignatureFields): string {
  return JSON.stringify({
    provider: fields.provider,
    serviceCode: fields.serviceCode,
    tier: fields.tier || undefined,
    countryCode: fields.countryCode,
    duration: fields.duration?.trim() || undefined,
    maxPrice: fields.maxPrice.trim(),
  })
}

export function normalizePurchaseSignature(signature: string): string {
  try {
    const parsed = JSON.parse(signature) as Partial<Record<keyof PurchaseSignatureFields, unknown>>
    if (
      !parsed ||
      typeof parsed !== 'object' ||
      Array.isArray(parsed) ||
      typeof parsed.provider !== 'string' ||
      typeof parsed.serviceCode !== 'string' ||
      typeof parsed.countryCode !== 'string' ||
      typeof parsed.maxPrice !== 'string'
    ) {
      return signature
    }
    return createPurchaseSignature({
      provider: parsed.provider,
      serviceCode: parsed.serviceCode,
      tier: typeof parsed.tier === 'string' ? parsed.tier : undefined,
      countryCode: parsed.countryCode,
      duration: typeof parsed.duration === 'string' ? parsed.duration : undefined,
      maxPrice: parsed.maxPrice,
    })
  } catch {
    return signature
  }
}

function addPurchaseIntent(
  intents: PurchaseIntents,
  signature: string,
  entry: PurchaseIntentEntry,
  normalizeSignature: (signature: string) => string,
  preferIncoming = false,
): void {
  const normalizedSignature = normalizeSignature(signature)
  const saved = intents[normalizedSignature]
  const incoming: PurchaseIntentEntry = {
    key: entry.key,
    updatedAt: entry.updatedAt,
    ...(entry.conflictingKeys?.length ? { conflictingKeys: [...entry.conflictingKeys] } : {}),
  }
  if (!saved) {
    intents[normalizedSignature] = incoming
    return
  }
  const preferred = preferIncoming || incoming.updatedAt >= saved.updatedAt ? incoming : saved
  const allKeys = new Set([
    saved.key,
    ...(saved.conflictingKeys ?? []),
    incoming.key,
    ...(incoming.conflictingKeys ?? []),
  ])
  allKeys.delete(preferred.key)
  intents[normalizedSignature] = {
    key: preferred.key,
    updatedAt: preferred.updatedAt,
    ...(allKeys.size ? { conflictingKeys: [...allKeys] } : {}),
  }
}

function parsePurchaseIntentRecord(
  value: unknown,
  normalizeSignature: (signature: string) => string,
): PurchaseIntents {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {}
  const intents: PurchaseIntents = {}
  for (const [signature, valueEntry] of Object.entries(value)) {
    if (
      typeof valueEntry !== 'object' ||
      valueEntry === null ||
      typeof (valueEntry as PurchaseIntentEntry).key !== 'string' ||
      typeof (valueEntry as PurchaseIntentEntry).updatedAt !== 'number' ||
      ((valueEntry as PurchaseIntentEntry).conflictingKeys !== undefined &&
        (!Array.isArray((valueEntry as PurchaseIntentEntry).conflictingKeys) ||
          !(valueEntry as PurchaseIntentEntry).conflictingKeys!.every(
            (key) => typeof key === 'string',
          )))
    ) {
      continue
    }
    addPurchaseIntent(intents, signature, valueEntry as PurchaseIntentEntry, normalizeSignature)
  }
  return intents
}

function parsePurchaseIntents(
  raw: string | null,
  now: () => number,
  normalizeSignature: (signature: string) => string,
): ParsedPurchaseIntents {
  if (raw === null) return { intents: {}, authoritative: false }
  try {
    const parsed = JSON.parse(raw) as unknown
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return { intents: {}, authoritative: false }
    }
    const document = parsed as Partial<PurchaseIntentDocument>
    if (document.version === 1 && document.intents && typeof document.intents === 'object') {
      return {
        intents: parsePurchaseIntentRecord(document.intents, normalizeSignature),
        authoritative: true,
      }
    }
    const legacy = parsed as { signature?: unknown; key?: unknown }
    if (typeof legacy.signature === 'string' && typeof legacy.key === 'string') {
      const intents: PurchaseIntents = {}
      addPurchaseIntent(
        intents,
        legacy.signature,
        { key: legacy.key, updatedAt: now() },
        normalizeSignature,
      )
      return { intents, authoritative: false }
    }
    return {
      intents: parsePurchaseIntentRecord(parsed, normalizeSignature),
      authoritative: false,
    }
  } catch {
    return { intents: {}, authoritative: false }
  }
}

export function createPurchaseIntentStore(options: PurchaseIntentStoreOptions): PurchaseIntentStore {
  const now = options.now ?? Date.now
  const normalizeSignature = options.normalizeSignature ?? ((signature: string) => signature)
  const shadow = new Map<string, string | null>()

  function removeMigrationValue(storageKey: string): void {
    try {
      options.migrationStorage?.removeItem(storageKey)
    } catch {
      // 主存储中的版本化文档会阻止旧数据再次合并。
    }
  }

  function removeMigrationValues(storageKey: string): void {
    removeMigrationValue(storageKey)
    const legacyStorageKey = options.legacyStorageKey
    if (legacyStorageKey && legacyStorageKey !== storageKey) removeMigrationValue(legacyStorageKey)
  }

  function writeMigrationFallback(storageKey: string, document: string): void {
    try {
      options.migrationStorage?.setItem(storageKey, document)
    } catch {
      // getOrCreate 会在主存储失败时阻止请求；shadow 仅保留当前页面的状态。
    }
  }

  function write(storageKey: string, intents: PurchaseIntents): boolean {
    if (!storageKey) return false
    const document = JSON.stringify({ version: 1, intents } satisfies PurchaseIntentDocument)
    const removeEmptyValue = !options.migrationStorage && !Object.keys(intents).length
    try {
      if (removeEmptyValue) options.primaryStorage.removeItem(storageKey)
      else options.primaryStorage.setItem(storageKey, document)
      const persisted = options.primaryStorage.getItem(storageKey)
      if (removeEmptyValue ? persisted !== null : persisted !== document) {
        throw new PurchaseIntentPersistenceError()
      }
      shadow.delete(storageKey)
      return true
    } catch {
      shadow.set(storageKey, removeEmptyValue ? null : document)
      writeMigrationFallback(storageKey, document)
      return false
    }
  }

  function readPrimary(storageKey: string): ParsedPurchaseIntents & { shadowed: boolean } {
    if (shadow.has(storageKey)) {
      const parsed = parsePurchaseIntents(shadow.get(storageKey) ?? null, now, normalizeSignature)
      return { ...parsed, authoritative: true, shadowed: true }
    }
    try {
      return {
        ...parsePurchaseIntents(options.primaryStorage.getItem(storageKey), now, normalizeSignature),
        shadowed: false,
      }
    } catch {
      throw new PurchaseIntentPersistenceError()
    }
  }

  function readMigrationValue(storageKey: string): string | null {
    try {
      return options.migrationStorage?.getItem(storageKey) ?? null
    } catch {
      return null
    }
  }

  function read(storageKey: string): PurchaseIntents {
    if (!storageKey) return {}
    const primary = readPrimary(storageKey)
    if (primary.shadowed) return primary.intents
    if (!options.migrationStorage) return primary.intents

    const scopedRaw = readMigrationValue(storageKey)
    const legacyStorageKey = options.legacyStorageKey
    const legacyRaw = legacyStorageKey ? readMigrationValue(legacyStorageKey) : null
    if (scopedRaw === null && (primary.authoritative || legacyRaw === null)) {
      if (primary.authoritative) removeMigrationValues(storageKey)
      return primary.intents
    }

    // 已持久化的 localStorage 值优先，其次是当前用户的 sessionStorage，
    // 最后才是旧版全局 sessionStorage，避免迁移覆盖更可信的新记录。
    const scoped = parsePurchaseIntents(scopedRaw, now, normalizeSignature)
    const sources = primary.authoritative || scoped.authoritative
      ? [scoped.intents, primary.intents]
      : [
          parsePurchaseIntents(legacyRaw, now, normalizeSignature).intents,
          scoped.intents,
          primary.intents,
        ]
    const merged: PurchaseIntents = {}
    for (const source of sources) {
      for (const [signature, entry] of Object.entries(source)) {
        addPurchaseIntent(merged, signature, entry, normalizeSignature, true)
      }
    }
    if (write(storageKey, merged)) removeMigrationValues(storageKey)
    return merged
  }

  function getOrCreate(storageKey: string, signature: string): string {
    const intents = read(storageKey)
    const normalizedSignature = normalizeSignature(signature)
    const saved = intents[normalizedSignature]
    if (saved) {
      if (saved.conflictingKeys?.length) throw new PurchaseIntentConflictError()
      saved.updatedAt = now()
      if (!write(storageKey, intents)) throw new PurchaseIntentPersistenceError()
      removeMigrationValues(storageKey)
      return saved.key
    }
    const key = options.createKey()
    intents[normalizedSignature] = { key, updatedAt: now() }
    if (!write(storageKey, intents)) throw new PurchaseIntentPersistenceError()
    removeMigrationValues(storageKey)
    return key
  }

  function clear(storageKey: string, signature: string): boolean {
    try {
      const intents = read(storageKey)
      delete intents[normalizeSignature(signature)]
      const persisted = write(storageKey, intents)
      if (persisted) removeMigrationValues(storageKey)
      return persisted
    } catch {
      return false
    }
  }

  return { read, getOrCreate, clear }
}

export function getOrCreatePurchaseIntentKey(
  store: PurchaseIntentStore,
  storageKey: string,
  signature: string,
  lockManager?: PurchaseIntentLockManager,
): Promise<string> {
  const getOrCreate = () => store.getOrCreate(storageKey, signature)
  if (!lockManager) return Promise.resolve().then(getOrCreate)
  const lockName = 'buy-sms:purchase-intent:' + storageKey
  return lockManager.request(lockName, getOrCreate)
}

export function clearPurchaseIntent(
  store: PurchaseIntentStore,
  storageKey: string,
  signature: string,
  lockManager?: PurchaseIntentLockManager,
): Promise<boolean> {
  const clear = () => store.clear(storageKey, signature)
  if (!lockManager) return Promise.resolve().then(clear)
  const lockName = 'buy-sms:purchase-intent:' + storageKey
  return lockManager.request(lockName, clear)
}
