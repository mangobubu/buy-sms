<script setup lang="ts">
import { computed } from 'vue'

type TagType = 'primary' | 'success' | 'warning' | 'info' | 'danger'

interface StatusMeta {
  label: string
  type: TagType
}

interface Props {
  status?: string | null
  localCompleted?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  status: '',
})

const statusMap: Readonly<Record<string, StatusMeta>> = {
  pending: { label: '待处理', type: 'warning' },
  active: { label: '使用中', type: 'primary' },
  receiving: { label: '接收中', type: 'primary' },
  completed: { label: '已完成', type: 'success' },
  settled: { label: '已结算', type: 'success' },
  cancelled: { label: '已取消', type: 'info' },
  expired: { label: '已过期', type: 'warning' },
  failed: { label: '失败', type: 'danger' },
}

const statusMeta = computed<StatusMeta>(() => {
  const normalizedStatus = props.status?.trim().toLowerCase() ?? ''

  if (normalizedStatus === 'completed' && props.localCompleted) {
    return { label: '本地已结束', type: 'info' }
  }

  return statusMap[normalizedStatus] ?? {
    label: props.status?.trim() || '未知状态',
    type: 'info',
  }
})
</script>

<template>
  <el-tag
    :type="statusMeta.type"
    :title="localCompleted ? '仅结束本站记录，不代表上游已完成或退款' : undefined"
    effect="light"
    round
  >
    {{ statusMeta.label }}
  </el-tag>
</template>
