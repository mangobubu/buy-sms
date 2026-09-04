export interface RenewalIntentSignatureFields {
  orderId: string
  mode: string
  value: number
  unit: string
}

export interface RenewalIntentStorage {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export interface RenewalIntentStore {
  getOrCreate(signature: string): string
  clear(signature: string): void
  read(): Record<string, string>
}

interface RenewalIntentStoreOptions {
  storage: RenewalIntentStorage
  createKey: () => string
  storageKey?: string
}

interface RenewalIntentDocument {
  version: 1
  intents: Record<string, string>
}

export const RENEWAL_INTENT_STORAGE_KEY = 'buy_sms_renewal_intents'

const RETAINED_RENEWAL_FAILURE_CODES = new Set([
  'renewal_in_progress',
  'renewal_result_unknown',
])

export class RenewalIntentPersistenceError extends Error {
  constructor() {
    super('浏览器无法保存续期保护，请允许本站使用会话存储后重试')
    this.name = 'RenewalIntentPersistenceError'
  }
}

export function createRenewalIntentSignature(fields: RenewalIntentSignatureFields): string {
  return JSON.stringify({
    orderId: fields.orderId,
    mode: fields.mode,
    value: fields.value,
    unit: fields.unit,
  })
}

export function createRenewalIdempotencyKey(): string {
  if (typeof globalThis.crypto?.randomUUID === 'function') return globalThis.crypto.randomUUID()
  if (typeof globalThis.crypto?.getRandomValues !== 'function') {
    throw new RenewalIntentPersistenceError()
  }
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

function parseIntents(raw: string | null): Record<string, string> {
  if (!raw) return {}
  try {
    const document = JSON.parse(raw) as Partial<RenewalIntentDocument>
    if (document.version !== 1 || !document.intents || typeof document.intents !== 'object') {
      return {}
    }
    return Object.fromEntries(
      Object.entries(document.intents).filter(
        ([signature, key]) => Boolean(signature) && typeof key === 'string' && Boolean(key),
      ),
    )
  } catch {
    return {}
  }
}

export function createRenewalIntentStore(options: RenewalIntentStoreOptions): RenewalIntentStore {
  const storageKey = options.storageKey ?? RENEWAL_INTENT_STORAGE_KEY

  function read(): Record<string, string> {
    try {
      return parseIntents(options.storage.getItem(storageKey))
    } catch {
      throw new RenewalIntentPersistenceError()
    }
  }

  function write(intents: Record<string, string>): void {
    try {
      if (!Object.keys(intents).length) {
        options.storage.removeItem(storageKey)
        if (options.storage.getItem(storageKey) !== null) throw new Error('remove failed')
        return
      }
      const document = JSON.stringify({ version: 1, intents } satisfies RenewalIntentDocument)
      options.storage.setItem(storageKey, document)
      if (options.storage.getItem(storageKey) !== document) throw new Error('write failed')
    } catch {
      throw new RenewalIntentPersistenceError()
    }
  }

  function getOrCreate(signature: string): string {
    const intents = read()
    const existing = intents[signature]
    if (existing) return existing
    const key = options.createKey()
    intents[signature] = key
    write(intents)
    return key
  }

  function clear(signature: string): void {
    const intents = read()
    if (!(signature in intents)) return
    delete intents[signature]
    write(intents)
  }

  return { getOrCreate, clear, read }
}

function renewalFailureCode(error: unknown): string {
  if (!error || typeof error !== 'object') return ''
  const response = (error as { response?: { data?: unknown } }).response
  const data = response?.data
  if (!data || typeof data !== 'object') return ''
  const code = (data as { code?: unknown }).code
  return typeof code === 'string' ? code.trim().toLowerCase() : ''
}

export function shouldRetainRenewalIntent(error: unknown): boolean {
  const code = renewalFailureCode(error)
  if (RETAINED_RENEWAL_FAILURE_CODES.has(code)) return true
  if (!error || typeof error !== 'object') return false
  const candidate = error as { isAxiosError?: unknown; response?: unknown }
  return candidate.isAxiosError === true && candidate.response == null
}
