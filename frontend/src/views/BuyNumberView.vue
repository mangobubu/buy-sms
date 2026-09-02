<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage } from 'element-plus'
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
  ProviderCode,
  ProviderConfig,
  Quote,
  ServiceOption,
  SmsBowerTier,
} from '@/types/api'
import { formatMoney, providerName } from '@/utils/format'

const formRef = ref<FormInstance>()
const ordersRef = ref<InstanceType<typeof OrdersView>>()
const loadingProviders = ref(false)
const loadingCountries = ref(false)
const loadingServices = ref(false)
const loadingQuote = ref(false)
const purchasing = ref(false)
const purchaseIdempotencyKey = ref('')
const PURCHASE_INTENT_KEY = 'buy_sms_purchase_intent'
const TERMINAL_PURCHASE_FAILURE_CODES = new Set([
  'configuration',
  'idempotency_mismatch',
  'insufficient_balance',
  'invalid_selection',
  'no_numbers',
  'provider_disabled',
  'provider_rate_limited',
  'price_exceeded',
  'purchase_failed',
  'purchase_setup_failed',
])
const providers = ref<ProviderConfig[]>([])
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

const enabledProviders = computed(() => providers.value.filter((item) => item.enabled))
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

const providerSelections: Partial<Record<ProviderCode, ProviderSelection>> = {}

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

function saveCurrentSelection(provider = form.provider): void {
  if (provider) providerSelections[provider] = snapshotSelection()
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
  purchaseIdempotencyKey.value = ''
  saveCurrentSelection()
}

async function loadProviders(): Promise<void> {
  loadingProviders.value = true
  try {
    providers.value = await providersApi.list()
    if (!form.provider && enabledProviders.value[0]) form.provider = enabledProviders.value[0].code
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '供应商加载失败'))
  } finally {
    loadingProviders.value = false
  }
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

async function loadQuote(): Promise<Quote[] | null> {
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
    const nextSelection = priceOptions.value.some((option) => option.key === savedSelection)
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
    await loadQuote()
  } finally {
    if (generation === providerRestoreGeneration) {
      restoringProviderSelection.value = false
    }
  }
}

watch(
  () => form.provider,
  (provider) => {
    purchaseIdempotencyKey.value = ''
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
    if (provider) void restoreProviderSelection(provider, providerSelections[provider])
  },
)

