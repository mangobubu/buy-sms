import { http, unwrap } from './http'
import type { DashboardOverview } from '@/types/api'

export const dashboardApi = {
  overview: () => http.get<DashboardOverview>('/dashboard').then(unwrap),
}
