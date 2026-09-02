<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage } from 'element-plus'
import { Cellphone, Lock, Refresh, User } from '@element-plus/icons-vue'
import { authApi } from '@/api/auth'
import { errorMessage } from '@/api/http'
import { authSession } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const formRef = ref<FormInstance>()
const loading = ref(false)
const captchaLoading = ref(false)
const captchaImage = ref('')

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

const adminSlug = computed(() => String(route.params.adminSlug || ''))

async function refreshCaptcha(): Promise<void> {
  captchaLoading.value = true
  try {
    const result = await authApi.captcha()
    form.captchaId = result.id
    captchaImage.value = result.image
    form.captcha = ''
  } catch (error) {
    captchaImage.value = ''
    ElMessage.error(errorMessage(error, '验证码加载失败，请点击重试'))
  } finally {
    captchaLoading.value = false
  }
}

function safeRedirect(): string {
  const fallback = `/${adminSlug.value}/dashboard`
  const target = typeof route.query.redirect === 'string' ? route.query.redirect : ''
  const prefix = `/${adminSlug.value}/`
  return target.startsWith(prefix) && !target.startsWith('//') ? target : fallback
}

async function submit(): Promise<void> {
  if (!formRef.value || !(await formRef.value.validate().catch(() => false))) return
  loading.value = true
  try {
    await authSession.login({
      username: form.username.trim(),
      password: form.password,
      captchaId: form.captchaId,
      captcha: form.captcha.trim(),
      adminPath: `/${adminSlug.value}`,
    })
    ElMessage.success('登录成功，欢迎回来')
    await router.replace(safeRedirect())
  } catch (error) {
    ElMessage.error(errorMessage(error, '用户名、密码或验证码不正确'))
    await refreshCaptcha()
    await nextTick()
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  if (route.query.expired === '1') ElMessage.warning('登录状态已过期，请重新登录')
  void refreshCaptcha()
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
        <p>聚合 HeroSMS、SMSBower 与 SMSPool，在一个安全、清晰的工作台完成号码采购与持续收码。</p>
        <div class="intro-features">
          <div><strong>3</strong><span>供应商聚合</span></div>
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
          <h2>登录管理后台</h2>
          <p>请输入管理员凭据以继续</p>
        </div>

        <el-form ref="formRef" :model="form" :rules="rules" label-position="top" size="large" @submit.prevent="submit">
          <el-form-item label="用户名" prop="username">
            <el-input v-model="form.username" autocomplete="username" placeholder="请输入用户名" :prefix-icon="User" />
          </el-form-item>
          <el-form-item label="密码" prop="password">
            <el-input
              v-model="form.password"
              autocomplete="current-password"
              placeholder="请输入密码"
              type="password"
              show-password
              :prefix-icon="Lock"
              @keyup.enter="submit"
            />
          </el-form-item>
          <el-form-item label="图片验证码" prop="captcha">
            <div class="captcha-row">
              <el-input
                v-model="form.captcha"
                autocomplete="off"
                maxlength="8"
                placeholder="不区分大小写"
                @keyup.enter="submit"
              />
              <button
                type="button"
                class="captcha-image"
                :class="{ 'is-loading': captchaLoading }"
                title="点击刷新验证码"
                @click="refreshCaptcha"
              >
                <img v-if="captchaImage" :src="captchaImage" alt="图片验证码" />
                <span v-else><el-icon><Refresh /></el-icon>重新加载</span>
              </button>
            </div>
          </el-form-item>
          <el-button class="login-submit" type="primary" native-type="submit" :loading="loading">
            安全登录
          </el-button>
        </el-form>

        <p class="login-security-note"><Lock /> 登录入口及通信均受服务端安全策略保护</p>
      </div>
    </section>
  </div>
</template>
