<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Key, Lock } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import { authApi } from '@/api/auth'
import { errorMessage } from '@/api/http'
import { authSession } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const formRef = ref<FormInstance>()
const saving = ref(false)
const form = reactive({ currentPassword: '', newPassword: '', confirmPassword: '' })

const rules: FormRules = {
  currentPassword: [{ required: true, message: '请输入当前密码', trigger: 'blur' }],
  newPassword: [
    { required: true, message: '请输入新密码', trigger: 'blur' },
    { min: 10, message: '新密码至少 10 个字符', trigger: 'blur' },
    {
      validator: (_rule, value, callback) =>
        value === form.currentPassword ? callback(new Error('新密码不能与当前密码相同')) : callback(),
      trigger: 'blur',
    },
  ],
  confirmPassword: [
    { required: true, message: '请再次输入新密码', trigger: 'blur' },
    {
      validator: (_rule, value, callback) =>
        value === form.newPassword ? callback() : callback(new Error('两次输入的新密码不一致')),
      trigger: 'blur',
    },
  ],
}

async function submit(): Promise<void> {
  if (!formRef.value || !(await formRef.value.validate().catch(() => false))) return
  saving.value = true
  try {
    await authApi.changePassword({ currentPassword: form.currentPassword, newPassword: form.newPassword })
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '密码修改失败，请确认当前密码是否正确'))
    saving.value = false
    return
  }

  try {
    await ElMessageBox.alert('密码已更新，请使用新密码重新登录。', '修改成功', {
      confirmButtonText: '重新登录',
      type: 'success',
    })
  } catch {
    // 关闭提示后仍继续退出，避免旧会话停留在管理页。
  }
  try {
    // 改密接口会撤销当前会话；此处远端注销的 401 或网络失败不应阻断跳转。
    await authSession.logout()
  } finally {
    authSession.clear()
    saving.value = false
    await router.replace(`/${String(route.params.adminSlug)}/login`)
  }
}
</script>

<template>
  <div class="page-stack security-page">
    <PageHeader title="修改密码" description="定期更新管理密码，以降低账号泄露风险。" />
    <section class="security-layout">
      <article class="content-card password-card">
        <div class="security-card-heading">
          <span><Key /></span>
          <div><h3>更新登录密码</h3><p>修改成功后，当前会话将退出。</p></div>
        </div>
        <el-form ref="formRef" :model="form" :rules="rules" label-position="top" @submit.prevent="submit">
          <el-form-item label="当前密码" prop="currentPassword">
            <el-input v-model="form.currentPassword" type="password" show-password autocomplete="current-password" :prefix-icon="Lock" />
          </el-form-item>
          <el-form-item label="新密码" prop="newPassword">
            <el-input v-model="form.newPassword" type="password" show-password autocomplete="new-password" :prefix-icon="Key" placeholder="至少 10 个字符" />
          </el-form-item>
          <el-form-item label="确认新密码" prop="confirmPassword">
            <el-input v-model="form.confirmPassword" type="password" show-password autocomplete="new-password" :prefix-icon="Key" />
          </el-form-item>
          <el-button type="primary" native-type="submit" :loading="saving">保存新密码</el-button>
        </el-form>
      </article>
      <aside class="password-advice">
        <div class="advice-icon"><Lock /></div>
        <h3>密码安全建议</h3>
        <ul>
          <li>使用至少 10 个字符</li>
          <li>混合字母、数字和符号</li>
          <li>避免使用其他网站的相同密码</li>
          <li>不要向任何人透露管理密码</li>
        </ul>
      </aside>
    </section>
  </div>
</template>
