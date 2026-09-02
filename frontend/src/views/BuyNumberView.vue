<script setup lang="ts">
import axios from 'axios'
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Check, Refresh, ShoppingCart } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import OrdersView from '@/views/OrdersView.vue'
import { providersApi } from '@/api/providers'
import { catalogApi } from '@/api/catalog'
import { ordersApi } from '@/api/orders'
import { errorCode, errorMessage } from '@/api/http'
import { authSession } from '@/stores/auth'
import type {
  CountryOption,
  ProviderBalance,
  ProviderCode,
  ProviderConfig,
  Quote,
  ServiceOption,
  SmsBowerTier,
} from '@/types/api'
import { formatMoney, providerName } from '@/utils/format'
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
} from '@/utils/purchase-intents'

const formRef = ref<FormInstance>()
const ordersRef = ref<InstanceType<typeof OrdersView>>()
const loadingProviders = ref(false)
const loadingBalances = ref(false)
const loadingCountries = ref(false)
const loadingServices = ref(false)
const loadingQuote = ref(false)
const purchasing = ref(false)
const PURCHASE_INTENT_KEY = 'buy_sms_purchase_intent'
const FORM_SELECTION_KEY = 'buy_sms_form_selection'
const TERMINAL_PURCHASE_FAILURE_CODES = new Set([
  'configuration',
  'idempotency_mismatch',
  'insufficient_balance',
  'invalid_selection',
  'no_numbers',
  'provider_disabled',
  'provider_preflight_error',
  'provider_rate_limited',
  'price_exceeded',
  'purchase_setup_failed',
])
const UNCERTAIN_PURCHASE_FAILURE_CODES = new Set([
  'provider_error',
  'purchase_failed',
  'purchase_result_unknown',
  'database_error',
])
const providers = ref<ProviderConfig[]>([])
const providerBalances = ref<Partial<Record<ProviderCode, ProviderBalance>>>({})
const countries = ref<CountryOption[]>([])
const services = ref<ServiceOption[]>([])
const quotes = ref<Quote[]>([])
const smsBowerTiers: { value: SmsBowerTier; label: string }[] = [
  { value: 'bronze', label: 'Bronze' },
  { value: 'silver', label: 'Silver' },
  { value: 'gold', label: 'Gold' },
]

let servicesGeneration = 0
let countriesGeneration = 0
let quoteGeneration = 0
let providerRestoreGeneration = 0
let balanceRefreshTimer: number | undefined
let balanceAbortController: AbortController | undefined
let balanceRefreshPromise: Promise<void> | undefined
let disposed = false
const restoringProviderSelection = ref(false)

const form = reactive({
  provider: '' as ProviderCode | '',
  serviceCode: '',
  tier: '' as SmsBowerTier | '',
  countryCode: '',
  priceSelection: '',
  maxPrice: '',
})

const rules: FormRules = {
  provider: [{ required: true, message: '请选择供应商', trigger: 'change' }],
  serviceCode: [{ required: true, message: '请选择接码服务', trigger: 'change' }],
  countryCode: [{ required: true, message: '请选择国家或地区', trigger: 'change' }],
  priceSelection: [{ required: true, message: '请选择价格', trigger: 'change' }],
}

function providerPurchasable(provider: ProviderConfig): boolean {
  return provider.enabled && provider.purchasable && (providerBalances.value[provider.code]?.purchasable ?? true)
}

const purchasableProviders = computed(() => providers.value.filter(providerPurchasable))

function providerBalanceText(provider: ProviderConfig): string {
  if (!provider.enabled) return '接口未启用 · 余额 —'
  const current = providerBalances.value[provider.code]
  if (!current && loadingBalances.value) return '接口已启用 · 余额查询中'
  if (current?.stale && current.balance !== undefined) {
    return `接口已启用 · 上次余额 ${formatMoney(current.balance, current.currency || 'USD')}`
  }
  if (current?.status === 'ok' && current.balance !== undefined) {
    return `接口已启用 · 余额 ${formatMoney(current.balance, current.currency || 'USD')}`
  }
  if (current?.status === 'unconfigured') return '接口不可用 · 余额 —'
  if (current?.purchasable === false) return '接口不可用 · 余额 —'
  if (current?.status === 'timeout') return '接口已启用 · 余额查询超时'
  if (current?.status === 'unavailable') return '接口已启用 · 余额暂不可用'
  return '接口已启用 · 余额查询中'
}

