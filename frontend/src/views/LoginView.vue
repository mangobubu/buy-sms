<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { FormInstance, FormRules, InputInstance } from 'element-plus'
import { ElMessage } from 'element-plus'
import { Cellphone, Key, Lock, Refresh, User } from '@element-plus/icons-vue'
import { authApi } from '@/api/auth'
import { errorCode, errorMessage } from '@/api/http'
import { authSession } from '@/stores/auth'
import type { TwoFactorChallenge } from '@/types/api'

const route = useRoute()
const router = useRouter()
const formRef = ref<FormInstance>()
const twoFactorFormRef = ref<FormInstance>()
const usernameInput = ref<InputInstance>()
const twoFactorInput = ref<InputInstance>()
const loading = ref(false)
const captchaLoading = ref(false)
const captchaImage = ref('')
const challenge = ref<TwoFactorChallenge | null>(null)
const verification = reactive({ code: '' })
let captchaRequest = 0

const form = reactive({
  username: '',
  password: '',
  captchaId: '',
  captcha: '',
})

const rules: FormRules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
  captcha: [{ required: true, message: '请输入图片验证码', trigger: 'blur' }],
}

const twoFactorRules: FormRules = {
  code: [
    { required: true, message: '请输入动态验证码', trigger: 'blur' },
    { pattern: /^\d{6}$/, message: '请输入 6 位数字验证码', trigger: 'blur' },
  ],
}

const adminSlug = computed(() => String(route.params.adminSlug || ''))

async function refreshCaptcha(): Promise<void> {
  if (captchaLoading.value) return
  const request = ++captchaRequest
  captchaLoading.value = true
  form.captchaId = ''
  form.captcha = ''
  try {
    const result = await authApi.captcha()
    if (request !== captchaRequest) return
    form.captchaId = result.id
    captchaImage.value = result.image
  } catch (error) {
    if (request !== captchaRequest) return
    captchaImage.value = ''
    ElMessage.error(errorMessage(error, '验证码加载失败，请点击重试'))
  } finally {
    if (request === captchaRequest) captchaLoading.value = false
  }
}

function safeRedirect(): string {
  const fallback = `/${adminSlug.value}/dashboard`
  const target = typeof route.query.redirect === 'string' ? route.query.redirect : ''
  const prefix = `/${adminSlug.value}/`
  return target.startsWith(prefix) && !target.startsWith('//') ? target : fallback
}

async function returnToPassword(): Promise<void> {
  challenge.value = null
  verification.code = ''
  form.password = ''
  form.captcha = ''
  form.captchaId = ''
  captchaImage.value = ''
  await nextTick()
  formRef.value?.clearValidate()
  usernameInput.value?.focus()
  await refreshCaptcha()
}

async function finishLogin(): Promise<void> {
  form.password = ''
  verification.code = ''
  ElMessage.success('登录成功，欢迎回来')
  await router.replace(safeRedirect())
}

async function submit(): Promise<void> {
  if (loading.value || captchaLoading.value || !formRef.value) return
  loading.value = true
  try {
    if (!(await formRef.value.validate().catch(() => false))) return
    const result = await authSession.login({
      username: form.username.trim(),
      password: form.password,
      captchaId: form.captchaId,
      captcha: form.captcha.trim(),
      adminPath: `/${adminSlug.value}`,
    })
    if ('twoFactorRequired' in result) {
      challenge.value = result
      verification.code = ''
      form.password = ''
      form.captcha = ''
      form.captchaId = ''
      captchaImage.value = ''
      loading.value = false
      await nextTick()
      twoFactorInput.value?.focus()
      return
    }
    await finishLogin()
  } catch (error) {
    ElMessage.error(errorMessage(error, '用户名、密码或验证码不正确'))
    await refreshCaptcha()
  } finally {
    loading.value = false
  }
}

async function submitTwoFactor(): Promise<void> {
  if (loading.value || !challenge.value || !twoFactorFormRef.value) return
  loading.value = true
  try {
    if (!(await twoFactorFormRef.value.validate().catch(() => false))) return
    await authSession.verifyTwoFactor({
      challengeToken: challenge.value.challengeToken,
      code: verification.code,
      adminPath: `/${adminSlug.value}`,
    })
    await finishLogin()
  } catch (error) {
    ElMessage.error(errorMessage(error, '动态验证码验证失败，请重试'))
    loading.value = false
    if (errorCode(error) === 'two_factor_expired') {
      await returnToPassword()
    } else {
      verification.code = ''
      await nextTick()
      twoFactorFormRef.value?.clearValidate()
      twoFactorInput.value?.focus()
    }
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  if (route.query.expired === '1') ElMessage.warning('登录状态已过期，请重新登录')
  void refreshCaptcha()
})

