import { http, unwrap } from './http'
import type { NumberOrder, OrderQuery, PageResult, PurchasePayload } from '@/types/api'

export const ordersApi = {
  create: (payload: PurchasePayload, idempotencyKey: string) =>
    http
      .post<NumberOrder>('/orders', payload, {
        headers: { 'Idempotency-Key': idempotencyKey },
        timeout: 50_000,
      })
      .then(unwrap),
  list: (query: OrderQuery) =>
    http.get<PageResult<NumberOrder> | NumberOrder[]>('/orders', { params: query }).then(unwrap),
  detail: (id: string) => http.get<NumberOrder>(`/orders/${id}`).then(unwrap),
  complete: (id: string) => http.post<NumberOrder>(`/orders/${id}/complete`).then(unwrap),
  cancel: (id: string) => http.post<NumberOrder>(`/orders/${id}/cancel`).then(unwrap),
}
