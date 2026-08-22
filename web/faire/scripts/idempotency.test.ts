import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import type { SendMessageRequest } from "../src/api/types";
import { StableRequestIDs } from "../src/lib/idempotency";
import {
  createMediaSendOperation,
  createTextSendOperation,
  executeMessageSend,
  type MessageSendOperation,
} from "../src/lib/message-send";
import { executeMediaSave, type PreparedMediaUpload } from "../src/lib/media-save";
import {
  acceptFlashSaleOperation,
  beginCheckoutOperation,
  beginFlashSaleOperation,
  clearCheckoutOperation,
  clearCheckoutOperationByOrderNo,
  clearFlashSaleOperation,
  isCheckoutOrderProcessing,
  markCheckoutOperationProcessing,
  readCheckoutOperation,
  readFlashSaleOperations,
  updateFlashSaleOperation,
} from "../src/lib/pending-operations";

class MemoryStorage implements Storage {
  private readonly values = new Map<string, string>();

  get length() {
    return this.values.size;
  }

  clear() {
    this.values.clear();
  }

  getItem(key: string) {
    return this.values.get(key) ?? null;
  }

  key(index: number) {
    return [...this.values.keys()][index] ?? null;
  }

  removeItem(key: string) {
    this.values.delete(key);
  }

  setItem(key: string, value: string) {
    this.values.set(key, value);
  }
}

type StorageFault = "getItem" | "setItem" | "removeItem" | "key" | "length";

class ThrowingStorage implements Storage {
  constructor(private readonly fault: StorageFault) {}

  get length() {
    this.fail("length");
    return 1;
  }

  clear() {}

  getItem() {
    this.fail("getItem");
    return null;
  }

  key() {
    this.fail("key");
    return "faire-pending:flashsale:7:42";
  }

  removeItem() {
    this.fail("removeItem");
  }

  setItem() {
    this.fail("setItem");
  }

  private fail(member: StorageFault) {
    if (this.fault === member) throw new DOMException(`${member} unavailable`);
  }
}

function useStorage(storage: Storage) {
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { sessionStorage: storage },
  });
}

beforeEach(() => {
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { sessionStorage: new MemoryStorage() },
  });
});

afterEach(() => {
  Reflect.deleteProperty(globalThis, "window");
});

