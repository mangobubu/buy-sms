import type { ProviderCode, PurchasePayload, Quote, SmsBowerTier } from '../types/api'

export interface DisplayPriceOption {
  key: string
  price: string
  available: number
  currency: string
  tier?: SmsBowerTier
  operator?: number
  label?: string
}

function validOperator(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0
}

export function priceOptionKey(tier: SmsBowerTier | undefined, price: string, operator?: number): string {
  const base = `${tier || 'standard'}:${price}`
  return validOperator(operator) ? `${base}:operator:${operator}` : base
}

export function displayQuotePriceOptions(quote: Quote): DisplayPriceOption[] {
  const options: NonNullable<Quote['priceOptions']> = quote.priceOptions?.length
    ? quote.priceOptions
    : quote.provider === 'smspin'
      ? []
      : [{ price: quote.price, available: quote.available }]
  return options
    .filter((option) => Number.isFinite(Number(option.price)) && Number(option.price) > 0 && option.available > 0)
    .filter((option) => option.operator === undefined || validOperator(option.operator))
    .filter((option) => quote.provider !== 'smspin' || validOperator(option.operator))
    .sort((left, right) => Number(left.price) - Number(right.price))
    .map((option) => ({
      ...option,
      key: priceOptionKey(quote.tier, option.price, option.operator),
      currency: quote.currency || 'USD',
      tier: quote.tier,
    }))
}

export function purchasePayloadForOption(
  conditions: { provider: ProviderCode; countryCode: string; serviceCode: string; tier?: SmsBowerTier | ''; duration?: string; priceMode?: 'fixed' | 'bid' },
  option: DisplayPriceOption,
): PurchasePayload {
  return {
    provider: conditions.provider,
    countryCode: conditions.countryCode,
    serviceCode: conditions.serviceCode,
    ...((option.tier || conditions.tier) ? { tier: option.tier || conditions.tier as SmsBowerTier } : {}),
    ...(conditions.duration ? { duration: conditions.duration } : {}),
    ...(validOperator(option.operator) ? { operator: option.operator } : {}),
    maxPrice: option.price.trim(),
    ...(conditions.priceMode === 'bid' ? { priceMode: 'bid' } : {}),
  }
}
