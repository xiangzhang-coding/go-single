import { describe, expect, test } from "bun:test";

import {
  isOrderProcessingResponse,
  shouldRetryOrderDetail,
} from "../web/faire/src/lib/order";

describe("order creation contract", () => {
  test("recognizes an explicit processing response", () => {
    expect(isOrderProcessingResponse({ state: "processing", order_no: "12345" })).toBe(true);
    expect(isOrderProcessingResponse({ order_no: "12345", status: "pending_payment" })).toBe(false);
  });

  test("retries a temporarily missing processing order up to thirty times", () => {
    expect(shouldRetryOrderDetail(0, 404, true)).toBe(true);
    expect(shouldRetryOrderDetail(29, 404, true)).toBe(true);
    expect(shouldRetryOrderDetail(30, 404, true)).toBe(false);
  });

  test("uses the normal two-retry budget outside processing", () => {
    expect(shouldRetryOrderDetail(1, 500, false)).toBe(true);
    expect(shouldRetryOrderDetail(2, 500, false)).toBe(false);
    expect(shouldRetryOrderDetail(0, 500, true)).toBe(true);
  });
});