function selectPurchasableProvider(): void {
  const selected = providers.value.find((item) => item.code === form.provider)
  if (selected && providerPurchasable(selected)) return
  if (form.provider) form.provider = ''
  const preferredProvider = purchasableProviders.value.find((item) => item.code === persistedProviderPreference)
  const nextProvider = preferredProvider || purchasableProviders.value[0]
  if (nextProvider) form.provider = nextProvider.code
}
interface DisplayPriceOption {
  key: string
  price: string
  available: number
  currency: string
  tier?: SmsBowerTier
}

interface ProviderSelection {
  serviceCode: string
  tier: SmsBowerTier | ''
  countryCode: string
  priceSelection: string
  maxPrice: string
}

interface PersistedProviderSelection {
  serviceCode: string
  countryCode: string
}

const providerCodes: ProviderCode[] = ['herosms', 'smsbower', 'smspool']
const providerSelections: Partial<Record<ProviderCode, ProviderSelection>> = {}
let persistedProviderPreference: ProviderCode | '' = ''

function priceOptionKey(tier: SmsBowerTier | undefined, price: string): string {
  return `${tier || 'standard'}:${price}`
}

const priceOptions = computed(() => {
  return quotes.value.flatMap((currentQuote) => {
    const options = currentQuote.priceOptions?.length
      ? currentQuote.priceOptions
      : [{ price: currentQuote.price, available: currentQuote.available }]
    return options.map<DisplayPriceOption>((option) => ({
      ...option,
      key: priceOptionKey(currentQuote.tier, option.price),
      currency: currentQuote.currency || 'USD',
      tier: currentQuote.tier,
    }))
  })
})
const selectedPriceOption = computed(() =>
  priceOptions.value.find((option) => option.key === form.priceSelection),
)

function smsBowerTierLabel(tier?: SmsBowerTier): string {
  return smsBowerTiers.find((option) => option.value === tier)?.label || ''
}

function priceOptionLabel(option: DisplayPriceOption): string {
  const price = formatMoney(option.price, option.currency)
  return option.tier ? `${smsBowerTierLabel(option.tier)} · ${price}` : price
}

function snapshotSelection(): ProviderSelection {
  return {
    serviceCode: form.serviceCode,
    tier: form.tier,
    countryCode: form.countryCode,
    priceSelection: form.priceSelection,
    maxPrice: form.maxPrice,
  }
}

function formSelectionStorageKey(): string {
  const userID = authSession.state.user?.id
  return userID ? `${FORM_SELECTION_KEY}:${userID}` : ''
}

function isProviderCode(value: unknown): value is ProviderCode {
  return typeof value === 'string' && providerCodes.includes(value as ProviderCode)
}

function readPersistedFormSelections(): ProviderCode | '' {
  const storageKey = formSelectionStorageKey()
  if (!storageKey) return ''
  try {
    const parsed = JSON.parse(sessionStorage.getItem(storageKey) ?? '{}') as {
      provider?: unknown
      selections?: Record<string, unknown>
    }
    if (parsed.selections && typeof parsed.selections === 'object') {
      for (const provider of providerCodes) {
        const raw = parsed.selections[provider]
        if (!raw || typeof raw !== 'object' || Array.isArray(raw)) continue
        const selection = raw as { serviceCode?: unknown; countryCode?: unknown }
        const serviceCode = typeof selection.serviceCode === 'string' ? selection.serviceCode : ''
        const countryCode = typeof selection.countryCode === 'string' ? selection.countryCode : ''
        providerSelections[provider] = {
          serviceCode,
          countryCode,
          tier: '',
          priceSelection: '',
          maxPrice: '',
        }
      }
    }
    return isProviderCode(parsed.provider) ? parsed.provider : ''
  } catch {
    try {
      sessionStorage.removeItem(storageKey)
    } catch {
      // 会话存储整体不可用时回退为当前页面内状态。
    }
    return ''
  }
}

