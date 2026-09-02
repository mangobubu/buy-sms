<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage } from 'element-plus'
import { Check, Refresh, ShoppingCart } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import OrdersView from '@/views/OrdersView.vue'
import { providersApi } from '@/api/providers'
import { catalogApi } from '@/api/catalog'
import { ordersApi } from '@/api/orders'
import { purchaseAttemptsApi } from '@/api/purchase-attempts'
import { errorCode, errorMessage } from '@/api/http'
import { authSession } from '@/stores/auth'
import type {
  CountryOption,
  PurchaseAttempt,
  ProviderCode,
  ProviderConfig,
  Quote,
  ServiceOption,
  SmsBowerTier,
} from '@/types/api'
import { formatDateTime, formatMoney, providerName } from '@/utils/format'

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
const quote = ref<Quote | null>(null)
const purchaseAttempts = ref<PurchaseAttempt[]>([])
const loadingPurchaseAttempts = ref(false)
const refreshingPurchaseAttempts = ref(false)
const purchaseAttemptsError = ref('')
const tierOptions: { value: SmsBowerTier; label: string; caption: string }[] = [
  { value: 'bronze', label: 'Bronze', caption: '青铜级' },
  { value: 'silver', label: 'Silver', caption: '白银级' },
  { value: 'gold', label: 'Gold', caption: '黄金级' },
]

let servicesGeneration = 0
let countriesGeneration = 0
let quoteGeneration = 0
let purchaseAttemptsPollTimer: number | undefined
let purchaseAttemptsLoadPromise: Promise<void> | null = null
let purchaseAttemptsPendingReload: { silent: boolean } | null = null
let purchaseAttemptsActive = false

const form = reactive({
  provider: '' as ProviderCode | '',
  serviceCode: '',
  tier: '' as SmsBowerTier | '',
  countryCode: '',
  maxPrice: '',
})

const rules: FormRules = {
  provider: [{ required: true, message: '请选择供应商', trigger: 'change' }],
  serviceCode: [{ required: true, message: '请选择接码服务', trigger: 'change' }],
  tier: [
    {
      validator: (_rule, value, callback) =>
        form.provider !== 'smsbower' || value ? callback() : callback(new Error('请选择号码等级')),
      trigger: 'change',
    },
  ],
  countryCode: [{ required: true, message: '请选择国家或地区', trigger: 'change' }],
  maxPrice: [{ required: true, message: '请选择价格', trigger: 'change' }],
}

const enabledProviders = computed(() => providers.value.filter((item) => item.enabled))
const isSmsBower = computed(() => form.provider === 'smsbower')
const priceOptions = computed(() => {
  if (!quote.value) return []
  const options = quote.value.priceOptions?.length
    ? quote.value.priceOptions
    : [{ price: quote.value.price, available: quote.value.available }]
  return options.map((option) => ({ ...option, currency: quote.value?.currency || 'USD' }))
})
const selectedPriceOption = computed(() =>
  priceOptions.value.find((option) => option.price === form.maxPrice),
)
const hasPendingPurchaseAttempts = computed(() =>
  purchaseAttempts.value.some((attempt) => attempt.status.toLowerCase() === 'provisioning'),
)

function purchaseAttemptStatusLabel(status: string): string {
  switch (status.toLowerCase()) {
    case 'provisioning': return '处理中'
    case 'succeeded': return '购买成功'
    case 'failed': return '购买失败'
    case 'unknown': return '结果待确认'
    default: return '状态更新中'
  }
}

function purchaseAttemptStatusTone(status: string): 'primary' | 'success' | 'warning' | 'danger' | 'info' {
  switch (status.toLowerCase()) {
    case 'provisioning': return 'primary'
    case 'succeeded': return 'success'
    case 'failed': return 'danger'
    case 'unknown': return 'warning'
    default: return 'info'
  }
}

function purchaseAttemptTierLabel(tier?: SmsBowerTier): string {
  return tier ? tier.charAt(0).toUpperCase() + tier.slice(1) : '—'
}

function stopPurchaseAttemptsPolling(): void {
  if (purchaseAttemptsPollTimer) window.clearInterval(purchaseAttemptsPollTimer)
  purchaseAttemptsPollTimer = undefined
}

