<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import type { FormInstance, FormRules } from 'element-plus'
import { ElMessage } from 'element-plus'
import { Edit, Plus, Refresh, User } from '@element-plus/icons-vue'
import PageHeader from '@/components/PageHeader.vue'
import EmptyState from '@/components/EmptyState.vue'
import { usersApi } from '@/api/users'
import { errorMessage } from '@/api/http'
import type { SaveUserPayload, SystemUser } from '@/types/api'
import { formatDateTime } from '@/utils/format'

const loading = ref(false)
const saving = ref(false)
const users = ref<SystemUser[]>([])
const dialogVisible = ref(false)
const editingUser = ref<SystemUser | null>(null)
const formRef = ref<FormInstance>()

const form = reactive<SaveUserPayload>({
  username: '',
  displayName: '',
  role: 'operator',
  enabled: true,
  password: '',
})

const rules = computed<FormRules>(() => ({
  username: [
    { required: true, message: '请输入用户名', trigger: 'blur' },
    { min: 3, max: 64, message: '用户名长度为 3 到 64 个字符', trigger: 'blur' },
  ],
  role: [{ required: true, message: '请选择角色', trigger: 'change' }],
  password: editingUser.value
    ? [{ min: 8, message: '新密码至少 8 个字符；留空表示不修改', trigger: 'blur' }]
    : [
        { required: true, message: '请输入初始密码', trigger: 'blur' },
        { min: 8, message: '密码至少 8 个字符', trigger: 'blur' },
      ],
}))

async function load(): Promise<void> {
  loading.value = true
  try {
    users.value = await usersApi.list()
  } catch (reason) {
    ElMessage.error(errorMessage(reason, '用户列表加载失败'))
  } finally {
    loading.value = false
  }
}

function clearForm(): void {
  form.username = ''
  form.displayName = ''
  form.role = 'operator'
  form.enabled = true
  form.password = ''
  formRef.value?.clearValidate()
}

function openCreate(): void {
  editingUser.value = null
  clearForm()
  dialogVisible.value = true
}

function openEdit(user: SystemUser): void {
  editingUser.value = user
  form.username = user.username
  form.displayName = user.displayName || ''
  form.role = user.role
  form.enabled = user.enabled
  form.password = ''
  formRef.value?.clearValidate()
  dialogVisible.value = true
}

async function save(): Promise<void> {
  if (!formRef.value || !(await formRef.value.validate().catch(() => false))) return
  const payload: SaveUserPayload = {
    username: form.username.trim(),
    displayName: form.displayName?.trim(),
    role: form.role,
    enabled: form.enabled,
  }
  if (form.password) payload.password = form.password

  saving.value = true
  try {
    if (editingUser.value) await usersApi.update(editingUser.value.id, payload)
    else await usersApi.create(payload)
    ElMessage.success(editingUser.value ? '用户信息已更新' : '用户已创建')
    dialogVisible.value = false
    await load()
  } catch (reason) {
    ElMessage.error(errorMessage(reason, editingUser.value ? '更新用户失败' : '创建用户失败'))
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page-stack users-page">
    <PageHeader title="用户管理" description="创建管理端用户并控制角色与登录状态。">
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

    <el-dialog v-model="dialogVisible" :title="editingUser ? '编辑用户' : '新增用户'" width="min(520px, calc(100vw - 32px))" destroy-on-close>
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
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
          <el-input v-model="form.password" type="password" show-password autocomplete="new-password" :placeholder="editingUser ? '留空表示不修改' : '至少 8 个字符'" />
        </el-form-item>
        <el-form-item label="允许登录">
          <el-switch v-model="form.enabled" inline-prompt active-text="启用" inactive-text="停用" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
