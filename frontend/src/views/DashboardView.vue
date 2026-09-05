<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowRight, Cellphone, Connection, Message, Money, Refresh } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import OrderCountdown from '@/components/OrderCountdown.vue'
import OrderStatusTag from '@/components/OrderStatusTag.vue'
import EmptyState from '@/components/EmptyState.vue'
import { dashboardApi } from '@/api/dashboard'
import { errorMessage } from '@/api/http'
import type { DashboardOverview } from '@/types/api'
import { formatDateTime, formatMoney, formatPhoneNumber, formatPurchaseDuration, providerName, smsBowerTierLabel } from '@/utils/format'
import { useSecondNow } from '@/composables/useSecondNow'

const route = useRoute()
const router = useRouter()
const loading = ref(false)
const error = ref('')
const overview = ref<DashboardOverview | null>(null)
const countdownNow = useSecondNow()
let pollTimer: number | undefined
let loadInFlight = false

const adminSlug = computed(() => String(route.params.adminSlug || ''))
const greeting = computed(() => {
  const hour = new Date().getHours()
  if (hour < 6) return '夜深了'
  if (hour < 12) return '早上好'
  if (hour < 18) return '下午好'
  return '晚上好'
})
const stats = computed(() => {
  const data = overview.value
  return [
    {
      label: '进行中号码',
      value: data?.activeOrders ?? 0,
      suffix: '个',
      hint: '持续监听验证码',
      icon: Cellphone,
      tone: 'blue',
    },
    {
      label: '今日订单',
      value: data?.todayOrders ?? 0,
      suffix: '笔',
      hint: '所有供应商合计',
      icon: Connection,
      tone: 'violet',
    },
    {
      label: '今日验证码',
      value: data?.todayMessages ?? 0,
      suffix: '条',
      hint: '包含重复验证码',
      icon: Message,
      tone: 'green',
    },
    {
      label: '今日支出',
      value: formatMoney(data?.todaySpend ?? '0', data?.currency || 'USD'),
      suffix: '',
      hint: '仅统计已完成订单',
      icon: Money,
      tone: 'amber',
    },
  ]
})

async function load(options: { silent?: boolean } = {}): Promise<void> {
  if (loadInFlight) return
  const silent = options.silent === true
  loadInFlight = true
  if (!silent) loading.value = true
  try {
    overview.value = await dashboardApi.overview()
    error.value = ''
  } catch (reason) {
    if (!silent) error.value = errorMessage(reason, '仪表盘数据加载失败')
  } finally {
    if (!silent) loading.value = false
    loadInFlight = false
  }
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

function go(path: string): void {
  void router.push(`/${adminSlug.value}${path}`)
}

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
  <div class="page-stack">
    <PageHeader :title="`${greeting}，欢迎回来`" description="这里是当前号码与验证码接收任务的实时概览。">
      <template #actions>
        <el-button :icon="Refresh" :loading="loading" @click="load()">刷新</el-button>
        <el-button type="primary" :icon="Cellphone" @click="go('/buy')">购买号码</el-button>
      </template>
    </PageHeader>

    <el-alert v-if="error" :title="error" type="error" show-icon :closable="false">
      <template #default><el-button link type="primary" @click="load()">重新加载</el-button></template>
    </el-alert>

    <section class="stats-grid" v-loading="loading && !overview">
      <article v-for="stat in stats" :key="stat.label" class="stat-card">
        <div class="stat-icon" :class="`tone-${stat.tone}`"><el-icon><component :is="stat.icon" /></el-icon></div>
        <div class="stat-copy">
          <span>{{ stat.label }}</span>
          <div><strong>{{ stat.value }}</strong><small>{{ stat.suffix }}</small></div>
          <p>{{ stat.hint }}</p>
        </div>
      </article>
    </section>

    <section class="dashboard-grid">
      <article class="content-card recent-card">
        <header class="card-header">
          <div>
            <h3>最近订单</h3>
            <p>号码分配和验证码接收进度</p>
          </div>
          <el-button link type="primary" @click="go('/buy#orders')">查看全部 <el-icon><ArrowRight /></el-icon></el-button>
        </header>

        <div v-if="overview?.recentOrders?.length" class="recent-orders">
          <button
            v-for="order in overview.recentOrders"
            :key="order.id"
            type="button"
            class="recent-order"
            @click="go('/buy#orders')"
          >
            <span class="provider-logo" :class="`provider-${order.provider}`">{{ providerName(order.provider).slice(0, 1) }}</span>
            <span class="recent-order-main">
              <strong>{{ formatPhoneNumber(order.phoneNumber) }}</strong>
              <small>{{ order.providerName || providerName(order.provider) }} · {{ order.serviceName || '服务代码：' + order.serviceCode }}</small>
              <small v-if="order.provider === 'smsbower' && smsBowerTierLabel(order.tier)">等级：{{ smsBowerTierLabel(order.tier) }}</small>
              <small v-if="order.provider === 'herosms'">时长：{{ formatPurchaseDuration(order.duration) }}</small>
            </span>
            <span class="recent-order-timing">
              <OrderCountdown :status="order.status" :expires-at="order.expiresAt" :now="countdownNow" />
              <time>{{ formatDateTime(order.updatedAt || order.createdAt) }}</time>
            </span>
            <span class="message-count">{{ order.messages?.length || 0 }} 条短信</span>
            <OrderStatusTag :status="order.status" />
            <el-icon class="recent-arrow"><ArrowRight /></el-icon>
          </button>
        </div>
        <EmptyState v-else description="暂无订单，购买一个号码后会显示在这里">
          <el-button type="primary" @click="go('/buy')">购买号码</el-button>
        </EmptyState>
      </article>

      <article class="content-card provider-health-card">
        <header class="card-header">
          <div>
            <h3>供应商状态</h3>
            <p>连接与余额信息</p>
          </div>
          <span class="health-summary">
            {{ overview?.providerHealthy ?? 0 }}/{{ overview?.providerTotal ?? 3 }} 正常
          </span>
        </header>

        <div v-if="overview?.providers?.length" class="health-list">
          <div v-for="provider in overview.providers" :key="provider.code" class="health-item">
            <span class="provider-logo" :class="`provider-${provider.code}`">{{ providerName(provider.code).slice(0, 1) }}</span>
            <span class="health-copy">
              <strong>{{ provider.name || providerName(provider.code) }}</strong>
              <small>{{ provider.message || (provider.enabled ? '接口连接正常' : '当前未启用') }}</small>
            </span>
            <span v-if="provider.balance" class="health-balance">
              <strong>{{ formatMoney(provider.balance, provider.currency || 'USD') }}</strong>
              <small>可用余额</small>
            </span>
            <span class="connection-dot" :class="{ healthy: provider.healthy, disabled: !provider.enabled }" />
          </div>
        </div>
        <EmptyState v-else description="尚未配置供应商连接" />

        <button type="button" class="card-footer-link" @click="go('/providers')">
          管理供应商配置 <el-icon><ArrowRight /></el-icon>
        </button>
      </article>
    </section>

    <section class="quick-actions">
      <div class="quick-actions-copy">
        <span class="eyebrow"><i /> 快速开始</span>
        <h3>准备接收下一条验证码？</h3>
        <p>选择供应商、国家与服务，几秒钟内获取一个可用号码。</p>
      </div>
      <el-button size="large" type="primary" @click="go('/buy')">立即购买号码 <el-icon><ArrowRight /></el-icon></el-button>
    </section>
  </div>
</template>