describe("stable client request ids", () => {
  test("checkout retries survive reloads within one user and business intent", () => {
    let sequence = 0;
    const cart = { kind: "cart" } as const;
    const direct = { kind: "direct", productId: 3, skuId: 9, quantity: 2 } as const;

    const first = beginCheckoutOperation(1, cart, () => `checkout-${++sequence}`);
    expect(beginCheckoutOperation(1, cart, () => `checkout-${++sequence}`)).toEqual(first);
    expect(beginCheckoutOperation(2, cart, () => `checkout-${++sequence}`).clientRequestId).toBe("checkout-2");
    expect(beginCheckoutOperation(1, direct, () => `checkout-${++sequence}`).clientRequestId).toBe("checkout-3");

    clearCheckoutOperation(1, cart);
    expect(readCheckoutOperation(1, cart)).toBeNull();
    expect(beginCheckoutOperation(1, cart, () => `checkout-${++sequence}`).clientRequestId).toBe("checkout-4");
  });

  test("a processing checkout is recoverable by order number until the order can be read", () => {
    const cart = { kind: "cart" } as const;
    beginCheckoutOperation(1, cart, () => "checkout-1");

    markCheckoutOperationProcessing(1, cart, "order-1");

    expect(isCheckoutOrderProcessing(1, "order-1")).toBe(true);
    expect(isCheckoutOrderProcessing(2, "order-1")).toBe(false);
    clearCheckoutOperationByOrderNo(1, "order-1");
    expect(isCheckoutOrderProcessing(1, "order-1")).toBe(false);
  });

  test("flash-sale submission and polling state survive reloads until a terminal result", () => {
    const submitting = beginFlashSaleOperation(7, 42, () => "seckill-1");
    expect(submitting).toEqual({
      activityId: 42,
      clientRequestId: "seckill-1",
      phase: "submitting",
      attempts: 0,
    });
    expect(beginFlashSaleOperation(7, 42, () => "seckill-2")).toEqual(submitting);

    const queued = acceptFlashSaleOperation(7, 42, {
      preDeductionId: "pre-42",
      orderNo: "order-42",
    });
    expect(readFlashSaleOperations(7)[42]).toEqual({
      ...queued,
      preDeductionId: "pre-42",
      orderNo: "order-42",
      phase: "queued",
    });

    updateFlashSaleOperation(7, 42, { phase: "timeout", attempts: 30 });
    expect(readFlashSaleOperations(7)[42]?.attempts).toBe(30);
    expect(readFlashSaleOperations(8)[42]).toBeUndefined();

    clearFlashSaleOperation(7, 42);
    expect(readFlashSaleOperations(7)[42]).toBeUndefined();
    expect(beginFlashSaleOperation(7, 42, () => "seckill-2").clientRequestId).toBe("seckill-2");
    clearFlashSaleOperation(7, 42);
  });

  test("session storage DOMExceptions never block pending operations", () => {
    const cart = { kind: "cart" } as const;

    useStorage(new ThrowingStorage("getItem"));
    expect(readCheckoutOperation(1, cart)).toBeNull();

    useStorage(new ThrowingStorage("setItem"));
    const checkout = beginCheckoutOperation(1, cart, () => "checkout-memory");
    expect(checkout.clientRequestId).toBe("checkout-memory");
    const processing = markCheckoutOperationProcessing(1, cart, "order-1", checkout);
    expect(processing.orderNo).toBe("order-1");
    expect(readCheckoutOperation(1, cart)).toEqual(processing);
    const submitting = beginFlashSaleOperation(7, 42, () => "seckill-memory");
    const queued = acceptFlashSaleOperation(7, 42, { preDeductionId: "pre-42" }, submitting);
    const polling = updateFlashSaleOperation(7, 42, { attempts: 1 }, queued);
    expect(polling.clientRequestId).toBe("seckill-memory");
    expect(readFlashSaleOperations(7)[42]).toEqual(polling);

    useStorage(new ThrowingStorage("removeItem"));
    expect(() => clearCheckoutOperation(1, cart)).not.toThrow();
    expect(() => clearFlashSaleOperation(7, 42)).not.toThrow();

    useStorage(new ThrowingStorage("key"));
    expect(readFlashSaleOperations(7)).toEqual({});

    useStorage(new ThrowingStorage("length"));
    expect(readFlashSaleOperations(7)).toEqual({});
    expect(isCheckoutOrderProcessing(1, "order-1")).toBe(false);
  });

  test("a blocked sessionStorage getter is treated as unavailable", () => {
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: Object.defineProperty({}, "sessionStorage", {
        get() {
          throw new DOMException("session storage unavailable");
        },
      }),
    });

    expect(beginFlashSaleOperation(7, 42, () => "seckill-memory").clientRequestId).toBe("seckill-memory");
    expect(readFlashSaleOperations(7)[42]?.clientRequestId).toBe("seckill-memory");
    clearFlashSaleOperation(7, 42);
  });

  test("memory state wins when storage becomes stale or cannot remove a terminal operation", () => {
    const storage = new MemoryStorage();
    useStorage(storage);
    const submitting = beginFlashSaleOperation(7, 42, () => "seckill-1");

    storage.setItem = () => {
      throw new DOMException("quota exceeded");
    };
    const queued = acceptFlashSaleOperation(7, 42, { preDeductionId: "pre-42" }, submitting);
    expect(readFlashSaleOperations(7)[42]).toEqual(queued);

    storage.removeItem = () => {
      throw new DOMException("storage unavailable");
    };
    clearFlashSaleOperation(7, 42);
    expect(beginFlashSaleOperation(7, 42, () => "seckill-2").clientRequestId).toBe("seckill-2");
    clearFlashSaleOperation(7, 42);
  });

  test("retries reuse an operation id until it completes", () => {
    let sequence = 0;
    const ids = new StableRequestIDs(() => `request-${++sequence}`);

    expect(ids.forOperation("flashsale:7")).toBe("request-1");
    expect(ids.forOperation("flashsale:7")).toBe("request-1");

    ids.complete("flashsale:7");
    expect(ids.forOperation("flashsale:7")).toBe("request-2");
  });

  test("a failed media send keeps its id and uploaded reference for retry", async () => {
    const file = new File(["image"], "photo.png", { type: "image/png" });
    const initial = createMediaSendOperation("image", file, () => "message-1");
    let pending: MessageSendOperation = initial;
    let uploads = 0;
    const requests: SendMessageRequest[] = [];

    const upload = async () => {
      uploads += 1;
      return { url: "/files/reference-1" };
    };
    const failSend = async (request: SendMessageRequest) => {
      requests.push(request);
      throw new Error("unknown network result");
    };

    await expect(executeMessageSend(pending, 2, upload, failSend, (prepared) => {
      pending = prepared;
    })).rejects.toThrow("unknown network result");
    await expect(executeMessageSend(pending, 2, upload, failSend, (prepared) => {
      pending = prepared;
    })).rejects.toThrow("unknown network result");

    expect(uploads).toBe(1);
    expect(requests).toEqual([
      { to_user_id: 2, type: "image", url: "/files/reference-1", client_request_id: "message-1" },
      { to_user_id: 2, type: "image", url: "/files/reference-1", client_request_id: "message-1" },
    ]);
  });

  test("an upload failure retries the same media operation id", async () => {
    const file = new File(["image"], "photo.png", { type: "image/png" });
    const pending = createMediaSendOperation("image", file, () => "message-1");
    let uploads = 0;
    const upload = async () => {
      uploads += 1;
      if (uploads === 1) throw new Error("upload failed");
      return { url: "/files/reference-1" };
    };
    const requests: SendMessageRequest[] = [];

    await expect(executeMessageSend(pending, 2, upload, async (request) => {
      requests.push(request);
      return "sent";
    }, () => undefined)).rejects.toThrow("upload failed");
    await executeMessageSend(pending, 2, upload, async (request) => {
      requests.push(request);
      return "sent";
    }, () => undefined);

    expect(requests).toEqual([
      { to_user_id: 2, type: "image", url: "/files/reference-1", client_request_id: "message-1" },
    ]);
  });

  test("a successful send allows the next operation to get a new id", () => {
    let sequence = 0;
    const makeID = () => `message-${++sequence}`;

    expect(createTextSendOperation("first", makeID).clientRequestId).toBe("message-1");
    expect(createTextSendOperation("second", makeID).clientRequestId).toBe("message-2");
  });

  test("a failed save reuses the uploaded media reference", async () => {
    const file = new File(["image"], "photo.png", { type: "image/png" });
    let prepared: PreparedMediaUpload | null = null;
    let uploads = 0;
    const references: Array<string | undefined> = [];
    const upload = async () => {
      uploads += 1;
      return { url: "/files/reference-1" };
    };
    const failSave = async (reference?: string) => {
      references.push(reference);
      throw new Error("profile save failed");
    };

    await expect(executeMediaSave(file, prepared, upload, failSave, (next) => {
      prepared = next;
    })).rejects.toThrow("profile save failed");
    await expect(executeMediaSave(file, prepared, upload, failSave, (next) => {
      prepared = next;
    })).rejects.toThrow("profile save failed");

    expect(uploads).toBe(1);
    expect(references).toEqual(["/files/reference-1", "/files/reference-1"]);
  });
});