function persistFormSelections(): void {
  const storageKey = formSelectionStorageKey()
  if (!storageKey || !form.provider) return
  const selections: Partial<Record<ProviderCode, PersistedProviderSelection>> = {}
  for (const provider of providerCodes) {
    const selection = providerSelections[provider]
    if (!selection) continue
    selections[provider] = {
      serviceCode: selection.serviceCode,
      countryCode: selection.countryCode,
    }
  }
  try {
    sessionStorage.setItem(storageKey, JSON.stringify({ provider: form.provider, selections }))
  } catch {
    // 会话存储不可用时仍保留当前页面内的选择。
  }
}

function saveCurrentSelection(provider = form.provider): void {
  if (!provider) return
  providerSelections[provider] = snapshotSelection()
  persistFormSelections()
}

function clearPriceSelection(): void {
  form.tier = ''
  form.priceSelection = ''
  form.maxPrice = ''
  quotes.value = []
}

function selectPrice(selection: string): void {
  form.priceSelection = selection
  const option = priceOptions.value.find((item) => item.key === selection)
  form.tier = option?.tier || ''
  form.maxPrice = option?.price || ''
  saveCurrentSelection()
}

async function loadProviders(): Promise<void> {
  loadingProviders.value = true
  try {
    const result = await providersApi.list()
    if (disposed) return
    providers.value = result
    const selected = providers.value.find((item) => item.code === form.provider)
    if (selected && (!selected.enabled || providerBalances.value[selected.code]?.purchasable === false)) form.provider = ''
    if (!form.provider) persistedProviderPreference = readPersistedFormSelections()
    selectPurchasableProvider()
  } catch (reason) {
    if (!disposed) ElMessage.error(errorMessage(reason, '供应商加载失败'))
  } finally {
    if (!disposed) loadingProviders.value = false
  }
}

function loadProviderBalances(notify = false): Promise<void> {
  if (disposed || document.hidden || !navigator.onLine) return Promise.resolve()
  if (balanceRefreshPromise) return balanceRefreshPromise
  const request = performProviderBalanceRefresh(notify)
  let tracked: Promise<void>
  tracked = request.finally(() => {
    if (balanceRefreshPromise === tracked) balanceRefreshPromise = undefined
  })
  balanceRefreshPromise = tracked
  return tracked
}

async function forceLoadProviderBalances(notify = false): Promise<void> {
  if (disposed || document.hidden || !navigator.onLine) return
  const currentRequest = balanceRefreshPromise
  if (currentRequest) {
    balanceAbortController?.abort()
    await currentRequest
  }
  await loadProviderBalances(notify)
}

async function performProviderBalanceRefresh(notify: boolean): Promise<void> {
  loadingBalances.value = true
  const controller = new AbortController()
  balanceAbortController = controller
  try {
    const result = await providersApi.balances(controller.signal)
    if (disposed) return
    providerBalances.value = Object.fromEntries(result.map((item) => {
      const previous = providerBalances.value[item.code]
      if (item.purchasable && item.status !== 'ok' && previous?.balance !== undefined) {
        return [item.code, {
          ...item,
          balance: previous.balance,
          currency: previous.currency,
          lastCheckedAt: previous.lastCheckedAt,
          stale: true,
        } satisfies ProviderBalance]
      }
      return [item.code, item]
    }))
    const enabled = new Map(result.map((item) => [item.code, item.enabled]))
    providers.value = providers.value.map((item) =>
      enabled.has(item.code) ? { ...item, enabled: enabled.get(item.code)! } : item,
    )
    selectPurchasableProvider()
  } catch (reason) {
    if (disposed || controller.signal.aborted) return
    if (notify) ElMessage.error(errorMessage(reason, '余额刷新失败'))
    const unavailable: Partial<Record<ProviderCode, ProviderBalance>> = {}
    for (const item of providers.value) {
      const previous = providerBalances.value[item.code]
      const purchasable = item.purchasable && (previous?.purchasable ?? true)
      unavailable[item.code] = {
        code: item.code,
        name: item.name,
        enabled: item.enabled,
        purchasable,
        status: item.enabled ? 'unavailable' : 'disabled',
        balance: purchasable ? previous?.balance : undefined,
        currency: purchasable ? previous?.currency : undefined,
        lastCheckedAt: purchasable ? previous?.lastCheckedAt : undefined,
        stale: purchasable && previous?.balance !== undefined,
        message: !item.enabled ? '接口未启用' : purchasable ? '余额刷新失败，当前显示上次查询结果' : '接口不可用',
      }
    }
    providerBalances.value = unavailable
    selectPurchasableProvider()
  } finally {
    if (balanceAbortController === controller) balanceAbortController = undefined
    if (!disposed) loadingBalances.value = false
  }
}

