<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Cellphone, CircleCheck, Close, Connection, Refresh, Search } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import CopyButton from '@/components/CopyButton.vue'
import EmptyState from '@/components/EmptyState.vue'
import OrderStatusTag from '@/components/OrderStatusTag.vue'
import { ordersApi } from '@/api/orders'
import { errorMessage } from '@/api/http'
import type { NumberOrder, OrderQuery, PageResult } from '@/types/api'
import { formatDateTime, formatMoney, formatPhoneNumber, providerName } from '@/utils/format'

withDefaults(defineProps<{ embedded?: boolean }>(), {
  embedded: false,
})

const loading = ref(false)
const refreshing = ref(false)
const actionOrderId = ref('')
const orders = ref<NumberOrder[]>([])
const total = ref(0)
const lastRefreshedAt = ref<Date | null>(null)
const expandedOrders = ref<string[]>([])

const query = reactive<OrderQuery>({
  page: 1,
  pageSize: 10,
  status: '',
  provider: '',
  keyword: '',
})

const terminalStatuses = new Set(['settled', 'completed', 'cancelled', 'expired', 'failed'])
const liveCount = computed(
  () => orders.value.filter((order) => !terminalStatuses.has(order.status.toLowerCase())).length,
)

let pollTimer: number | undefined
let requestInFlight = false
let pendingReload: { silent: boolean } | null = null
let activeLoadPromise: Promise<void> | null = null

function normalizeResult(result: PageResult<NumberOrder> | NumberOrder[]): void {
  if (Array.isArray(result)) {
    orders.value = result
    total.value = result.length
  } else {
    orders.value = result.items || []
    total.value = result.total ?? orders.value.length
  }
}

async function drainLoads(initialIntent: { silent: boolean }): Promise<void> {
  requestInFlight = true
  let intent: { silent: boolean } | null = initialIntent
  try {
    while (intent) {
      const currentIntent: { silent: boolean } = intent
      pendingReload = null
      if (currentIntent.silent) refreshing.value = true
      else loading.value = true
      try {
        normalizeResult(await ordersApi.list({ ...query }))
        lastRefreshedAt.value = new Date()
      } catch (reason) {
        if (!currentIntent.silent) ElMessage.error(errorMessage(reason, '订单加载失败'))
      }
      intent = pendingReload
    }
  } finally {
    loading.value = false
    refreshing.value = false
    requestInFlight = false
    pendingReload = null
    activeLoadPromise = null
  }
}

function load(options: { silent?: boolean } = {}): Promise<void> {
  const intent = { silent: options.silent === true }
  if (requestInFlight && activeLoadPromise) {
    pendingReload = { silent: (pendingReload?.silent ?? true) && intent.silent }
    return activeLoadPromise
  }
  activeLoadPromise = drainLoads(intent)
  return activeLoadPromise
}

function search(): void {
  query.page = 1
  void load()
}

function resetFilters(): void {
  query.status = ''
  query.provider = ''
  query.keyword = ''
  query.page = 1
  void load()
}

async function completeOrder(order: NumberOrder): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `完成后将停止号码 ${order.phoneNumber ? formatPhoneNumber(order.phoneNumber) : order.id} 的持续收码，确认已经获取到所需的全部验证码吗？`,
      '完成并结算订单',
      { confirmButtonText: '确认完成', cancelButtonText: '继续收码', type: 'warning' },
    )
  } catch {
    return
  }
  actionOrderId.value = order.id
  try {
    await ordersApi.complete(order.id)
    ElMessage.success('订单已完成结算')
    await load({ silent: true })
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '完成订单失败'))
  } finally {
    actionOrderId.value = ''
  }
}

async function cancelOrder(order: NumberOrder): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `取消后将停止号码 ${order.phoneNumber ? formatPhoneNumber(order.phoneNumber) : order.id} 的持续收码，该操作通常不可撤销。`,
      '取消号码',
      { confirmButtonText: '确认取消', cancelButtonText: '保留号码', type: 'warning' },
    )
  } catch {
    return
  }
  actionOrderId.value = order.id
  try {
    await ordersApi.cancel(order.id)
    ElMessage.success('号码已取消')
    await load({ silent: true })
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '取消号码失败'))
  } finally {
    actionOrderId.value = ''
  }
}

function isLive(order: NumberOrder): boolean {
  return !terminalStatuses.has(order.status.toLowerCase())
}

function toggleMobileOrder(id: string): void {
  expandedOrders.value = expandedOrders.value.includes(id)
    ? expandedOrders.value.filter((item) => item !== id)
    : [...expandedOrders.value, id]
}

function startPolling(): void {
  stopPolling()
  pollTimer = window.setInterval(() => {
    if (document.visibilityState === 'visible') void load({ silent: true })
  }, 5_000)
}