function syncPurchaseAttemptsPolling(): void {
  if (!purchaseAttemptsActive || document.visibilityState !== 'visible' || !hasPendingPurchaseAttempts.value) {
    stopPurchaseAttemptsPolling()
    return
  }
  if (purchaseAttemptsPollTimer) return
  purchaseAttemptsPollTimer = window.setInterval(() => {
    void loadPurchaseAttempts({ silent: true })
  }, 10_000)
}

async function performPurchaseAttemptsLoad(initialIntent: { silent: boolean }): Promise<void> {
  let intent: { silent: boolean } | null = initialIntent
  try {
    while (intent) {
      const currentIntent: { silent: boolean } = intent
      purchaseAttemptsPendingReload = null
      const blocking = !currentIntent.silent && purchaseAttempts.value.length === 0
      if (blocking) loadingPurchaseAttempts.value = true
      else refreshingPurchaseAttempts.value = true
      try {
        purchaseAttempts.value = (await purchaseAttemptsApi.list()).slice(0, 20)
        purchaseAttemptsError.value = ''
      } catch (reason) {
        purchaseAttemptsError.value = errorMessage(reason, '最近购买记录加载失败')
      }
      intent = purchaseAttemptsPendingReload
    }
  } finally {
    loadingPurchaseAttempts.value = false
    refreshingPurchaseAttempts.value = false
    purchaseAttemptsLoadPromise = null
    purchaseAttemptsPendingReload = null
    syncPurchaseAttemptsPolling()
  }
}

function loadPurchaseAttempts(options: { silent?: boolean } = {}): Promise<void> {
  const intent = { silent: options.silent === true }
  if (purchaseAttemptsLoadPromise) {
    purchaseAttemptsPendingReload = {
      silent: (purchaseAttemptsPendingReload?.silent ?? true) && intent.silent,
    }
    return purchaseAttemptsLoadPromise
  }
  purchaseAttemptsLoadPromise = performPurchaseAttemptsLoad(intent)
  return purchaseAttemptsLoadPromise
}