async function refreshProviders(): Promise<void> {
  await loadProviders()
  await forceLoadProviderBalances(true)
}

function handleVisibilityChange(): void {
  if (!document.hidden && navigator.onLine) void loadProviderBalances()
}

async function loadServices(provider: ProviderCode): Promise<ServiceOption[] | null> {
  const generation = ++servicesGeneration
  loadingServices.value = true
  try {
    const result = await catalogApi.services(provider)
    if (generation !== servicesGeneration || form.provider !== provider) return null
    services.value = result
    return result
  } catch (reason) {
    if (generation === servicesGeneration) ElMessage.error(errorMessage(reason, '服务列表加载失败'))
    return null
  } finally {
    if (generation === servicesGeneration) loadingServices.value = false
  }
}

function mergeCountries(groups: CountryOption[][]): CountryOption[] {
  const merged = new Map<string, CountryOption>()
  for (const group of groups) {
    for (const country of group) {
      const existing = merged.get(country.code)
      if (!existing) {
        merged.set(country.code, { ...country })
        continue
      }
      if (!existing.name && country.name) existing.name = country.name
      if (!existing.flag && country.flag) existing.flag = country.flag
      if (country.available !== undefined) existing.available = (existing.available ?? 0) + country.available
    }
  }
  return [...merged.values()].sort((left, right) => left.name.localeCompare(right.name, 'zh-CN'))
}

async function loadCountries(): Promise<CountryOption[] | null> {
  if (!form.provider || !form.serviceCode) return null
  const provider = form.provider
  const service = form.serviceCode
  const generation = ++countriesGeneration
  loadingCountries.value = true
  try {
    let result: CountryOption[]
    let loadedTierCount = smsBowerTiers.length
    if (provider === 'smsbower') {
      const settled = await Promise.allSettled(
        smsBowerTiers.map(({ value }) => catalogApi.countries(provider, service, value)),
      )
      const succeeded = settled
        .filter((entry): entry is PromiseFulfilledResult<CountryOption[]> => entry.status === 'fulfilled')
        .map((entry) => entry.value)
      if (!succeeded.length) throw settled.find((entry) => entry.status === 'rejected')?.reason
      loadedTierCount = succeeded.length
      result = mergeCountries(succeeded)
    } else {
      result = await catalogApi.countries(provider, service)
    }
    if (generation !== countriesGeneration || form.provider !== provider || form.serviceCode !== service) return null
    countries.value = result
    if (provider === 'smsbower' && loadedTierCount < smsBowerTiers.length) {
      ElMessage.warning('部分号码等级的国家暂未加载')
    }
    return result
  } catch (reason) {
    if (generation === countriesGeneration) ElMessage.error(errorMessage(reason, '国家列表加载失败'))
    return null
  } finally {
    if (generation === countriesGeneration) loadingCountries.value = false
  }
}

