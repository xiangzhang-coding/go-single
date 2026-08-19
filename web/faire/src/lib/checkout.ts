import type { CreateOrderRequest } from "../api/types";

export type CheckoutIntent =
  | { kind: "cart" }
  | { kind: "direct"; productId: number; skuId: number; quantity: number };

export function parseCheckoutIntent(params: URLSearchParams): CheckoutIntent | null {
  const keys = ["product_id", "sku_id", "quantity"];
  if (!keys.some((key) => params.has(key))) {
    return { kind: "cart" };
  }

  const productId = positiveInteger(params.get("product_id"));
  const skuId = positiveInteger(params.get("sku_id"));
  const quantity = positiveInteger(params.get("quantity"));
  if (productId === null || skuId === null || quantity === null || quantity > 99) {
    return null;
  }
  return { kind: "direct", productId, skuId, quantity };
}

export function buildOrderRequest(
  intent: CheckoutIntent,
  clientRequestId: string,
  addressId: number,
  couponId: number,
): CreateOrderRequest {
  const base = {
    client_request_id: clientRequestId,
    address_id: addressId,
    coupon_id: couponId,
  };
  if (intent.kind === "cart") {
    return { ...base, from_cart: true };
  }
  return {
    ...base,
    from_cart: false,
    items: [{ sku_id: intent.skuId, quantity: intent.quantity }],
  };
}

function positiveInteger(value: string | null): number | null {
  if (value === null || value.trim() === "") return null;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : null;
}
