<script setup lang="ts">
import { Warning } from '@element-plus/icons-vue'
</script>

<template>
  <main class="not-found-page">
    <section class="not-found-card">
      <div class="not-found-icon"><el-icon><Warning /></el-icon></div>
      <div class="not-found-code">404</div>
      <h1>页面不存在</h1>
      <p>请求的地址不存在或已被移除。请检查地址后重试。</p>
    </section>
  </main>
</template>
