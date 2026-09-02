<script setup lang="ts">
import { computed } from 'vue'
import { DocumentCopy } from '@element-plus/icons-vue'
import { copyText } from '@/utils/clipboard'

interface Props {
  value?: string | number | null
  label?: string
}

const props = withDefaults(defineProps<Props>(), {
  value: '',
  label: '内容',
})

const copyValue = computed(() => String(props.value ?? ''))
const accessibleLabel = computed(() => `复制${props.label}`)

async function handleCopy(): Promise<void> {
  await copyText(copyValue.value, props.label)
}
</script>

<template>
  <el-button
    class="copy-button"
    link
    type="primary"
    :icon="DocumentCopy"
    :aria-label="accessibleLabel"
    :title="accessibleLabel"
    @click.stop="handleCopy"
  >
    <slot>复制</slot>
  </el-button>
</template>

<style scoped>
.copy-button {
  flex: 0 0 auto;
  padding-inline: 4px;
  vertical-align: middle;
}
</style>
