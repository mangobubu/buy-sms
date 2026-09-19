<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage } from 'element-plus'
import { Edit, Key, Plus, Refresh, User } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import EmptyState from '@/components/EmptyState.vue'
import TwoFactorKey from '@/components/TwoFactorKey.vue'
import { usersApi } from '@/api/users'
import { errorCode, errorMessage } from '@/api/http'
import { authSession } from '@/stores/auth'
import type { SaveUserPayload, SystemUser, TwoFactorConfiguration, TwoFactorSetup } from '@/types/api'
import { formatDateTime } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const loading = ref(false)
const saving = ref(false)
const users = ref<SystemUser[]>([])
const dialogVisible = ref(false)
const editingUser = ref<SystemUser | null>(null)
const formRef = ref<FormInstance>()
const twoFactorLoading = ref(false)
const twoFactorError = ref('')
const twoFactorSetup = ref<TwoFactorSetup | null>(null)
const existingTwoFactor = ref<TwoFactorConfiguration | null>(null)
let twoFactorRequest = 0
let listRequest = 0

const form = reactive({
  username: '',
  displayName: '',
  role: 'operator',
  enabled: true,
  password: '',
  twoFactorEnabled: false,
  twoFactorCode: '',
})

const needsEnrollment = computed(() => form.twoFactorEnabled && !editingUser.value?.twoFactorEnabled)
const visibleTwoFactor = computed(() => twoFactorSetup.value || existingTwoFactor.value)
const changesOwnTwoFactor = computed(
  () => Boolean(
    editingUser.value &&
    editingUser.value.id === authSession.state.user?.id &&
    Boolean(editingUser.value.twoFactorEnabled) !== form.twoFactorEnabled,
  ),
)

const rules = computed<FormRules>(() => ({
  username: [
    { required: true, message: '请输入用户名', trigger: 'blur' },
    { min: 3, max: 64, message: '用户名长度为 3 到 64 个字符', trigger: 'blur' },
  ],
  role: [{ required: true, message: '请选择角色', trigger: 'change' }],
  password: editingUser.value
    ? [{ min: 10, message: '新密码至少 10 个字符；留空表示不修改', trigger: 'blur' }]
    : [
        { required: true, message: '请输入初始密码', trigger: 'blur' },
        { min: 10, message: '密码至少 10 个字符', trigger: 'blur' },
      ],
  twoFactorCode: needsEnrollment.value
    ? [
        { required: true, message: '请输入身份验证器中的动态验证码以完成绑定', trigger: 'blur' },
        { pattern: /^\d{6}$/, message: '请输入 6 位数字验证码', trigger: 'blur' },
      ]
    : [],
}))

async function load(): Promise<void> {
  const request = ++listRequest
  loading.value = true
  try {
    const result = await usersApi.list()
    if (request === listRequest) users.value = result
  } catch (reason) {
    if (request === listRequest) ElMessage.error(errorMessage(reason, '用户列表加载失败'))
  } finally {
    if (request === listRequest) loading.value = false
  }
}

function clearTwoFactor(): void {
  // 关闭弹窗、切换用户或修改绑定用户名后，忽略此前仍在途中的密钥响应。
  ++twoFactorRequest
  twoFactorSetup.value = null
  existingTwoFactor.value = null
  twoFactorLoading.value = false
  twoFactorError.value = ''
  form.twoFactorCode = ''
  formRef.value?.clearValidate('twoFactorCode')
}

function clearForm(): void {
  clearTwoFactor()
  form.username = ''
  form.displayName = ''
  form.role = 'operator'
  form.enabled = true
  form.password = ''
  form.twoFactorEnabled = false
  formRef.value?.clearValidate()
}

function openCreate(): void {
  if (saving.value) return
  dialogVisible.value = false
  editingUser.value = null
  clearForm()
  dialogVisible.value = true
}

