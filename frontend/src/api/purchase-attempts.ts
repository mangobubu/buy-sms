import { http, unwrap } from './http'
import type { PurchaseAttempt } from '@/types/api'

export const purchaseAttemptsApi = {
  list: () => http.get<PurchaseAttempt[]>('/purchase-attempts').then(unwrap),
}