async function loadQuote(options: { requireSelection?: boolean } = {}): Promise<Quote[] | null> {
  if (!form.provider || !form.countryCode || !form.serviceCode) return null
  const provider = form.provider
  const country = form.countryCode
  const service = form.serviceCode
  const savedSelection = form.priceSelection
  const generation = ++quoteGeneration
  loadingQuote.value = true
  try {
    let result: Quote[]
    if (provider === 'smsbower') {
      const settled = await Promise.allSettled(
        smsBowerTiers.map(async ({ value }) => ({ ...(await catalogApi.quote(provider, country, service, value)), tier: value })),
      )
      result = settled.flatMap((entry) => entry.status === 'fulfilled' ? [entry.value] : [])
      if (!result.length) throw settled.find((entry) => entry.status === 'rejected')?.reason
    } else {
      result = [await catalogApi.quote(provider, country, service)]
    }
    if (
      generation !== quoteGeneration ||
      form.provider !== provider ||
      form.countryCode !== country ||
      form.serviceCode !== service
    ) return null
    quotes.value = result
    if (provider === 'smsbower' && result.length < smsBowerTiers.length) {
      ElMessage.warning('部分号码等级的价格暂未加载')
    }
    const nextSelection = options.requireSelection
      ? ''
      : priceOptions.value.some((option) => option.key === savedSelection)
        ? savedSelection
        : priceOptions.value[0]?.key || ''
    selectPrice(nextSelection)
    return result
  } catch (reason) {
    if (generation === quoteGeneration) ElMessage.error(errorMessage(reason, '报价加载失败'))
    return null
  } finally {
    if (generation === quoteGeneration) loadingQuote.value = false
  }
}

async function restoreProviderSelection(provider: ProviderCode, selection?: ProviderSelection): Promise<void> {
  const generation = ++providerRestoreGeneration
  restoringProviderSelection.value = true
  try {
    const loadedServices = await loadServices(provider)
    if (generation !== providerRestoreGeneration || form.provider !== provider || !loadedServices) return
    if (!selection || !loadedServices.some((service) => service.code === selection.serviceCode)) return

    form.serviceCode = selection.serviceCode
    const loadedCountries = await loadCountries()
    if (generation !== providerRestoreGeneration || form.provider !== provider || !loadedCountries) return
    if (!loadedCountries.some((country) => country.code === selection.countryCode)) return

    form.countryCode = selection.countryCode
    form.priceSelection = selection.priceSelection
    await loadQuote({ requireSelection: !selection.priceSelection })
  } finally {
    if (generation === providerRestoreGeneration) {
      restoringProviderSelection.value = false
    }
  }
}

watch(
  () => form.provider,
  (provider) => {
    providerRestoreGeneration += 1
    servicesGeneration += 1
    countriesGeneration += 1
    quoteGeneration += 1
    loadingServices.value = false
    loadingCountries.value = false
    loadingQuote.value = false
    form.serviceCode = ''
    form.tier = ''
    form.countryCode = ''
    form.priceSelection = ''
    form.maxPrice = ''
    services.value = []
    countries.value = []
    quotes.value = []
    restoringProviderSelection.value = false
    persistFormSelections()
    if (provider) void restoreProviderSelection(provider, providerSelections[provider])
  },
)

watch(
  () => form.serviceCode,
  (service) => {
    if (restoringProviderSelection.value) return
    countriesGeneration += 1
    quoteGeneration += 1
    loadingCountries.value = false
    loadingQuote.value = false
    form.countryCode = ''
    countries.value = []
    clearPriceSelection()
    saveCurrentSelection()
    if (service) void loadCountries()
  },
)

watch(
  () => form.countryCode,
  (country) => {
    if (restoringProviderSelection.value) return
    quoteGeneration += 1
    loadingQuote.value = false
    clearPriceSelection()
    saveCurrentSelection()
    if (country) void loadQuote()
  },
)

function newIdempotencyKey(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80
  return Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('')
}

function browserStorage(name: 'localStorage' | 'sessionStorage'): PurchaseIntentStorage {
  return {
    getItem: (key) => window[name].getItem(key),
    setItem: (key, value) => window[name].setItem(key, value),
    removeItem: (key) => window[name].removeItem(key),
  }
}