function stopPolling(): void {
  if (pollTimer) window.clearInterval(pollTimer)
  pollTimer = undefined
}

function onVisibilityChange(): void {
  if (document.visibilityState === 'visible') void load({ silent: true })
}

function revealLatest(): Promise<void> {
  query.page = 1
  query.status = ''
  query.provider = ''
  query.keyword = ''
  return load()
}

defineExpose({ refresh: () => load({ silent: true }), revealLatest })

onMounted(() => {
  void load()
  startPolling()
  document.addEventListener('visibilitychange', onVisibilityChange)
})

onBeforeUnmount(() => {
  stopPolling()
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <div class="page-stack orders-page">
    <PageHeader v-if="!embedded" title="号码订单" description="号码在完成或取消前会持续接收验证码，历史短信不会被覆盖。">
      <template #actions>
        <span class="live-indicator" :class="{ idle: liveCount === 0 }">
          <i /> {{ liveCount ? `${liveCount} 个号码持续收码中` : '当前无收码任务' }}
        </span>
        <el-button :icon="Refresh" :loading="refreshing" @click="load({ silent: true })">刷新</el-button>
      </template>
    </PageHeader>

    <div v-else class="orders-section-heading">
      <div>
        <h2>号码订单</h2>
        <p>号码在完成或取消前会持续接收验证码，历史短信不会被覆盖。</p>
      </div>
      <div class="orders-section-actions">
        <span class="live-indicator" :class="{ idle: liveCount === 0 }">
          <i /> {{ liveCount ? `${liveCount} 个号码持续收码中` : '当前无收码任务' }}
        </span>
        <el-button :icon="Refresh" :loading="refreshing" @click="load({ silent: true })">刷新</el-button>
      </div>
    </div>

    <section class="content-card filter-card">
      <el-input
        v-model="query.keyword"
        class="order-search"
        clearable
        placeholder="搜索号码或订单号"
        :prefix-icon="Search"
        @keyup.enter="search"
        @clear="search"
      />
      <el-select v-model="query.status" clearable placeholder="全部状态" @change="search">
        <el-option label="持续收码" value="active" />
        <el-option label="等待号码" value="pending" />
        <el-option label="已完成" value="completed" />
        <el-option label="已取消" value="cancelled" />
        <el-option label="已过期" value="expired" />
      </el-select>
      <el-select v-model="query.provider" clearable placeholder="全部供应商" @change="search">
        <el-option label="HeroSMS" value="herosms" />
        <el-option label="SMSBower" value="smsbower" />
        <el-option label="SMSPool" value="smspool" />
      </el-select>
      <el-button type="primary" :icon="Search" @click="search">查询</el-button>
      <el-button @click="resetFilters">重置</el-button>
      <span v-if="lastRefreshedAt" class="refresh-time">自动刷新 · {{ formatDateTime(lastRefreshedAt.toISOString()) }}</span>
    </section>

    <section class="content-card orders-table-card desktop-orders" v-loading="loading">
      <el-table :data="orders" row-key="id" class="orders-table">
        <el-table-column type="expand" width="46">
          <template #default="scope">
            <div class="order-expand">
              <div class="receive-state" :class="{ finished: !isLive(scope.row) }">
                <span><el-icon><Connection /></el-icon></span>
                <p>
                  <strong>{{ isLive(scope.row) ? '持续接收验证码' : '收码已结束' }}</strong>
                  <small v-if="isLive(scope.row)">
                    {{ scope.row.webhookEnabled ? 'Webhook 实时推送已启用' : '正在通过安全轮询获取新短信' }}，收到第一条后仍会继续监听
                  </small>
                  <small v-else>该订单不会再接收新的验证码</small>
                </p>
              </div>

              <div v-if="scope.row.messages?.length" class="sms-timeline">
                <article v-for="(message, index) in scope.row.messages" :key="message.id" class="sms-item">
                  <span class="sms-sequence">{{ index + 1 }}</span>
                  <div class="sms-body">
                    <div class="sms-code-row">
                      <strong>{{ message.code || '未识别验证码' }}</strong>
                      <CopyButton v-if="message.code" :value="message.code" label="验证码" />
                      <time>{{ formatDateTime(message.receivedAt) }}</time>
                    </div>
                    <p>{{ message.content }}</p>
                    <CopyButton :value="message.content" label="短信内容">复制短信全文</CopyButton>
                  </div>
                </article>
              </div>
              <EmptyState v-else description="尚未收到验证码，系统会继续等待">
                <span v-if="isLive(scope.row)" class="waiting-pulse"><i /> 正在监听新短信</span>
              </EmptyState>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="号码" min-width="190">
          <template #default="scope">
            <div class="phone-cell">
              <span class="phone-icon"><Cellphone /></span>
              <span><strong>{{ formatPhoneNumber(scope.row.phoneNumber) }}</strong><small>{{ scope.row.countryName || '国家代码：' + scope.row.countryCode }}</small></span>
              <CopyButton v-if="scope.row.phoneNumber" :value="scope.row.phoneNumber" label="号码" />
            </div>
          </template>
        </el-table-column>
        <el-table-column label="供应商 / 服务" min-width="160">
          <template #default="scope">
            <div class="stacked-cell"><strong>{{ scope.row.providerName || providerName(scope.row.provider) }}</strong><small>{{ scope.row.serviceName || '服务代码：' + scope.row.serviceCode }}</small></div>
          </template>
        </el-table-column>
        <el-table-column label="验证码" min-width="110">
          <template #default="scope">
            <div class="message-cell" :class="{ empty: !scope.row.messages?.length }">
              <strong>{{ scope.row.messages?.length || 0 }}</strong><span>条</span>
              <i v-if="isLive(scope.row)" />
            </div>
          </template>
        </el-table-column>
        <el-table-column label="金额" min-width="110">
          <template #default="scope"><strong class="money-cell">{{ formatMoney(scope.row.price, scope.row.currency) }}</strong></template>
        </el-table-column>
        <el-table-column label="状态" min-width="105">
          <template #default="scope"><OrderStatusTag :status="scope.row.status" /></template>
        </el-table-column>
        <el-table-column label="创建时间" min-width="145">
          <template #default="scope"><span class="date-cell">{{ formatDateTime(scope.row.createdAt) }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="160" fixed="right">
          <template #default="scope">
            <div v-if="isLive(scope.row)" class="order-actions">
              <el-button link type="success" :loading="actionOrderId === scope.row.id" @click="completeOrder(scope.row)">
                完成
              </el-button>
              <el-button link type="danger" :disabled="Boolean(actionOrderId)" @click="cancelOrder(scope.row)">取消</el-button>
            </div>
            <span v-else class="muted-text">已结束</span>
          </template>
        </el-table-column>
        <template #empty><EmptyState description="没有符合条件的订单" /></template>
      </el-table>
    </section>

    <section class="mobile-orders" v-loading="loading">
      <article v-for="order in orders" :key="order.id" class="mobile-order-card">
        <header @click="toggleMobileOrder(order.id)">
          <span class="phone-icon"><Cellphone /></span>
          <div><strong>{{ formatPhoneNumber(order.phoneNumber) }}</strong><small>{{ order.providerName || providerName(order.provider) }} · {{ order.serviceName || '服务代码：' + order.serviceCode }}</small></div>
          <OrderStatusTag :status="order.status" />
        </header>
        <div class="mobile-order-meta">
          <span>{{ order.countryName || '国家代码：' + order.countryCode }}</span>
          <span>{{ formatMoney(order.price, order.currency) }}</span>
          <span>{{ order.messages?.length || 0 }} 条短信</span>
        </div>
        <div class="mobile-number-actions">
          <CopyButton v-if="order.phoneNumber" :value="order.phoneNumber" label="号码">复制号码</CopyButton>
          <el-button v-if="isLive(order)" link type="success" :icon="CircleCheck" @click="completeOrder(order)">完成</el-button>
          <el-button v-if="isLive(order)" link type="danger" :icon="Close" @click="cancelOrder(order)">取消</el-button>
          <el-button link @click="toggleMobileOrder(order.id)">{{ expandedOrders.includes(order.id) ? '收起短信' : '查看短信' }}</el-button>
        </div>
        <div v-if="expandedOrders.includes(order.id)" class="mobile-sms-list">
          <div class="receive-state" :class="{ finished: !isLive(order) }">
            <span><el-icon><Connection /></el-icon></span>
            <p><strong>{{ isLive(order) ? '持续接收中' : '收码已结束' }}</strong><small>{{ isLive(order) ? '收到验证码后仍会继续监听' : '不会再接收新短信' }}</small></p>
          </div>
          <article v-for="message in order.messages" :key="message.id" class="mobile-sms-item">
            <div><strong>{{ message.code || '未识别验证码' }}</strong><time>{{ formatDateTime(message.receivedAt) }}</time></div>
            <p>{{ message.content }}</p>
            <CopyButton v-if="message.code" :value="message.code" label="验证码" />
            <CopyButton :value="message.content" label="短信内容">复制全文</CopyButton>
          </article>
          <EmptyState v-if="!order.messages?.length" description="尚未收到验证码" />
        </div>
      </article>
      <EmptyState v-if="!orders.length" description="没有符合条件的订单" />
    </section>

    <el-pagination
      v-if="total > query.pageSize"
      v-model:current-page="query.page"
      v-model:page-size="query.pageSize"
      class="orders-pagination"
      background
      layout="total, prev, pager, next"
      :total="total"
      @current-change="load()"
    />
  </div>
</template>
