import type { CreateOrderResponse, OrderProcessingResponse } from "../api/types";

export function isOrderProcessingResponse(value: CreateOrderResponse | unknown): value is OrderProcessingResponse {
  return typeof value === "object"
    && value !== null
    && "state" in value
    && value.state === "processing"
    && "order_no" in value
    && typeof value.order_no === "string";
}

export function shouldRetryOrderDetail(
  failureCount: number,
  status: number | undefined,
  awaitingCreation: boolean,
): boolean {
  if (awaitingCreation && status === 404) return failureCount < 30;
  return failureCount < 2;
}