watch(
  () => form.serviceCode,
  (service) => {
    if (restoringProviderSelection.value) return
    purchaseIdempotencyKey.value = ''
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
    purchaseIdempotencyKey.value = ''
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

function purchaseSignature(): string {
  return JSON.stringify({
    provider: form.provider,
    serviceCode: form.serviceCode,
    tier: form.tier || undefined,
    countryCode: form.countryCode,
    maxPrice: form.maxPrice.trim(),
  })
}

function purchaseIntentStorageKey(): string {
  const userID = authSession.state.user?.id
  return userID ? `${PURCHASE_INTENT_KEY}:${userID}` : ''
}

interface PurchaseIntentEntry {
  key: string
  updatedAt: number
}

function readPurchaseIntents(storageKey: string): Record<string, PurchaseIntentEntry> {
  if (!storageKey) return {}
  try {
    let raw = sessionStorage.getItem(storageKey)
    if (raw === null) {
      // 当前页面处于已认证路由，旧版全局值归入当前账号后立即删除，
      // 避免后续账号重复继承或终态清理后再次复活旧购买编号。
      raw = sessionStorage.getItem(PURCHASE_INTENT_KEY)
      if (raw !== null) {
        sessionStorage.setItem(storageKey, raw)
        sessionStorage.removeItem(PURCHASE_INTENT_KEY)
      }
    }
    const parsed = JSON.parse(raw ?? '{}') as unknown
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
    const legacy = parsed as { signature?: unknown; key?: unknown }
    if (typeof legacy.signature === 'string' && typeof legacy.key === 'string') {
      return { [legacy.signature]: { key: legacy.key, updatedAt: Date.now() } }
    }
    return Object.fromEntries(
      Object.entries(parsed).filter(
          (entry): entry is [string, PurchaseIntentEntry] =>
            typeof entry[1] === 'object' &&
            entry[1] !== null &&
            typeof (entry[1] as PurchaseIntentEntry).key === 'string' &&
            typeof (entry[1] as PurchaseIntentEntry).updatedAt === 'number',
        ),
    )
  } catch {
    sessionStorage.removeItem(storageKey)
    return {}
  }
}

function writePurchaseIntents(storageKey: string, intents: Record<string, PurchaseIntentEntry>): void {
  if (!storageKey) return
  if (Object.keys(intents).length) sessionStorage.setItem(storageKey, JSON.stringify(intents))
  else sessionStorage.removeItem(storageKey)
}

function persistedIdempotencyKey(storageKey: string, signature: string): string {
  const intents = readPurchaseIntents(storageKey)
  const saved = intents[signature]
  if (saved) {
    saved.updatedAt = Date.now()
    writePurchaseIntents(storageKey, intents)
    return saved.key
  }
  const key = newIdempotencyKey()
  intents[signature] = { key, updatedAt: Date.now() }
  writePurchaseIntents(storageKey, intents)
  return key
}

function clearPersistedPurchase(storageKey: string, signature: string): void {
  purchaseIdempotencyKey.value = ''
  const intents = readPurchaseIntents(storageKey)
  delete intents[signature]
  writePurchaseIntents(storageKey, intents)
}

async function purchase(): Promise<void> {
  if (purchasing.value) return
  purchasing.value = true
  let requestSignature = ''
  let requestStorageKey = ''
  try {
    if (!formRef.value || !(await formRef.value.validate().catch(() => false))) return
    if (!form.provider) return

    requestStorageKey = purchaseIntentStorageKey()
    if (!requestStorageKey) {
      ElMessage.error('登录状态尚未就绪，请刷新页面后重试')
      return
    }
    requestSignature = purchaseSignature()
    const requestKey = purchaseIdempotencyKey.value || persistedIdempotencyKey(requestStorageKey, requestSignature)
    purchaseIdempotencyKey.value = requestKey
    await ordersApi.create(
      {
        provider: form.provider,
        countryCode: form.countryCode,
        serviceCode: form.serviceCode,
        ...(form.tier ? { tier: form.tier } : {}),
        maxPrice: form.maxPrice,
      },
      requestKey,
    )
    clearPersistedPurchase(requestStorageKey, requestSignature)
    ElMessage.success('号码购买成功，已开始持续接收验证码')
    formRef.value?.clearValidate()
    saveCurrentSelection()
    await Promise.all([ordersRef.value?.revealLatest(), loadQuote()])
  } catch (reason) {
    if (requestStorageKey && requestSignature && TERMINAL_PURCHASE_FAILURE_CODES.has(errorCode(reason))) {
      clearPersistedPurchase(requestStorageKey, requestSignature)
    }
    ElMessage.error(errorMessage(reason, '购买失败，请调整条件后重试'))
  } finally {
    purchasing.value = false
  }
}

onMounted(loadProviders)
</script>

<template>
  <div class="page-stack buy-page">
    <PageHeader title="号码管理" description="选择号码资源并直接购买，下方可管理订单与持续接收验证码。">
      <template #actions>
        <el-button :icon="Refresh" :loading="loadingProviders" @click="loadProviders">刷新平台</el-button>
      </template>
    </PageHeader>

    <section>
      <article class="content-card buy-form-card">
        <div class="step-heading">
          <span>01</span>
          <div><h3>设置采购条件</h3><p>仅展示当前已启用的供应商及其可用资源。</p></div>
        </div>

        <el-form ref="formRef" :model="form" :rules="rules" label-position="top" class="purchase-form">
          <el-form-item label="供应商" prop="provider">
            <div class="provider-selector" v-loading="loadingProviders">
              <button
                v-for="provider in enabledProviders"
                :key="provider.code"
                type="button"
                class="provider-option"
                :class="{ selected: form.provider === provider.code }"
                @click="form.provider = provider.code"
              >
                <span class="provider-logo" :class="`provider-${provider.code}`">{{ providerName(provider.code).slice(0, 1) }}</span>
                <span><strong>{{ provider.name || providerName(provider.code) }}</strong><small>接口已启用</small></span>
                <el-icon class="option-check"><Check /></el-icon>
              </button>
              <el-empty v-if="!loadingProviders && !enabledProviders.length" description="暂无已启用供应商" :image-size="60" />
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
              placeholder="选择国家后加载价格"
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