const purchaseIntentStore = createPurchaseIntentStore({
  primaryStorage: browserStorage('localStorage'),
  migrationStorage: browserStorage('sessionStorage'),
  legacyStorageKey: PURCHASE_INTENT_KEY,
  createKey: newIdempotencyKey,
  normalizeSignature: normalizePurchaseSignature,
})
const purchaseIntentLockManager: PurchaseIntentLockManager | undefined =
  typeof navigator !== 'undefined' && navigator.locks
    ? {
        request: <T,>(name: string, callback: () => T | Promise<T>) =>
          navigator.locks.request(name, () => callback()),
      }
    : undefined

interface PurchaseConditionSnapshot {
  provider: ProviderCode
  serviceCode: string
  tier: SmsBowerTier | ''
  countryCode: string
  maxPrice: string
}

function purchaseIntentStorageKey(): string {
  const userID = authSession.state.user?.id
  return userID ? `${PURCHASE_INTENT_KEY}:${userID}` : ''
}

function uncertainTransportFailureMessage(reason: unknown, applicationCode: string): string {
  if (!axios.isAxiosError(reason)) return ''
  const code = reason.code?.toUpperCase()
  if (!reason.response) {
    if (code === 'ECONNABORTED' || code === 'ETIMEDOUT') {
      return '页面等待购买结果超时，购买结果尚未确认。系统尚未确认供应商是否已生成号码或当前订单是否已保存。为避免重复扣费，已暂停当前平台与当前购买条件的再次提交。'
    }
    if (code === 'ERR_NETWORK') {
      return '购买连接中断，购买结果尚未确认。系统尚未确认供应商是否已生成号码或当前订单是否已保存。为避免重复扣费，已暂停当前平台与当前购买条件的再次提交。'
    }
    return ''
  }
  if (!applicationCode && [500, 502, 503, 504].includes(reason.response.status)) {
    return '网关或服务响应异常，购买结果尚未确认。系统尚未确认供应商是否已生成号码或当前订单是否已保存。为避免重复扣费，已暂停当前平台与当前购买条件的再次提交。'
  }
  return ''
}

async function confirmPurchaseRetryUnlock(
  storageKey: string,
  signature: string,
  conditions: PurchaseConditionSnapshot,
  failureMessage: string,
): Promise<void> {
  const conditionSummary =
    '平台：' +
    providerName(conditions.provider) +
    '；服务：' +
    conditions.serviceCode +
    '；国家或地区：' +
    conditions.countryCode +
    '；号码等级：' +
    (conditions.tier || '默认') +
    '；最高价格：' +
    formatMoney(conditions.maxPrice, 'USD')
  try {
    await ElMessageBox.confirm(
      failureMessage +
        ' 本次请求条件为：' +
        conditionSummary +
        '。重新购买前，请先刷新当前订单，并登录该供应商后台，按以上条件确认没有生成号码，也没有产生扣费。确认后只会解除上述平台与购买条件的重试保护，其他平台和购买条件不受影响。',
      '购买结果待核对',
      {
        confirmButtonText: '已核对，允许重新购买',
        cancelButtonText: '保留重试保护',
        type: 'warning',
      },
    )
  } catch {
    return
  }
  if (await clearPurchaseIntent(purchaseIntentStore, storageKey, signature, purchaseIntentLockManager)) {
    if (!disposed) ElMessage.success('已解除当前平台与当前购买条件的重试保护，请手动重新购买')
  } else if (!disposed) {
    ElMessage.error('浏览器无法保存解除状态，请允许本站使用持久化存储后重试')
  }
}

