<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Connection, Key, Lock, Refresh, Setting } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import CopyButton from '@/components/CopyButton.vue'
import EmptyState from '@/components/EmptyState.vue'
import { providersApi } from '@/api/providers'
import { errorMessage } from '@/api/http'
import type { ProviderConfig, UpdateProviderPayload } from '@/types/api'
import { formatDateTime, providerName } from '@/utils/format'

interface ProviderDraft extends UpdateProviderPayload {
  apiKey: string
  webhookToken: string
}

const loading = ref(false)
const savingId = ref('')
const providers = ref<ProviderConfig[]>([])
const drafts = reactive<Record<string, ProviderDraft>>({})

const enabledCount = computed(() => providers.value.filter((provider) => provider.enabled).length)
const providerRows = computed(() =>
  providers.value.map((provider) => ({
    provider,
    draft: drafts[provider.id] || makeDraft(provider),
  })),
)

function makeDraft(provider: ProviderConfig): ProviderDraft {
  return {
    apiBaseUrl: provider.apiBaseUrl || '',
    apiKey: '',
    webhookToken: '',
    enabled: provider.enabled,
    webhookEnabled: provider.webhookEnabled ?? false,
    pollingIntervalSeconds: provider.pollingIntervalSeconds || 10,
  }
}

async function load(): Promise<void> {
  loading.value = true
  try {
    providers.value = await providersApi.list()
    for (const provider of providers.value) drafts[provider.id] = makeDraft(provider)
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '供应商配置加载失败'))
  } finally {
    loading.value = false
  }
}

async function save(provider: ProviderConfig): Promise<void> {
  const draft = drafts[provider.id]
  if (!draft) return
  if (!draft.apiBaseUrl.trim()) {
    ElMessage.warning('请输入 API 地址')
    return
  }
  if (draft.pollingIntervalSeconds < 5 || draft.pollingIntervalSeconds > 600) {
    ElMessage.warning('轮询间隔应在 5 到 600 秒之间')
    return
  }

  const payload: UpdateProviderPayload = {
    apiBaseUrl: draft.apiBaseUrl.trim(),
    enabled: draft.enabled,
    webhookEnabled: draft.webhookEnabled,
    pollingIntervalSeconds: draft.pollingIntervalSeconds,
  }
  if (draft.apiKey.trim()) payload.apiKey = draft.apiKey.trim()
  if (provider.webhookSupported && draft.webhookToken.trim()) payload.webhookToken = draft.webhookToken.trim()

  savingId.value = provider.id
  try {
    const updated = await providersApi.update(provider.id, payload)
    const index = providers.value.findIndex((item) => item.id === provider.id)
    if (index >= 0) providers.value[index] = updated
    drafts[provider.id] = makeDraft(updated)
    ElMessage.success(`${updated.name || providerName(updated.code)} 配置已保存`)
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '保存供应商配置失败'))
  } finally {
    savingId.value = ''
  }
}

function reset(provider: ProviderConfig): void {
  drafts[provider.id] = makeDraft(provider)
}

onMounted(load)
</script>

<template>
  <div class="page-stack providers-page">
    <PageHeader title="供应商配置" description="配置第三方接口凭据。密钥只写入服务端，保存后不会再回显。">
      <template #actions>
        <span class="provider-count"><i /> {{ enabledCount }}/{{ providers.length || 3 }} 已启用</span>
        <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
      </template>
    </PageHeader>

    <el-alert
      title="敏感信息保护"
      description="API Key 与 Webhook Token 仅用于本次提交。留空表示保留现有值，服务端响应不会包含密钥原文。"
      type="info"
      show-icon
      :closable="false"
    />

    <section class="provider-config-grid" v-loading="loading">
      <article v-for="row in providerRows" :key="row.provider.id" class="content-card provider-config-card">
        <header class="provider-card-head">
          <span class="provider-logo large" :class="`provider-${row.provider.code}`">{{ providerName(row.provider.code).slice(0, 1) }}</span>
          <span class="provider-card-title">
            <strong>{{ row.provider.name || providerName(row.provider.code) }}</strong>
            <small>{{ row.provider.webhookSupported ? 'Webhook 实时推送 + 轮询兜底' : '安全轮询接收验证码' }}</small>
          </span>
          <el-switch
            v-model="row.draft.enabled"
            inline-prompt
            active-text="启用"
            inactive-text="停用"
          />
        </header>

        <el-form label-position="top" class="provider-form">
          <el-form-item label="API 地址">
            <el-input v-model="row.draft.apiBaseUrl" :prefix-icon="Connection" placeholder="https://api.example.com" />
          </el-form-item>

          <el-form-item>
            <template #label>
              <span class="secret-label">API Key <el-tag v-if="row.provider.hasApiKey" size="small" type="success" effect="plain">已配置</el-tag></span>
            </template>
            <el-input
              v-model="row.draft.apiKey"
              type="password"
              show-password
              autocomplete="new-password"
              :prefix-icon="Key"
              :placeholder="row.provider.hasApiKey ? '留空以保留现有密钥' : '请输入 API Key'"
            />
          </el-form-item>

          <el-form-item v-if="row.provider.webhookSupported">
            <el-switch v-model="row.draft.webhookEnabled" active-text="已在供应商后台配置 Webhook" inactive-text="仅使用轮询" />
          </el-form-item>

          <el-form-item v-if="row.provider.webhookSupported">
            <template #label>
              <span class="secret-label">Webhook Token <el-tag v-if="row.provider.hasWebhookToken" size="small" type="success" effect="plain">已配置</el-tag></span>
            </template>
            <el-input
              v-model="row.draft.webhookToken"
              type="password"
              show-password
              autocomplete="new-password"
              :prefix-icon="Lock"
              :placeholder="row.provider.hasWebhookToken ? '留空以保留现有 Token' : '请输入 Webhook Token'"
            />
          </el-form-item>

          <el-form-item label="轮询间隔">
            <el-input-number v-model="row.draft.pollingIntervalSeconds" :min="5" :max="600" :step="1" controls-position="right" />
            <span class="input-suffix">秒</span>
            <p class="form-help">Webhook 不可用或推送失败时按此间隔降级轮询。</p>
          </el-form-item>
        </el-form>

        <div v-if="row.provider.webhookSupported && row.provider.webhookUrl" class="webhook-box">
          <span><Setting /></span>
          <div><strong>Webhook 回调地址</strong><code>{{ row.provider.webhookUrl }}</code></div>
          <CopyButton :value="row.provider.webhookUrl" label="Webhook 地址" />
        </div>

        <footer class="provider-card-footer">
          <span>{{ row.provider.updatedAt ? `更新于 ${formatDateTime(row.provider.updatedAt)}` : '尚未保存配置' }}</span>
          <div>
            <el-button @click="reset(row.provider)">重置</el-button>
            <el-button type="primary" :loading="savingId === row.provider.id" @click="save(row.provider)">保存配置</el-button>
          </div>
        </footer>
      </article>

      <EmptyState v-if="!loading && !providers.length" description="暂无供应商配置，请确认服务端初始化状态" />
    </section>
  </div>
</template>
