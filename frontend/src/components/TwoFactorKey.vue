<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import QRCode from 'qrcode'
import CopyButton from '@/components/CopyButton.vue'

const props = defineProps<{ secret: string; otpauthUrl: string }>()
const image = ref('')
const loading = ref(false)
const failed = ref(false)
let renderRequest = 0

async function renderQrCode(): Promise<void> {
  const request = ++renderRequest
  const otpauthUrl = props.otpauthUrl
  image.value = ''
  failed.value = false
  loading.value = true
  try {
    const result = await QRCode.toDataURL(otpauthUrl, {
      width: 208,
      margin: 2,
      errorCorrectionLevel: 'M',
    })
    if (request === renderRequest) image.value = result
  } catch {
    if (request === renderRequest) failed.value = true
  } finally {
    if (request === renderRequest) loading.value = false
  }
}

watch(() => props.otpauthUrl, () => void renderQrCode(), { immediate: true })

onBeforeUnmount(() => {
  ++renderRequest
  image.value = ''
})
</script>

<template>
  <div class="two-factor-key">
    <p class="two-factor-instructions">使用身份验证器扫描二维码，或手动输入下方密钥。</p>
    <div class="two-factor-qr" v-loading="loading">
      <img v-if="image" :src="image" width="208" height="208" alt="账户 2FA 绑定二维码" />
      <div v-else-if="failed" class="two-factor-qr-error" role="status">
        <span>二维码生成失败，可使用密钥手动绑定</span>
        <el-button link type="primary" @click="renderQrCode">重新生成二维码</el-button>
      </div>
    </div>
    <div class="two-factor-key-label">2FA 密钥</div>
    <div class="two-factor-secret">
      <code>{{ secret }}</code>
      <CopyButton :value="secret" label="2FA 密钥" />
    </div>
  </div>
</template>

<style scoped>
.two-factor-key {
  min-width: 0;
  padding: 16px;
  border: 1px solid #e4e7ec;
  border-radius: 12px;
  background: #f9fafb;
}

.two-factor-instructions {
  margin: 0 0 12px;
  color: #667085;
  font-size: 13px;
  line-height: 1.7;
}

.two-factor-qr {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 208px;
  max-width: 100%;
  min-height: 208px;
  margin: 0 auto 16px;
  border-radius: 8px;
  background: #fff;
}

.two-factor-qr img {
  display: block;
  max-width: 100%;
  height: auto;
}

.two-factor-qr-error {
  display: grid;
  gap: 12px;
  padding: 20px;
  color: #667085;
  text-align: center;
  font-size: 13px;
  line-height: 1.6;
}

.two-factor-key-label {
  margin-bottom: 6px;
  color: #475467;
  font-size: 12px;
  font-weight: 600;
}

.two-factor-secret {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  min-width: 0;
}

.two-factor-secret code {
  flex: 1;
  min-width: 0;
  padding-top: 2px;
  color: #101828;
  font-size: 14px;
  line-height: 1.7;
  letter-spacing: 0.04em;
  overflow-wrap: anywhere;
}

.two-factor-secret :deep(.copy-button) {
  margin-top: 3px;
}
</style>