async function purchase(): Promise<void> {
  if (purchasing.value) return
  purchasing.value = true
  let requestSignature = ''
  let requestStorageKey = ''
  let requestConditions: PurchaseConditionSnapshot | null = null
  try {
    if (!formRef.value || !(await formRef.value.validate().catch(() => false))) return
    if (!form.provider) return

    requestStorageKey = purchaseIntentStorageKey()
    if (!requestStorageKey) {
      ElMessage.error('登录状态尚未就绪，请刷新页面后重试')
      return
    }
    requestConditions = {
      provider: form.provider,
      serviceCode: form.serviceCode,
      tier: form.tier,
      countryCode: form.countryCode,
      maxPrice: form.maxPrice.trim(),
    }
    requestSignature = createPurchaseSignature(requestConditions)
    const requestKey = await getOrCreatePurchaseIntentKey(
      purchaseIntentStore,
      requestStorageKey,
      requestSignature,
      purchaseIntentLockManager,
    )
    await ordersApi.create(
      {
        provider: requestConditions.provider,
        countryCode: requestConditions.countryCode,
        serviceCode: requestConditions.serviceCode,
        ...(requestConditions.tier ? { tier: requestConditions.tier } : {}),
        maxPrice: requestConditions.maxPrice,
      },
      requestKey,
    )
    await clearPurchaseIntent(
      purchaseIntentStore,
      requestStorageKey,
      requestSignature,
      purchaseIntentLockManager,
    )
    if (disposed) return
    ElMessage.success('号码购买成功，已开始持续接收验证码')
    formRef.value?.clearValidate()
    saveCurrentSelection()
    await Promise.all([ordersRef.value?.revealLatest(), loadQuote(), forceLoadProviderBalances()])
  } catch (reason) {
    if (reason instanceof PurchaseIntentPersistenceError) {
      if (!disposed) ElMessage.error(reason.message)
      return
    }
    if (reason instanceof PurchaseIntentConflictError) {
      if (!disposed && requestStorageKey && requestSignature && requestConditions) {
        await confirmPurchaseRetryUnlock(
          requestStorageKey,
          requestSignature,
          requestConditions,
          reason.message,
        )
      }
      return
    }
    const code = errorCode(reason)
    if (requestStorageKey && requestSignature && TERMINAL_PURCHASE_FAILURE_CODES.has(code)) {
      await clearPurchaseIntent(
        purchaseIntentStore,
        requestStorageKey,
        requestSignature,
        purchaseIntentLockManager,
      )
    }
    if (disposed) return
    const transportFailureMessage = uncertainTransportFailureMessage(reason, code)
    const failureMessage = transportFailureMessage || errorMessage(reason, '购买失败，请调整条件后重试')
    if (
      requestStorageKey &&
      requestSignature &&
      requestConditions &&
      (UNCERTAIN_PURCHASE_FAILURE_CODES.has(code) || Boolean(transportFailureMessage))
    ) {
      await confirmPurchaseRetryUnlock(requestStorageKey, requestSignature, requestConditions, failureMessage)
    } else {
      ElMessage.error(failureMessage)
    }
  } finally {
    purchasing.value = false
  }
}

onMounted(async () => {
  document.addEventListener('visibilitychange', handleVisibilityChange)
  window.addEventListener('online', handleVisibilityChange)
  await loadProviders()
  if (disposed) return
  await loadProviderBalances()
  if (disposed) return
  balanceRefreshTimer = window.setInterval(() => {
    if (!document.hidden && navigator.onLine) void loadProviderBalances()
  }, 15_000)
})

onBeforeUnmount(() => {
  if (!restoringProviderSelection.value) saveCurrentSelection()
  disposed = true
  if (balanceRefreshTimer !== undefined) window.clearInterval(balanceRefreshTimer)
  balanceAbortController?.abort()
  document.removeEventListener('visibilitychange', handleVisibilityChange)
  window.removeEventListener('online', handleVisibilityChange)
  providerRestoreGeneration += 1
  servicesGeneration += 1
  countriesGeneration += 1
  quoteGeneration += 1
})
</script>