function onPurchaseAttemptsVisibilityChange(): void {
  if (document.visibilityState !== 'visible') {
    stopPurchaseAttemptsPolling()
    return
  }
  if (hasPendingPurchaseAttempts.value) void loadPurchaseAttempts({ silent: true })
  else syncPurchaseAttemptsPolling()
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

async function loadServices(provider: ProviderCode): Promise<void> {
  const generation = ++servicesGeneration
  loadingServices.value = true
  try {
    const result = await catalogApi.services(provider)
    if (generation !== servicesGeneration || form.provider !== provider) return
    services.value = result
  } catch (reason) {
    if (generation === servicesGeneration) ElMessage.error(errorMessage(reason, '服务列表加载失败'))
  } finally {
    if (generation === servicesGeneration) loadingServices.value = false
  }
}

async function loadCountries(): Promise<void> {
  if (!form.provider || !form.serviceCode || (isSmsBower.value && !form.tier)) return
  const provider = form.provider
  const service = form.serviceCode
  const tier = form.tier || undefined
  const generation = ++countriesGeneration
  loadingCountries.value = true
  try {
    const result = await catalogApi.countries(provider, service, tier)
    if (
      generation !== countriesGeneration ||
      form.provider !== provider ||
      form.serviceCode !== service ||
      (form.tier || undefined) !== tier
    ) return
    countries.value = result
  } catch (reason) {
    if (generation === countriesGeneration) ElMessage.error(errorMessage(reason, '国家列表加载失败'))
  } finally {
    if (generation === countriesGeneration) loadingCountries.value = false
  }
}

async function loadQuote(): Promise<void> {
  if (!form.provider || !form.countryCode || !form.serviceCode) return
  const provider = form.provider
  const country = form.countryCode
  const service = form.serviceCode
  const tier = form.tier || undefined
  const generation = ++quoteGeneration
  loadingQuote.value = true
  quote.value = null
  try {
    const result = await catalogApi.quote(provider, country, service, tier)
    if (
      generation !== quoteGeneration ||
      form.provider !== provider ||
      form.countryCode !== country ||
      form.serviceCode !== service ||
      (form.tier || undefined) !== tier
    ) return
    quote.value = result
    form.maxPrice = result.priceOptions?.[0]?.price ?? result.price
  } catch (reason) {
    if (generation === quoteGeneration) ElMessage.error(errorMessage(reason, '报价加载失败'))
  } finally {
    if (generation === quoteGeneration) loadingQuote.value = false
  }
}

watch(
  () => form.provider,
  (provider) => {
    purchaseIdempotencyKey.value = ''
    servicesGeneration += 1
    countriesGeneration += 1
    quoteGeneration += 1
    loadingServices.value = false
    loadingCountries.value = false
    loadingQuote.value = false
    form.serviceCode = ''
    form.tier = ''
    form.countryCode = ''
    form.maxPrice = ''
    services.value = []
    countries.value = []
    quote.value = null
    if (provider) void loadServices(provider)
  },
)

watch(
  () => form.serviceCode,
  (service) => {
    purchaseIdempotencyKey.value = ''
    countriesGeneration += 1
    quoteGeneration += 1
    loadingCountries.value = false
    loadingQuote.value = false
    const nextTier: SmsBowerTier | '' = isSmsBower.value && service ? 'gold' : ''
    const tierChanged = form.tier !== nextTier
    form.tier = nextTier
    form.countryCode = ''
    form.maxPrice = ''
    countries.value = []
    quote.value = null
    if (service && !tierChanged) void loadCountries()
  },
)

watch(
  () => form.tier,
  (tier) => {
    purchaseIdempotencyKey.value = ''
    countriesGeneration += 1
    quoteGeneration += 1
    loadingCountries.value = false
    loadingQuote.value = false
    form.countryCode = ''
    form.maxPrice = ''
    countries.value = []
    quote.value = null
    if (form.serviceCode && (!isSmsBower.value || tier)) void loadCountries()
  },
)

watch(
  () => form.countryCode,
  (country) => {
    purchaseIdempotencyKey.value = ''
    quoteGeneration += 1
    loadingQuote.value = false
    form.maxPrice = ''
    quote.value = null
    if (country) void loadQuote()
  },
)

watch(
  () => form.maxPrice,
  () => {
    purchaseIdempotencyKey.value = ''
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
    form.serviceCode = ''
    form.tier = ''
    form.countryCode = ''
    form.maxPrice = ''
    quote.value = null
    formRef.value?.clearValidate()
    await Promise.all([ordersRef.value?.revealLatest(), loadPurchaseAttempts({ silent: true })])
  } catch (reason) {
    if (requestStorageKey && requestSignature && TERMINAL_PURCHASE_FAILURE_CODES.has(errorCode(reason))) {
      clearPersistedPurchase(requestStorageKey, requestSignature)
    }
    ElMessage.error(errorMessage(reason, '购买失败，请调整条件后重试'))
    await loadPurchaseAttempts({ silent: true })
  } finally {
    purchasing.value = false
  }
}

onMounted(() => {
  purchaseAttemptsActive = true
  void loadProviders()
  void loadPurchaseAttempts()
  document.addEventListener('visibilitychange', onPurchaseAttemptsVisibilityChange)
})

onBeforeUnmount(() => {
  purchaseAttemptsActive = false
  stopPurchaseAttemptsPolling()
  document.removeEventListener('visibilitychange', onPurchaseAttemptsVisibilityChange)
})
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
              :disabled="!form.provider"
              placeholder="请先选择接码服务"
              style="width: 100%"
            >
              <el-option v-for="service in services" :key="service.code" :label="service.name" :value="service.code">
                <span class="select-option-main">{{ service.name }}</span>
                <small v-if="service.available !== undefined">{{ service.available }} 个可用</small>
              </el-option>
            </el-select>
          </el-form-item>

          <el-form-item v-if="isSmsBower" label="号码等级" prop="tier">
            <div class="tier-selector">
              <button
                v-for="tier in tierOptions"
                :key="tier.value"
                type="button"
                class="tier-option"
                :class="[`tier-${tier.value}`, { selected: form.tier === tier.value }]"
                :disabled="!form.serviceCode"
                @click="form.tier = tier.value"
              >
                <span class="tier-dot" />
                <span><strong>{{ tier.label }}</strong><small>{{ tier.caption }}</small></span>
                <el-icon class="option-check"><Check /></el-icon>
              </button>
            </div>
            <p class="form-help">Gold 为默认等级；切换等级后会重新加载对应国家与报价。</p>
          </el-form-item>

          <el-form-item label="国家或地区" prop="countryCode">
            <el-select
              v-model="form.countryCode"
              filterable
              :loading="loadingCountries"
              :disabled="!form.serviceCode || (isSmsBower && !form.tier)"
              placeholder="请先选择服务，再选择国家或地区"
              style="width: 100%"
            >
              <el-option v-for="country in countries" :key="country.code" :label="country.name" :value="country.code">
                <span class="select-option-main">{{ country.flag }} {{ country.name }}</span>
                <small v-if="country.available !== undefined">{{ country.available }} 个可用</small>
              </el-option>
            </el-select>
          </el-form-item>

          <el-form-item label="选择价格" prop="maxPrice">
            <el-select
              v-model="form.maxPrice"
              :loading="loadingQuote"
              :disabled="!quote"
              placeholder="选择国家后加载价格"
              style="width: 100%"
            >
              <el-option
                v-for="priceQuote in priceOptions"
                :key="priceQuote.price"
                :label="formatMoney(priceQuote.price, priceQuote.currency)"
                :value="priceQuote.price"
              >
                <span class="select-option-main">{{ formatMoney(priceQuote.price, priceQuote.currency) }}</span>
                <small>{{ priceQuote.available }} 个可用</small>
              </el-option>
            </el-select>
            <p class="form-help">请选择要购买的价格，可用数量随价格档位变化。</p>
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

    <section class="content-card purchase-attempts-card" aria-labelledby="purchase-attempts-title">
      <header class="purchase-attempts-heading">
        <div>
          <h2 id="purchase-attempts-title">最近购买尝试</h2>
          <p>直接查看购买处理状态和失败原因，无需手动查询数据库。</p>
        </div>
        <el-button
          :icon="Refresh"
          :loading="refreshingPurchaseAttempts"
          @click="loadPurchaseAttempts({ silent: true })"
        >
          刷新状态
        </el-button>
      </header>

      <el-alert
        v-if="purchaseAttemptsError"
        class="purchase-attempts-error"
        type="error"
        :title="purchaseAttemptsError"
        :closable="false"
        show-icon
      />

      <div v-loading="loadingPurchaseAttempts" class="purchase-attempts-body">
        <el-empty
          v-if="!loadingPurchaseAttempts && !purchaseAttempts.length"
          description="暂无购买尝试"
          :image-size="54"
        />
        <div v-else class="purchase-attempts-list">
          <article
            v-for="(attempt, index) in purchaseAttempts"
            :key="`${attempt.createdAt}-${attempt.provider}-${attempt.serviceCode}-${index}`"
            class="purchase-attempt-item"
          >
            <div class="purchase-attempt-main">
              <div class="purchase-attempt-title">
                <strong>{{ providerName(attempt.provider) }}</strong>
                <el-tag size="small" effect="light" :type="purchaseAttemptStatusTone(attempt.status)">
                  {{ purchaseAttemptStatusLabel(attempt.status) }}
                </el-tag>
              </div>
              <p>{{ attempt.message }}</p>
            </div>
            <dl class="purchase-attempt-meta">
              <div><dt>服务</dt><dd>{{ attempt.serviceName || '服务代码：' + attempt.serviceCode }}</dd></div>
              <div><dt>国家</dt><dd>{{ attempt.countryName || '国家代码：' + attempt.countryCode }}</dd></div>
              <div v-if="attempt.tier"><dt>等级</dt><dd>{{ purchaseAttemptTierLabel(attempt.tier) }}</dd></div>
              <div><dt>价格</dt><dd>{{ formatMoney(attempt.maxPrice, 'USD') }}</dd></div>
              <div><dt>请求时间</dt><dd>{{ formatDateTime(attempt.createdAt) }}</dd></div>
            </dl>
          </article>
        </div>
      </div>
    </section>

    <OrdersView id="orders" ref="ordersRef" embedded />
  </div>
</template>