function openEdit(user: SystemUser): void {
  if (saving.value) return
  dialogVisible.value = false
  clearTwoFactor()
  editingUser.value = user
  form.username = user.username
  form.displayName = user.displayName || ''
  form.role = user.role
  form.enabled = user.enabled
  form.password = ''
  form.twoFactorEnabled = Boolean(user.twoFactorEnabled)
  formRef.value?.clearValidate()
  dialogVisible.value = true
}

async function prepareTwoFactor(): Promise<void> {
  if (!dialogVisible.value || !needsEnrollment.value || twoFactorLoading.value || saving.value) return
  const username = form.username.trim()
  if (username.length < 3 || username.length > 64) {
    twoFactorError.value = '请先填写 3 到 64 个字符的用户名，再生成绑定信息'
    return
  }

  clearTwoFactor()
  const request = twoFactorRequest
  twoFactorLoading.value = true
  try {
    const result = await usersApi.prepareTwoFactor({
      username,
      ...(editingUser.value ? { userId: editingUser.value.id } : {}),
    })
    if (request !== twoFactorRequest) return
    twoFactorSetup.value = result
  } catch (reason) {
    if (request !== twoFactorRequest) return
    twoFactorError.value = errorMessage(reason, '绑定信息生成失败，请重试')
  } finally {
    if (request === twoFactorRequest) twoFactorLoading.value = false
  }
}

async function revealTwoFactor(): Promise<void> {
  if (!dialogVisible.value || !editingUser.value?.twoFactorEnabled || !form.twoFactorEnabled || twoFactorLoading.value || saving.value) return
  clearTwoFactor()
  const request = twoFactorRequest
  const userId = editingUser.value.id
  twoFactorLoading.value = true
  try {
    const result = await usersApi.getTwoFactor(userId)
    if (request !== twoFactorRequest) return
    existingTwoFactor.value = result
  } catch (reason) {
    if (request !== twoFactorRequest) return
    twoFactorError.value = errorMessage(reason, '2FA 密钥加载失败，请重试')
  } finally {
    if (request === twoFactorRequest) twoFactorLoading.value = false
  }
}

function toggleTwoFactor(): void {
  clearTwoFactor()
  if (needsEnrollment.value) void prepareTwoFactor()
}

watch(
  () => form.username.trim(),
  () => {
    if (!dialogVisible.value || !needsEnrollment.value) return
    clearTwoFactor()
    twoFactorError.value = '用户名已变更，请重新生成绑定信息并在身份验证器中绑定'
  },
  { flush: 'sync' },
)

watch(
  dialogVisible,
  (visible) => {
    if (visible) return
    clearTwoFactor()
    form.password = ''
  },
  { flush: 'sync' },
)

