<script setup lang="ts">
import { computed } from 'vue'
import { copyText } from '@/utils/clipboard'
import { formatPhoneNumber, getPhoneNumberParts } from '@/utils/format'

interface Props {
  value?: string | null
}

const props = withDefaults(defineProps<Props>(), {
  value: '',
})

const parts = computed(() => getPhoneNumberParts(props.value ?? undefined))
const fallbackText = computed(() => formatPhoneNumber(props.value ?? undefined))

async function copyFullNumber(): Promise<void> {
  if (!parts.value) return
  await copyText(parts.value.fullNumber, '完整号码')
}

async function copyLocalNumber(): Promise<void> {
  if (!parts.value?.localNumber) return
  await copyText(parts.value.localNumber, '号码')
}
</script>

<template>
  <strong v-if="parts" class="phone-number-copy">
    <button
      class="phone-number-part calling-code"
      type="button"
      :aria-label="`复制完整号码 ${parts.fullNumber}`"
      :title="`复制完整号码 ${parts.fullNumber}`"
      @click.stop="copyFullNumber"
    >
      {{ parts.callingCode }}
    </button>
    <button
      v-if="parts.localNumber"
      class="phone-number-part local-number"
      type="button"
      :aria-label="`复制区号后面的号码 ${parts.localNumber}`"
      :title="`复制区号后面的号码 ${parts.localNumber}`"
      @click.stop="copyLocalNumber"
    >
      {{ parts.formattedLocalNumber }}
    </button>
  </strong>
  <strong v-else>{{ fallbackText }}</strong>
</template>

<style scoped>
.phone-number-copy {
  display: inline-flex;
  align-items: baseline;
  gap: 4px;
}

.phone-number-part {
  appearance: none;
  padding: 0;
  color: inherit;
  font: inherit;
  font-weight: inherit;
  line-height: inherit;
  white-space: nowrap;
  cursor: pointer;
  background: transparent;
  border: 0;
}

.phone-number-part:hover,
.phone-number-part:focus-visible {
  color: var(--el-color-primary);
}

.phone-number-part:focus-visible {
  border-radius: 2px;
  outline: 2px solid var(--el-color-primary-light-5);
  outline-offset: 2px;
}
</style>
