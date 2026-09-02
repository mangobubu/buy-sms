import { ElMessage } from 'element-plus'

export async function copyText(value: string, label = '内容'): Promise<void> {
  if (!value) {
    ElMessage.warning(`暂无${label}可复制`)
    return
  }

  try {
    await navigator.clipboard.writeText(value)
    ElMessage.success(`${label}已复制`)
  } catch {
    const input = document.createElement('textarea')
    input.value = value
    input.style.position = 'fixed'
    input.style.opacity = '0'
    document.body.appendChild(input)
    input.select()
    document.execCommand('copy')
    input.remove()
    ElMessage.success(`${label}已复制`)
  }
}
