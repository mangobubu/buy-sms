<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowRight, Check, Connection, Money, Refresh, ShoppingCart } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import { providersApi } from '@/api/providers'
import { catalogApi } from '@/api/catalog'
import { ordersApi } from '@/api/orders'
import { errorMessage } from '@/api/http'
import type { CountryOption, ProviderCode, ProviderConfig, Quote, ServiceOption } from '@/types/api'
import { formatMoney, providerName } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const formRef = ref<FormInstance>()
const loadingProviders = ref(false)
const loadingCountries = ref(false)
const loadingServices = ref(false)
const loadingQuote = ref(false)
const purchasing = ref(false)
const purchaseIdempotencyKey = ref('')
const PURCHASE_INTENT_KEY = 'buy_sms_purchase_intent'
const providers = ref<ProviderConfig[]>([])
const countries = ref<CountryOption[]>([])
const services = ref<ServiceOption[]>([])
const quote = ref<Quote | null>(null)

const form = reactive({
  provider: '' as ProviderCode | '',
  countryCode: '',
  serviceCode: '',
  maxPrice: '',
})

const rules: FormRules = {
  provider: [{ required: true, message: '请选择供应商', trigger: 'change' }],
  countryCode: [{ required: true, message: '请选择国家或地区', trigger: 'change' }],
  serviceCode: [{ required: true, message: '请选择接码服务', trigger: 'change' }],
  maxPrice: [
    { required: true, message: '请输入最高可接受价格', trigger: 'blur' },
    {
      validator: (_rule, value, callback) =>
        Number(value) > 0 ? callback() : callback(new Error('最高价格必须大于 0')),
      trigger: 'blur',
    },
  ],
}

const enabledProviders = computed(() => providers.value.filter((item) => item.enabled))
const selectedCountry = computed(() => countries.value.find((item) => item.code === form.countryCode))
const selectedService = computed(() => services.value.find((item) => item.code === form.serviceCode))

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

async function loadCountries(provider: ProviderCode): Promise<void> {
  loadingCountries.value = true
  try {
    countries.value = await catalogApi.countries(provider)
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '国家列表加载失败'))
  } finally {
    loadingCountries.value = false
  }
}

async function loadServices(): Promise<void> {
  if (!form.provider || !form.countryCode) return
  loadingServices.value = true
  try {
    services.value = await catalogApi.services(form.provider, form.countryCode)
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '服务列表加载失败'))
  } finally {
    loadingServices.value = false
  }
}

async function loadQuote(): Promise<void> {
  if (!form.provider || !form.countryCode || !form.serviceCode) return
  loadingQuote.value = true
  quote.value = null
  try {
    quote.value = await catalogApi.quote(form.provider, form.countryCode, form.serviceCode)
    if (!form.maxPrice || Number(form.maxPrice) < Number(quote.value.price)) {
      form.maxPrice = quote.value.price
    }
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '报价加载失败'))
  } finally {
    loadingQuote.value = false
  }
}

watch(
  () => form.provider,
  (provider) => {
	purchaseIdempotencyKey.value = ''
    form.countryCode = ''
    form.serviceCode = ''
    form.maxPrice = ''
    countries.value = []
    services.value = []
    quote.value = null
    if (provider) void loadCountries(provider)
  },
)

watch(
  () => form.countryCode,
  () => {
	purchaseIdempotencyKey.value = ''
    form.serviceCode = ''
    form.maxPrice = ''
    services.value = []
    quote.value = null
    void loadServices()
  },
)

watch(
  () => form.serviceCode,
  () => {
	purchaseIdempotencyKey.value = ''
    quote.value = null
    void loadQuote()
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
    countryCode: form.countryCode,
    serviceCode: form.serviceCode,
    maxPrice: form.maxPrice.trim(),
  })
}

function persistedIdempotencyKey(): string {
  const signature = purchaseSignature()
  try {
    const saved = JSON.parse(sessionStorage.getItem(PURCHASE_INTENT_KEY) || '{}') as { signature?: string; key?: string }
    if (saved.signature === signature && saved.key) return saved.key
  } catch {
    sessionStorage.removeItem(PURCHASE_INTENT_KEY)
  }
  const key = newIdempotencyKey()
  sessionStorage.setItem(PURCHASE_INTENT_KEY, JSON.stringify({ signature, key }))
  return key
}

function clearPersistedPurchase(): void {
  purchaseIdempotencyKey.value = ''
  sessionStorage.removeItem(PURCHASE_INTENT_KEY)
}

