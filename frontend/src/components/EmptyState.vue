<script setup lang="ts">
interface Props {
  description?: string
  imageSize?: number
}

withDefaults(defineProps<Props>(), {
  description: '暂无数据',
  imageSize: 96,
})
</script>

<template>
  <el-empty class="empty-state" :image-size="imageSize">
    <template v-if="$slots.image" #image>
      <slot name="image" />
    </template>

    <template #description>
      <slot name="description">
        <p class="empty-state__description">{{ description }}</p>
      </slot>
    </template>

    <slot />
  </el-empty>
</template>

<style scoped>
.empty-state {
  --el-empty-padding: 40px 0;
}

.empty-state__description {
  margin: 0;
  color: var(--el-text-color-secondary);
  font-size: 14px;
  line-height: 1.6;
}
</style>