async function save(): Promise<void> {
  if (saving.value || twoFactorLoading.value || !formRef.value) return
  saving.value = true
  try {
    if (!(await formRef.value.validate().catch(() => false))) return
    if (needsEnrollment.value && !twoFactorSetup.value) {
      ElMessage.warning('请先生成 2FA 绑定信息，并填写动态验证码')
      return
    }

    const payload: SaveUserPayload = {
      username: form.username.trim(),
      displayName: form.displayName.trim(),
      role: form.role,
      enabled: form.enabled,
    }
    // 未操作 2FA 开关的编辑保留服务端当前状态，避免旧窗口覆盖其他管理员的设置。
    if (!editingUser.value || form.twoFactorEnabled !== Boolean(editingUser.value.twoFactorEnabled)) {
      payload.twoFactorEnabled = form.twoFactorEnabled
    }
    if (form.password) payload.password = form.password
    if (needsEnrollment.value && twoFactorSetup.value) {
      payload.twoFactorSetupToken = twoFactorSetup.value.setupToken
      payload.twoFactorCode = form.twoFactorCode
    }

    const wasEditing = Boolean(editingUser.value)
    const ownTwoFactorChanged = changesOwnTwoFactor.value
    const mustSignInAgain = Boolean(
      editingUser.value &&
      editingUser.value.id === authSession.state.user?.id &&
      (ownTwoFactorChanged ||
        payload.username !== editingUser.value.username ||
        payload.role !== editingUser.value.role ||
        payload.enabled !== editingUser.value.enabled ||
        payload.password),
    )
    if (editingUser.value) await usersApi.update(editingUser.value.id, payload)
    else await usersApi.create(payload)

    dialogVisible.value = false
    if (mustSignInAgain) {
      authSession.clear()
      ElMessage.success(ownTwoFactorChanged
        ? (payload.twoFactorEnabled
          ? '2FA 已开启，请使用身份验证器的下一个验证码重新登录'
          : '2FA 已关闭，请重新登录')
        : '账户信息已更新，请重新登录')
      await router.replace({ name: 'login', params: { adminSlug: route.params.adminSlug } })
      return
    }
    ElMessage.success(wasEditing ? '用户信息已更新' : '用户已创建')
    await load()
  } catch (reason) {
    const code = errorCode(reason)
    if (code === 'two_factor_setup_expired' || code === 'two_factor_setup_invalid') {
      clearTwoFactor()
      twoFactorError.value = '绑定信息已过期或失效，请重新生成并绑定'
    }
    ElMessage.error(errorMessage(reason, editingUser.value ? '更新用户失败' : '创建用户失败'))
  } finally {
    saving.value = false
  }
}

onMounted(load)
onBeforeUnmount(() => {
  ++listRequest
  clearTwoFactor()
  form.password = ''
})
</script>

