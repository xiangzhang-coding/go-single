import { describe, expect, test } from "bun:test";

import { buildOrderRequest, parseCheckoutIntent } from "../src/lib/checkout";
import { toUpdateAddressRequest } from "../src/lib/address";
import {
  describeCouponRule,
  formatAddress,
  formatDate,
  formatMoney,
  formatSpecs,
  isCouponUsable,
  parseSpecs,
  toLocalInput,
} from "../src/lib/format";
import { validateImage, validateMessageFile } from "../src/lib/media";
import { resolveProductArtwork } from "../src/lib/product-art";

describe("checkout intent", () => {
  test("uses the cart when no direct-purchase parameters exist", () => {
    const intent = parseCheckoutIntent(new URLSearchParams());
    expect(intent).toEqual({ kind: "cart" });
    expect(buildOrderRequest(intent!, "request-1", 7, 0)).toEqual({
      client_request_id: "request-1",
      address_id: 7,
      coupon_id: 0,
      from_cart: true,
    });
  });

  test("builds a direct order only from complete positive integer parameters", () => {
    const intent = parseCheckoutIntent(new URLSearchParams({
      product_id: "3",
      sku_id: "9",
      quantity: "2",
    }));
    expect(intent).toEqual({ kind: "direct", productId: 3, skuId: 9, quantity: 2 });
    expect(buildOrderRequest(intent!, "request-2", 8, 12)).toEqual({
      client_request_id: "request-2",
      address_id: 8,
      coupon_id: 12,
      from_cart: false,
      items: [{ sku_id: 9, quantity: 2 }],
    });
  });

  test("rejects partial, fractional, unsafe, and over-limit direct purchases", () => {
    expect(parseCheckoutIntent(new URLSearchParams({
      product_id: "1",
      sku_id: "2",
      quantity: "99",
    }))).toEqual({ kind: "direct", productId: 1, skuId: 2, quantity: 99 });
    for (const query of [
      { product_id: "1", sku_id: "2" },
      { product_id: "1", sku_id: "2", quantity: "1.5" },
      { product_id: "1", sku_id: "2", quantity: "100" },
      { product_id: "1", sku_id: "0", quantity: "1" },
      { product_id: "1", sku_id: String(Number.MAX_SAFE_INTEGER + 1), quantity: "1" },
    ]) {
      expect(parseCheckoutIntent(new URLSearchParams(query))).toBeNull();
    }
  });
});

describe("address requests", () => {
  test("an update omits the create-only default selection", () => {
    expect(toUpdateAddressRequest({
      receiver: "Alice",
      phone: "13800138000",
      province: "粤",
      city: "深",
      district: "南山",
      detail: "科技园",
      is_default: true,
    })).toEqual({
      receiver: "Alice",
      phone: "13800138000",
      province: "粤",
      city: "深",
      district: "南山",
      detail: "科技园",
    });
  });
});

describe("display and coupon rules", () => {
  test("formats money, addresses, and specification variants", () => {
    expect(formatMoney(1234)).toContain("12.34");
    expect(formatAddress({ province: "粤", city: "深", district: "南山", detail: "科技园" })).toBe("粤深南山科技园");
    expect(parseSpecs({ color: "红", size: 42 })).toEqual([["color", "红"], ["size", "42"]]);
    expect(parseSpecs('{"color":"蓝"}')).toEqual([["color", "蓝"]]);
    expect(parseSpecs("XL")).toEqual([["规格", "XL"]]);
    expect(parseSpecs(null)).toEqual([]);
    expect(formatSpecs({ color: "红" })).toBe("color: 红");
    expect(formatSpecs(null)).toBe("标准规格");
  });

  test("keeps invalid dates visible and converts valid local input", () => {
    expect(formatDate()).toBe("-");
    expect(formatDate("not-a-date")).toBe("not-a-date");
    expect(formatDate("2026-01-02T03:04:00Z")).toContain("2026");
    expect(toLocalInput("not-a-date")).toBe("");
    expect(toLocalInput("2026-01-02T03:04:00")).toBe("2026-01-02T03:04");
    expect(toLocalInput("2026-01-02T03:04:00Z")).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
  });

  test("requires an unused, active coupon whose threshold is met", () => {
    const active = {
      status: "unused" as const,
      min_amount: 1_000,
      valid_from: "2000-01-01T00:00:00Z",
      valid_until: "2999-01-01T00:00:00Z",
    };
    expect(isCouponUsable(active, 1_000)).toBe(true);
    expect(isCouponUsable({ ...active, status: "used" }, 1_000)).toBe(false);
    expect(isCouponUsable(active, 999)).toBe(false);
    expect(isCouponUsable({ ...active, valid_from: "2999-01-01T00:00:00Z" }, 1_000)).toBe(false);
    expect(describeCouponRule("threshold", 1_000)).toContain("10.00");
    expect(describeCouponRule("fixed", 0)).toBe("无门槛直减");
  });
});

describe("media validation", () => {
  test("accepts supported images within five MiB", () => {
    expect(validateImage(new File([], "empty.png", { type: "image/png" }))).toContain("不能为空");
    expect(validateImage(new File(["image"], "photo.png", { type: "image/png" }))).toBeNull();
    expect(validateImage(new File([new Uint8Array(5 * 1024 * 1024)], "limit.png", { type: "image/png" }))).toBeNull();
    expect(validateImage(new File(["image"], "photo.svg", { type: "image/svg+xml" }), "头像")).toContain("头像仅支持");
    expect(validateImage(new File([new Uint8Array(5 * 1024 * 1024 + 1)], "large.png", { type: "image/png" }))).toContain("5MB");
  });

  test("accepts supported message file extensions case-insensitively within twenty MiB", () => {
    expect(validateMessageFile(new File([], "empty.zip"))).toContain("不能为空");
    expect(validateMessageFile(new File(["text"], "notes.MD", { type: "text/markdown" }))).toBeNull();
    expect(validateMessageFile(new File([new Uint8Array(20 * 1024 * 1024)], "limit.zip"))).toBeNull();
    expect(validateMessageFile(new File(["text"], "script.exe"))).toContain("仅支持");
    expect(validateMessageFile(new File([new Uint8Array(20 * 1024 * 1024 + 1)], "large.zip"))).toContain("20MB");
  });
});

describe("product artwork", () => {
  test("selects a stable editorial still life from the product title", () => {
    expect(resolveProductArtwork(1, "手工釉面马克杯").key).toBe("vessel");
    expect(resolveProductArtwork(2, "亚麻桌布").key).toBe("textile");
    expect(resolveProductArtwork(3, "榉木托盘").key).toBe("tray");
    expect(resolveProductArtwork(4, "黄铜随身夹").key).toBe("desk");
    expect(resolveProductArtwork(5, "帆布手提袋").key).toBe("carry");
    expect(resolveProductArtwork(6, "玻璃花瓶").key).toBe("glass");
  });

  test("falls back deterministically when a title has no visual category", () => {
    const first = resolveProductArtwork(101, "未分类商品");
    const second = resolveProductArtwork(101, "另一件未分类商品");
    expect(first.key).toBe(second.key);
    expect(first.src).toStartWith("/products/");
    expect(first.src).toEndWith(".svg");
  });
});
