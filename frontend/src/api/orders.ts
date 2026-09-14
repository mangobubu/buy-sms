import { http, unwrap } from './http'
import type {
  NumberOrder,
  OrderQuery,
  PageResult,
  PurchasePayload,
  RenewalOptions,
  RenewalPayload,
} from '@/types/api'

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
  setPersonalUsed: (id: string, used: boolean) =>
    http.put<NumberOrder>(`/orders/${id}/personal-used`, { personalUsed: used }).then(unwrap),
  complete: (id: string) => http.post<NumberOrder>(`/orders/${id}/complete`).then(unwrap),
  closeLocal: (id: string, payload: { upstreamMissingConfirmed: true }) =>
    http.post<NumberOrder>(`/orders/${id}/close-local`, payload).then(unwrap),
  cancel: (id: string) => http.post<NumberOrder>(`/orders/${id}/cancel`).then(unwrap),
  renewalOptions: (id: string) =>
    http.get<RenewalOptions>(`/orders/${id}/renewal-options`).then(unwrap),
  renew: (id: string, payload: RenewalPayload, idempotencyKey: string) =>
    http
      .post<NumberOrder>(`/orders/${id}/renew`, payload, {
        headers: { 'Idempotency-Key': idempotencyKey },
        timeout: 50_000,
      })
      .then(unwrap),
}
