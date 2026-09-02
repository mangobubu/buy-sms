const normalize = (value?: string): string => {
  if (!value) return ''
  const clean = value.trim().replace(/^\/+|\/+$/g, '')
  return clean && !clean.includes('/') ? `/${clean}` : ''
}

export function configuredAdminPath(): string {
  return normalize(window.__APP_CONFIG__?.adminPath)
}

export function currentAdminPath(): string {
  const configured = configuredAdminPath()
  if (configured) return configured

  const firstSegment = window.location.pathname.split('/').filter(Boolean)[0]
  return normalize(firstSegment)
}

export function currentAdminSlug(): string {
  return currentAdminPath().slice(1)
}

export function isConfiguredAdminSlug(slug: unknown): boolean {
  const configured = configuredAdminPath()
  if (!configured) return typeof slug === 'string' && slug.length > 0
  return configured === normalize(typeof slug === 'string' ? slug : '')
}
