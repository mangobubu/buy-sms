import { createRouter, createWebHistory, type RouteLocationNormalized } from 'vue-router'
import { authSession } from '@/stores/auth'
import { configuredAdminPath, isConfiguredAdminSlug } from '@/utils/admin-path'

function adminSlug(to: RouteLocationNormalized): string {
  return typeof to.params.adminSlug === 'string' ? to.params.adminSlug : ''
}

const router = createRouter({
  history: createWebHistory(),
  scrollBehavior(to) {
    if (!to.hash) return { top: 0 }
    return new Promise((resolve) => {
      window.setTimeout(() => resolve({ el: to.hash, top: 20, behavior: 'smooth' }), 100)
    })
  },
  routes: [
    {
      path: '/:adminSlug/login',
      name: 'login',
      component: () => import('@/views/LoginView.vue'),
      meta: { public: true, adminRoute: true, title: '登录' },
    },
    {
      path: '/:adminSlug',
      component: () => import('@/layouts/AdminLayout.vue'),
      meta: { requiresAuth: true, adminRoute: true },
      children: [
        {
          path: '',
          redirect: (to) => ({ name: 'dashboard', params: { adminSlug: to.params.adminSlug } }),
        },
        {
          path: 'dashboard',
          name: 'dashboard',
          component: () => import('@/views/DashboardView.vue'),
          meta: { title: '仪表盘', requiresAuth: true, adminRoute: true },
        },
        {
          path: 'buy',
          name: 'buy-number',
          component: () => import('@/views/BuyNumberView.vue'),
          meta: { title: '号码管理', requiresAuth: true, adminRoute: true },
        },
        {
          path: 'orders',
          name: 'orders',
          redirect: (to) => ({
            name: 'buy-number',
            params: { adminSlug: to.params.adminSlug },
            query: to.query,
            hash: to.hash || '#orders',
          }),
          meta: { title: '号码管理', requiresAuth: true, adminRoute: true },
        },
        {
          path: 'providers',
          name: 'providers',
          component: () => import('@/views/ProviderSettingsView.vue'),
          meta: { title: '供应商配置', requiresAuth: true, adminRoute: true, adminOnly: true },
        },
        {
          path: 'users',
          name: 'users',
          component: () => import('@/views/UsersView.vue'),
          meta: { title: '用户管理', requiresAuth: true, adminRoute: true, adminOnly: true },
        },
        {
          path: 'security',
          name: 'security',
          component: () => import('@/views/SecurityView.vue'),
          meta: { title: '修改密码', requiresAuth: true, adminRoute: true },
        },
      ],
    },
    {
      path: '/404',
      name: 'not-found',
      component: () => import('@/views/NotFoundView.vue'),
      meta: { title: '页面不存在' },
    },
    {
      path: '/:pathMatch(.*)*',
      name: 'catch-all',
      component: () => import('@/views/NotFoundView.vue'),
      meta: { title: '页面不存在' },
    },
  ],
})

router.beforeEach(async (to) => {
  const title = typeof to.meta.title === 'string' ? to.meta.title : ''
  document.title = title ? `${title} · 短信聚合管理台` : '短信聚合管理台'

  if (to.meta.adminRoute) {
    const slug = adminSlug(to)
    if (!isConfiguredAdminSlug(slug)) return { name: 'not-found' }
  }

  if (to.meta.requiresAuth) {
    const authenticated = await authSession.hydrate()
    if (!authenticated) {
      const slug = adminSlug(to)
      if (!slug) return { name: 'not-found' }
      return {
        name: 'login',
        params: { adminSlug: slug },
        query: { redirect: to.fullPath },
      }
    }
  }

  if (to.meta.adminOnly && authSession.state.user?.role !== 'admin') {
    return { name: 'not-found' }
  }

  if (to.name === 'login' && authSession.hasToken()) {
    const authenticated = await authSession.hydrate()
    if (authenticated) {
      return { name: 'dashboard', params: { adminSlug: adminSlug(to) } }
    }
  }

  return true
})

router.onError(() => {
  const configured = configuredAdminPath()
  if (configured && window.location.pathname.startsWith(configured)) {
    window.location.assign(`${configured}/login`)
  }
})

export default router
