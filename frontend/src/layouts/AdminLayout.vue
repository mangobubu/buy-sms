<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessageBox } from 'element-plus'
import {
  ArrowDown,
  Bell,
  Cellphone,
  Connection,
  Expand,
  Fold,
  HomeFilled,
  Key,
  Menu as MenuIcon,
  Setting,
  SwitchButton,
  User,
} from '@element-plus/icons-vue'
import { authSession } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const collapsed = ref(false)
const mobileMenuVisible = ref(false)

const adminSlug = computed(() => String(route.params.adminSlug || ''))
const routeBase = computed(() => `/${adminSlug.value}`)
const activeMenu = computed(() => route.path)
const displayName = computed(
  () => authSession.state.user?.displayName || authSession.state.user?.username || '管理员',
)
const isAdmin = computed(() => authSession.state.user?.role === 'admin')

const menuGroups = computed(() => [
  {
    label: '工作台',
    items: [
      { path: '/dashboard', label: '仪表盘', icon: HomeFilled },
      { path: '/buy', label: '号码管理', icon: Cellphone },
    ],
  },
  {
    label: '系统',
    items: [
      ...(isAdmin.value
        ? [
            { path: '/providers', label: '供应商配置', icon: Connection },
            { path: '/users', label: '用户管理', icon: User },
          ]
        : []),
      { path: '/security', label: '修改密码', icon: Key },
    ],
  },
])

function navigate(path: string): void {
  mobileMenuVisible.value = false
  void router.push(`${routeBase.value}${path}`)
}

async function logout(): Promise<void> {
  try {
    await ElMessageBox.confirm('确定退出当前管理账号吗？', '退出登录', {
      confirmButtonText: '退出',
      cancelButtonText: '取消',
      type: 'warning',
    })
    await authSession.logout()
    await router.replace(`${routeBase.value}/login`)
  } catch {
    // 用户取消确认时保持当前会话。
  }
}

function handleUserCommand(command: string): void {
  if (command === 'security') navigate('/security')
  if (command === 'logout') void logout()
}
</script>

<template>
  <div class="admin-shell">
    <aside class="desktop-sidebar" :class="{ 'is-collapsed': collapsed }">
      <div class="brand" :class="{ 'is-collapsed': collapsed }">
        <div class="brand-mark"><Cellphone /></div>
        <div v-if="!collapsed" class="brand-copy">
          <strong>SMS Hub</strong>
          <span>号码聚合管理台</span>
        </div>
      </div>

      <el-scrollbar class="sidebar-scroll">
        <nav class="sidebar-nav">
          <div v-for="group in menuGroups" :key="group.label" class="menu-group">
            <div v-if="!collapsed" class="menu-group-label">{{ group.label }}</div>
            <button
              v-for="item in group.items"
              :key="item.path"
              type="button"
              class="nav-item"
              :class="{ 'is-active': activeMenu === `${routeBase}${item.path}` }"
              :title="collapsed ? item.label : undefined"
              @click="navigate(item.path)"
            >
              <el-icon :size="19"><component :is="item.icon" /></el-icon>
              <span v-if="!collapsed">{{ item.label }}</span>
            </button>
          </div>
        </nav>
      </el-scrollbar>

      <button class="collapse-button" type="button" @click="collapsed = !collapsed">
        <el-icon><Expand v-if="collapsed" /><Fold v-else /></el-icon>
        <span v-if="!collapsed">收起导航</span>
      </button>
    </aside>

    <el-drawer
      v-model="mobileMenuVisible"
      direction="ltr"
      :with-header="false"
      size="280px"
      class="mobile-drawer"
    >
      <div class="mobile-nav-panel">
        <div class="brand">
          <div class="brand-mark"><Cellphone /></div>
          <div class="brand-copy">
            <strong>SMS Hub</strong>
            <span>号码聚合管理台</span>
          </div>
        </div>
        <nav class="sidebar-nav">
          <div v-for="group in menuGroups" :key="group.label" class="menu-group">
            <div class="menu-group-label">{{ group.label }}</div>
            <button
              v-for="item in group.items"
              :key="item.path"
              type="button"
              class="nav-item"
              :class="{ 'is-active': activeMenu === `${routeBase}${item.path}` }"
              @click="navigate(item.path)"
            >
              <el-icon :size="19"><component :is="item.icon" /></el-icon>
              <span>{{ item.label }}</span>
            </button>
          </div>
        </nav>
      </div>
    </el-drawer>

    <section class="admin-main">
      <header class="topbar">
        <div class="topbar-start">
          <button class="mobile-menu-trigger" type="button" aria-label="打开导航" @click="mobileMenuVisible = true">
            <el-icon :size="22"><MenuIcon /></el-icon>
          </button>
          <div>
            <div class="topbar-title">{{ route.meta.title }}</div>
            <div class="topbar-caption">集中管理号码、验证码与供应商连接</div>
          </div>
        </div>

        <div class="topbar-actions">
          <el-tooltip content="当前无新通知" placement="bottom">
            <button class="icon-button" type="button" aria-label="通知">
              <el-icon :size="19"><Bell /></el-icon>
            </button>
          </el-tooltip>
          <el-dropdown trigger="click" @command="handleUserCommand">
            <button class="user-menu" type="button">
              <span class="user-avatar">{{ displayName.slice(0, 1).toUpperCase() }}</span>
              <span class="user-copy">
                <strong>{{ displayName }}</strong>
                <small>{{ authSession.state.user?.role === 'admin' ? '超级管理员' : '操作员' }}</small>
              </span>
              <el-icon><ArrowDown /></el-icon>
            </button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="security" :icon="Setting">修改密码</el-dropdown-item>
                <el-dropdown-item command="logout" :icon="SwitchButton" divided>退出登录</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </header>

      <main class="content-area">
        <RouterView v-slot="{ Component }">
          <Transition name="page" mode="out-in">
            <component :is="Component" />
          </Transition>
        </RouterView>
      </main>
    </section>
  </div>
</template>