<template>
  <div class="page-stack users-page">
    <PageHeader title="用户管理" description="创建管理端用户并控制角色、登录状态与双重验证。">
      <template #actions>
        <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" :icon="Plus" @click="openCreate">新增用户</el-button>
      </template>
    </PageHeader>

    <section class="content-card users-table-card" v-loading="loading">
      <el-table :data="users" row-key="id">
        <el-table-column label="用户" min-width="210">
          <template #default="scope">
            <div class="user-cell">
              <span class="table-avatar"><User /></span>
              <span><strong>{{ scope.row.displayName || scope.row.username }}</strong><small>@{{ scope.row.username }}</small></span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="角色" min-width="120">
          <template #default="scope">
            <el-tag :type="scope.row.role === 'admin' ? 'primary' : 'info'" effect="light">
              {{ scope.row.role === 'admin' ? '管理员' : '操作员' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" min-width="110">
          <template #default="scope">
            <span class="user-status" :class="{ enabled: scope.row.enabled }"><i />{{ scope.row.enabled ? '正常' : '已停用' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="2FA" min-width="100">
          <template #default="scope">
            <el-tag :type="scope.row.twoFactorEnabled ? 'success' : 'info'" effect="light">
              {{ scope.row.twoFactorEnabled ? '已开启' : '未开启' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="最后登录" min-width="170">
          <template #default="scope">{{ formatDateTime(scope.row.lastLoginAt) }}</template>
        </el-table-column>
        <el-table-column label="创建时间" min-width="170">
          <template #default="scope">{{ formatDateTime(scope.row.createdAt) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="100" fixed="right">
          <template #default="scope">
            <el-button link type="primary" :icon="Edit" @click="openEdit(scope.row)">编辑</el-button>
          </template>
        </el-table-column>
        <template #empty><EmptyState description="暂无用户" /></template>
      </el-table>
    </section>

    <el-dialog
      v-model="dialogVisible"
      :title="editingUser ? '编辑用户' : '新增用户'"
      width="min(520px, calc(100vw - 32px))"
      :show-close="!saving"
      :close-on-click-modal="!saving"
      :close-on-press-escape="!saving"
      destroy-on-close
    >
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top" :disabled="saving" @submit.prevent="save">
        <div class="form-grid two-cols">
          <el-form-item label="用户名" prop="username">
            <el-input v-model="form.username" autocomplete="off" placeholder="用于登录" />
          </el-form-item>
          <el-form-item label="显示名称">
            <el-input v-model="form.displayName" autocomplete="off" placeholder="选填" />
          </el-form-item>
        </div>
        <el-form-item label="角色" prop="role">
          <el-radio-group v-model="form.role">
            <el-radio-button value="operator">操作员</el-radio-button>
            <el-radio-button value="admin">管理员</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item :label="editingUser ? '重置密码' : '初始密码'" prop="password">
          <el-input v-model="form.password" type="password" show-password autocomplete="new-password" :placeholder="editingUser ? '留空表示不修改' : '至少 10 个字符'" />
        </el-form-item>
        <div class="form-grid two-cols">
          <el-form-item label="允许登录">
            <el-switch v-model="form.enabled" inline-prompt active-text="启用" inactive-text="停用" />
          </el-form-item>
          <el-form-item label="开启2FA">
            <el-switch v-model="form.twoFactorEnabled" aria-label="开启2FA" inline-prompt active-text="开启" inactive-text="关闭" @change="toggleTwoFactor" />
          </el-form-item>
        </div>
        <div v-if="form.twoFactorEnabled" class="two-factor-settings">
          <p class="two-factor-help">
            {{ needsEnrollment ? '绑定身份验证器并填写动态验证码，保存后开启双重验证。' : '已开启双重验证，登录时需要身份验证器中的动态验证码。' }}
          </p>
          <el-alert v-if="twoFactorError" :title="twoFactorError" type="warning" :closable="false" show-icon />
          <el-skeleton v-if="twoFactorLoading" :rows="3" animated />
          <TwoFactorKey
            v-else-if="visibleTwoFactor"
            :secret="visibleTwoFactor.secret"
            :otpauth-url="visibleTwoFactor.otpauthUrl"
          />
          <el-button v-else-if="needsEnrollment" :icon="Key" @click="prepareTwoFactor">
            {{ twoFactorError ? '重新生成绑定信息' : '生成绑定信息' }}
          </el-button>
          <el-button v-else :icon="Key" @click="revealTwoFactor">
            {{ twoFactorError ? '重新加载二维码和密钥' : '查看二维码和密钥' }}
          </el-button>
          <template v-if="needsEnrollment && twoFactorSetup">
            <el-form-item class="two-factor-code-field" label="动态验证码" prop="twoFactorCode">
              <el-input
                v-model="form.twoFactorCode"
                autocomplete="one-time-code"
                inputmode="numeric"
                maxlength="6"
                placeholder="请输入身份验证器中的 6 位数字"
              />
            </el-form-item>
            <p class="two-factor-expiry">绑定信息有效至 {{ formatDateTime(twoFactorSetup.expiresAt) }}，取消将放弃此次绑定。</p>
          </template>
        </div>
        <p v-else-if="editingUser?.twoFactorEnabled" class="two-factor-help">保存后将关闭此账户的双重验证。</p>
        <el-alert v-if="changesOwnTwoFactor" class="two-factor-session-note" title="修改当前账户的 2FA 设置后需要重新登录" type="info" :closable="false" show-icon />
      </el-form>
      <template #footer>
        <el-button :disabled="saving" @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" :disabled="twoFactorLoading" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.two-factor-settings {
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 14px;
  min-width: 0;
}

.two-factor-settings > .el-button {
  align-self: flex-start;
  max-width: 100%;
  margin-left: 0;
}

.two-factor-help,
.two-factor-expiry {
  margin: 0;
  color: #667085;
  font-size: 13px;
  line-height: 1.7;
  overflow-wrap: anywhere;
}

.two-factor-code-field {
  margin-bottom: 0;
}

.two-factor-expiry {
  font-size: 12px;
}

.two-factor-session-note {
  margin-top: 16px;
}
</style>