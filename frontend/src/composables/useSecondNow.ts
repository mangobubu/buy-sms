import { onBeforeUnmount, onMounted, ref, type Ref } from 'vue'

export function useSecondNow(): Ref<number> {
  const now = ref(Date.now())
  let timer: number | undefined

  function syncNow(): void {
    now.value = Date.now()
  }

  function onVisibilityChange(): void {
    if (document.visibilityState === 'visible') syncNow()
  }

  onMounted(() => {
    syncNow()
    timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') syncNow()
    }, 1_000)
    document.addEventListener('visibilitychange', onVisibilityChange)
  })

  onBeforeUnmount(() => {
    if (timer) window.clearInterval(timer)
    document.removeEventListener('visibilitychange', onVisibilityChange)
  })

  return now
}