onBeforeUnmount(() => {
  ++captchaRequest
  form.password = ''
  verification.code = ''
  challenge.value = null
})
</script>

<template>
  <div class="login-page">
    <section class="login-intro">
      <div class="intro-brand">
        <span class="brand-mark"><Cellphone /></span>
        <span>SMS Hub</span>
      </div>
      <div class="intro-content">
        <div class="eyebrow"><span /> 多平台统一接入</div>
        <h1>每一条验证码，<br />都在掌控之中。</h1>
        <p>聚合 HeroSMS、SMSBower、SMSPool 与 SMSPin，在一个安全、清晰的工作台完成号码采购与持续收码。</p>
        <div class="intro-features">
          <div><strong>4</strong><span>供应商聚合</span></div>
          <div><strong>24/7</strong><span>持续接收</span></div>
          <div><strong>Webhook</strong><span>实时优先</span></div>
        </div>
      </div>
      <div class="intro-foot">所有业务数据均由服务端持久化保存</div>
    </section>

    <section class="login-panel">
      <div class="login-card">
        <div class="mobile-login-brand">
          <span class="brand-mark"><Cellphone /></span>
          <span>SMS Hub</span>
        </div>
        <div class="login-heading">
          <span class="secure-dot"><i /></span>
          <h2>{{ challenge ? '双重身份验证' : '登录管理后台' }}</h2>
          <p>{{ challenge ? '请输入身份验证器中的 6 位动态验证码' : '请输入管理员凭据以继续' }}</p>
        </div>

        <el-form v-if="!challenge" ref="formRef" :model="form" :rules="rules" label-position="top" size="large" :disabled="loading" @submit.prevent="submit">
          <el-form-item label="用户名" prop="username">
            <el-input ref="usernameInput" v-model="form.username" autocomplete="username" placeholder="请输入用户名" :prefix-icon="User" />
          </el-form-item>
          <el-form-item label="密码" prop="password">
            <el-input
              v-model="form.password"
              autocomplete="current-password"
              placeholder="请输入密码"
              type="password"
              show-password
              :prefix-icon="Lock"
            />
          </el-form-item>
          <el-form-item label="图片验证码" prop="captcha">
            <div class="captcha-row">
              <el-input
                v-model="form.captcha"
                autocomplete="off"
                maxlength="8"
                placeholder="不区分大小写"
              />
              <button
                type="button"
                class="captcha-image"
                :class="{ 'is-loading': captchaLoading }"
                :disabled="captchaLoading || loading"
                title="点击刷新验证码"
                @click="refreshCaptcha"
              >
                <img v-if="captchaImage" :src="captchaImage" alt="图片验证码" />
                <span v-else><el-icon><Refresh /></el-icon>重新加载</span>
              </button>
            </div>
          </el-form-item>
          <el-button class="login-submit" type="primary" native-type="submit" :loading="loading" :disabled="captchaLoading || !form.captchaId">
            安全登录
          </el-button>
        </el-form>

        <el-form v-else ref="twoFactorFormRef" :model="verification" :rules="twoFactorRules" label-position="top" size="large" :disabled="loading" @submit.prevent="submitTwoFactor">
          <p class="two-factor-account">正在验证 <strong>{{ form.username }}</strong></p>
          <el-form-item label="动态验证码" prop="code">
            <el-input
              ref="twoFactorInput"
              v-model="verification.code"
              class="two-factor-code-input"
              autocomplete="one-time-code"
              inputmode="numeric"
              maxlength="6"
              placeholder="6 位数字验证码"
              :prefix-icon="Key"
            />
          </el-form-item>
          <el-button class="login-submit" type="primary" native-type="submit" :loading="loading">
            验证并登录
          </el-button>
          <el-button class="two-factor-back" text :disabled="loading" @click="returnToPassword">
            返回账号密码登录
          </el-button>
        </el-form>

        <p class="login-security-note"><Lock /> 登录入口及通信均受服务端安全策略保护</p>
      </div>
    </section>
  </div>
</template>

<style scoped>
.two-factor-account {
  margin: 0 0 24px;
  color: #667085;
  overflow-wrap: anywhere;
}

.two-factor-account strong {
  color: #344054;
}

.two-factor-code-input :deep(input) {
  letter-spacing: 0.12em;
}

.two-factor-back {
  width: 100%;
  margin: 12px 0 0;
}
</style>