async function purchase(): Promise<void> {
  if (!formRef.value || !(await formRef.value.validate().catch(() => false))) return
  if (!form.provider) return

  try {
    await ElMessageBox.confirm(
      `将从 ${providerName(form.provider)} 购买 ${selectedCountry.value?.name || form.countryCode} 的 ${selectedService.value?.name || form.serviceCode} 号码，最高价格 ${formatMoney(form.maxPrice, quote.value?.currency || 'USD')}。`,
      '确认购买号码',
      { confirmButtonText: '确认购买', cancelButtonText: '再检查一下', type: 'info' },
    )
  } catch {
    return
  }

  purchasing.value = true
  try {
	if (!purchaseIdempotencyKey.value) purchaseIdempotencyKey.value = persistedIdempotencyKey()
    await ordersApi.create(
      {
        provider: form.provider,
        countryCode: form.countryCode,
        serviceCode: form.serviceCode,
        maxPrice: form.maxPrice,
      },
      purchaseIdempotencyKey.value,
    )
	clearPersistedPurchase()
    ElMessage.success('号码购买成功，已开始持续接收验证码')
    await router.push(`/${String(route.params.adminSlug)}/orders`)
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '购买失败，请调整条件后重试'))
  } finally {
    purchasing.value = false
  }
}

onMounted(loadProviders)
</script>

<template>
  <div class="page-stack buy-page">
    <PageHeader title="购买号码" description="选择供应商、国家与服务，系统会在最高价格限制内完成采购。">
      <template #actions>
        <el-button :icon="Refresh" :loading="loadingProviders" @click="loadProviders">刷新平台</el-button>
      </template>
    </PageHeader>

    <section class="buy-layout">
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

          <div class="form-grid two-cols">
            <el-form-item label="国家或地区" prop="countryCode">
              <el-select
                v-model="form.countryCode"
                filterable
                :loading="loadingCountries"
                :disabled="!form.provider"
                placeholder="请选择国家或地区"
                style="width: 100%"
              >
                <el-option v-for="country in countries" :key="country.code" :label="country.name" :value="country.code">
                  <span class="select-option-main">{{ country.flag }} {{ country.name }}</span>
                  <small v-if="country.available !== undefined">{{ country.available }} 个可用</small>
                </el-option>
              </el-select>
            </el-form-item>

            <el-form-item label="接码服务" prop="serviceCode">
              <el-select
                v-model="form.serviceCode"
                filterable
                :loading="loadingServices"
                :disabled="!form.countryCode"
                placeholder="请选择服务"
                style="width: 100%"
              >
                <el-option v-for="service in services" :key="service.code" :label="service.name" :value="service.code">
                  <span class="select-option-main">{{ service.name }}</span>
                  <small v-if="service.available !== undefined">{{ service.available }} 个可用</small>
                </el-option>
              </el-select>
            </el-form-item>
          </div>

          <el-form-item label="最高可接受价格" prop="maxPrice">
            <el-input v-model="form.maxPrice" inputmode="decimal" placeholder="0.00" :prefix-icon="Money">
              <template #append>{{ quote?.currency || 'USD' }}</template>
            </el-input>
            <p class="form-help">实际采购价不会超过此金额；报价变化时以该上限为准。</p>
          </el-form-item>
        </el-form>
      </article>

      <aside class="content-card purchase-summary">
        <div class="step-heading compact">
          <span>02</span>
          <div><h3>确认采购信息</h3><p>购买前请确认以下条件</p></div>
        </div>

        <div class="summary-visual">
          <span class="summary-icon"><ShoppingCart /></span>
          <strong>{{ selectedService?.name || '等待选择服务' }}</strong>
          <small>{{ selectedCountry?.name || '请先选择国家或地区' }}</small>
        </div>

        <dl class="summary-list" v-loading="loadingQuote">
          <div><dt>供应商</dt><dd>{{ form.provider ? providerName(form.provider) : '—' }}</dd></div>
          <div><dt>国家 / 地区</dt><dd>{{ selectedCountry?.name || '—' }}</dd></div>
          <div><dt>服务</dt><dd>{{ selectedService?.name || '—' }}</dd></div>
          <div><dt>当前可用</dt><dd>{{ quote ? `${quote.available} 个号码` : '—' }}</dd></div>
          <div class="summary-price"><dt>参考单价</dt><dd>{{ quote ? formatMoney(quote.price, quote.currency) : '—' }}</dd></div>
        </dl>

        <div class="receive-notice">
          <el-icon><Connection /></el-icon>
          <p><strong>持续收码</strong><span>购买后将持续接收全部验证码，直到您主动完成或取消订单。</span></p>
        </div>

        <el-button
          class="purchase-button"
          type="primary"
          size="large"
          :loading="purchasing"
          :disabled="!quote || quote.available < 1"
          @click="purchase"
        >
          确认购买 <el-icon><ArrowRight /></el-icon>
        </el-button>
      </aside>
    </section>
  </div>
</template>