<template>
  <div class="page-stack buy-page">
    <PageHeader title="号码管理" description="选择号码资源并直接购买，下方可管理订单与持续接收验证码。">
      <template #actions>
        <el-button :icon="Refresh" :loading="loadingProviders || loadingBalances" @click="refreshProviders">刷新平台</el-button>
      </template>
    </PageHeader>

    <section>
      <article class="content-card buy-form-card">
        <div class="step-heading">
          <span>01</span>
          <div><h3>设置采购条件</h3><p>展示各供应商状态与实时余额，仅已启用接口可用于采购。</p></div>
        </div>

        <el-form ref="formRef" :model="form" :rules="rules" label-position="top" class="purchase-form">
          <el-form-item label="供应商" prop="provider">
            <div class="provider-selector" v-loading="loadingProviders">
              <button
                v-for="provider in providers"
                :key="provider.code"
                type="button"
                class="provider-option"
                :class="{ selected: form.provider === provider.code, disabled: !providerPurchasable(provider) }"
                :disabled="!providerPurchasable(provider)"
                :aria-disabled="!providerPurchasable(provider)"
                @click="providerPurchasable(provider) && (form.provider = provider.code)"
              >
                <span class="provider-logo" :class="`provider-${provider.code}`">{{ providerName(provider.code).slice(0, 1) }}</span>
                <span>
                  <strong>{{ provider.name || providerName(provider.code) }}</strong>
                  <small :title="providerBalances[provider.code]?.message || providerBalanceText(provider)">{{ providerBalanceText(provider) }}</small>
                </span>
                <el-icon class="option-check"><Check /></el-icon>
              </button>
              <el-empty v-if="!loadingProviders && !providers.length" description="暂无供应商配置" :image-size="60" />
            </div>
          </el-form-item>

          <el-form-item label="接码服务" prop="serviceCode">
            <el-select
              v-model="form.serviceCode"
              filterable
              :loading="loadingServices"
              :disabled="!form.provider || restoringProviderSelection"
              placeholder="请先选择接码服务"
              style="width: 100%"
            >
              <el-option v-for="service in services" :key="service.code" :label="service.name" :value="service.code">
                <span class="select-option-main">{{ service.name }}</span>
                <small v-if="service.available !== undefined">{{ service.available }} 个可用</small>
              </el-option>
            </el-select>
          </el-form-item>

          <el-form-item label="国家或地区" prop="countryCode">
            <el-select
              v-model="form.countryCode"
              filterable
              :loading="loadingCountries"
              :disabled="!form.serviceCode || restoringProviderSelection"
              placeholder="请先选择服务，再选择国家或地区"
              style="width: 100%"
            >
              <el-option v-for="country in countries" :key="country.code" :label="country.name" :value="country.code">
                <span class="select-option-main">{{ country.flag }} {{ country.name }}</span>
                <small v-if="country.available !== undefined">{{ country.available }} 个可用</small>
              </el-option>
            </el-select>
          </el-form-item>

          <el-form-item label="选择价格" prop="priceSelection">
            <el-select
              :model-value="form.priceSelection"
              :loading="loadingQuote"
              :disabled="!priceOptions.length || loadingQuote"
              :placeholder="loadingQuote ? '正在刷新价格' : priceOptions.length ? '请选择最新价格' : '选择国家后加载价格'"
              style="width: 100%"
              @change="selectPrice"
            >
              <el-option
                v-for="priceQuote in priceOptions"
                :key="priceQuote.key"
                :label="priceOptionLabel(priceQuote)"
                :value="priceQuote.key"
              >
                <span class="select-option-main">{{ priceOptionLabel(priceQuote) }}</span>
                <small>{{ priceQuote.available }} 个可用</small>
              </el-option>
            </el-select>
            <p v-if="form.provider === 'smsbower'" class="form-help">
              Bronze、Silver、Gold 等级已包含在价格选项中，可用数量随价格档位变化。
            </p>
            <p v-else class="form-help">请选择要购买的价格，可用数量随价格档位变化。</p>
          </el-form-item>

          <div class="purchase-form-actions">
            <p>点击后将按所选条件直接购买，无需再次确认。</p>
            <el-button
              class="purchase-button"
              type="primary"
              size="large"
              :icon="ShoppingCart"
              :loading="purchasing"
              :disabled="!selectedPriceOption || selectedPriceOption.available < 1"
              @click="purchase"
            >
              购买号码
            </el-button>
          </div>
        </el-form>
      </article>
    </section>

    <OrdersView id="orders" ref="ordersRef" embedded />
  </div>
</template>
