<script setup lang="ts">
import { computed } from 'vue'
import { Timer } from '@element-plus/icons-vue'
import { getOrderCountdown } from '@/utils/countdown'

const props = defineProps<{
  status?: string
  expiresAt?: string
  now: number
}>()

const countdown = computed(() => getOrderCountdown(props.status, props.expiresAt, props.now))
const caption = computed(() => countdown.value.state === 'active' ? '剩余' : '时效')
</script>

<template>
  <span
    class="order-countdown"
    :class="`is-${countdown.state}`"
    :aria-label="`${caption}${countdown.text}`"
  >
    <el-icon><Timer /></el-icon>
    <span>{{ caption }}</span>
    <strong>{{ countdown.text }}</strong>
  </span>
</template>
