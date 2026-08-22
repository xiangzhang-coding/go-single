import type { CheckoutIntent } from "./checkout";
import { makeClientRequestID } from "./format";

const OPERATION_PREFIX = "faire-pending";
const memoryOperations = new Map<string, string>();
const removedOperations = new Set<string>();

export interface PendingCheckoutOperation {
  clientRequestId: string;
  orderNo?: string;
  status: "submitting" | "processing";
}

export interface PendingFlashSaleOperation {
  activityId: number;
  clientRequestId: string;
  preDeductionId?: string;
  orderNo?: string;
  phase: "submitting" | "queued" | "timeout";
  attempts: number;
}

export function beginCheckoutOperation(
  userId: number,
  intent: CheckoutIntent,
  generate: () => string = makeClientRequestID,
): PendingCheckoutOperation {
  const current = readCheckoutOperation(userId, intent);
  if (current) return current;
  const operation: PendingCheckoutOperation = {
    clientRequestId: generate(),
    status: "submitting",
  };
  writeOperation(checkoutKey(userId, intent), operation);
  return operation;
}

export function readCheckoutOperation(
  userId: number,
  intent: CheckoutIntent,
): PendingCheckoutOperation | null {
  const value = readOperation(checkoutKey(userId, intent));
  if (!isRecord(value) || typeof value.clientRequestId !== "string") return null;
  if (value.status !== "submitting" && value.status !== "processing") return null;
  if (value.orderNo !== undefined && typeof value.orderNo !== "string") return null;
  return value as unknown as PendingCheckoutOperation;
}

export function clearCheckoutOperation(userId: number, intent: CheckoutIntent) {
  removeOperation(checkoutKey(userId, intent));
}

export function markCheckoutOperationProcessing(
  userId: number,
  intent: CheckoutIntent,
  orderNo: string,
  operation?: PendingCheckoutOperation,
): PendingCheckoutOperation {
  const current = operation ?? readCheckoutOperation(userId, intent);
  if (!current) throw new Error("下单状态不存在");
  const next: PendingCheckoutOperation = { ...current, orderNo, status: "processing" };
  writeOperation(checkoutKey(userId, intent), next);
  return next;
}

export function isCheckoutOrderProcessing(userId: number, orderNo: string): boolean {
  return findCheckoutOperation(userId, orderNo) !== null;
}

export function clearCheckoutOperationByOrderNo(userId: number, orderNo: string) {
  const match = findCheckoutOperation(userId, orderNo);
  if (match) removeOperation(match.key);
}

export function beginFlashSaleOperation(
  userId: number,
  activityId: number,
  generate: () => string = makeClientRequestID,
): PendingFlashSaleOperation {
  const current = readFlashSaleOperation(userId, activityId);
  if (current) return current;
  const operation: PendingFlashSaleOperation = {
    activityId,
    clientRequestId: generate(),
    phase: "submitting",
    attempts: 0,
  };
  writeOperation(flashSaleKey(userId, activityId), operation);
  return operation;
}

export function acceptFlashSaleOperation(
  userId: number,
  activityId: number,
  result: { preDeductionId: string; orderNo?: string },
  operation?: PendingFlashSaleOperation,
): PendingFlashSaleOperation {
  const current = operation ?? readFlashSaleOperation(userId, activityId);
  if (!current) throw new Error("秒杀提交状态不存在");
  return updateFlashSaleOperation(userId, activityId, {
    preDeductionId: result.preDeductionId,
    ...(result.orderNo ? { orderNo: result.orderNo } : {}),
    phase: "queued",
    attempts: 0,
  }, current);
}

export function updateFlashSaleOperation(
  userId: number,
  activityId: number,
  patch: Partial<Pick<PendingFlashSaleOperation, "preDeductionId" | "orderNo" | "phase" | "attempts">>,
  operation?: PendingFlashSaleOperation,
): PendingFlashSaleOperation {
  const current = operation ?? readFlashSaleOperation(userId, activityId);
  if (!current) throw new Error("秒杀提交状态不存在");
  const next = { ...current, ...patch };
  writeOperation(flashSaleKey(userId, activityId), next);
  return next;
}

export function readFlashSaleOperations(userId: number): Record<number, PendingFlashSaleOperation> {
  const operations: Record<number, PendingFlashSaleOperation> = {};
  const prefix = `${OPERATION_PREFIX}:flashsale:${userId}:`;
  for (const key of operationKeys(prefix)) {
    const activityId = Number(key.slice(prefix.length));
    const operation = readFlashSaleOperation(userId, activityId);
    if (operation) operations[activityId] = operation;
  }
  return operations;
}

export function clearFlashSaleOperation(userId: number, activityId: number) {
  removeOperation(flashSaleKey(userId, activityId));
}

function checkoutKey(userId: number, intent: CheckoutIntent): string {
  const businessIntent = intent.kind === "cart"
    ? "cart"
    : `direct:${intent.productId}:${intent.skuId}:${intent.quantity}`;
  return `${OPERATION_PREFIX}:checkout:${userId}:${businessIntent}`;
}

function findCheckoutOperation(
  userId: number,
  orderNo: string,
): { key: string; operation: PendingCheckoutOperation } | null {
  const prefix = `${OPERATION_PREFIX}:checkout:${userId}:`;
  for (const key of operationKeys(prefix)) {
    const value = readOperation(key);
    if (
      isRecord(value)
      && value.orderNo === orderNo
      && value.status === "processing"
      && typeof value.clientRequestId === "string"
    ) {
      return { key, operation: value as unknown as PendingCheckoutOperation };
    }
  }
  return null;
}

function flashSaleKey(userId: number, activityId: number): string {
  return `${OPERATION_PREFIX}:flashsale:${userId}:${activityId}`;
}

function readFlashSaleOperation(userId: number, activityId: number): PendingFlashSaleOperation | null {
  const value = readOperation(flashSaleKey(userId, activityId));
  if (!isRecord(value)) return null;
  if (value.activityId !== activityId || typeof value.clientRequestId !== "string") return null;
  if (value.phase !== "submitting" && value.phase !== "queued" && value.phase !== "timeout") return null;
  if (!Number.isSafeInteger(value.attempts) || (value.attempts as number) < 0) return null;
  if (value.preDeductionId !== undefined && typeof value.preDeductionId !== "string") return null;
  if (value.orderNo !== undefined && typeof value.orderNo !== "string") return null;
  return value as unknown as PendingFlashSaleOperation;
}

function readOperation(key: string): unknown {
  if (removedOperations.has(key)) return null;
  let value: string | null = memoryOperations.get(key) ?? null;
  if (value) return parseOperation(value);

  const storage = browserSessionStorage();
  if (storage) {
    try {
      value = storage.getItem(key);
    } catch {
      value = null;
    }
  }
  if (!value) return null;
  memoryOperations.set(key, value);
  return parseOperation(value);
}

function parseOperation(value: string): unknown {
  try {
    return JSON.parse(value) as unknown;
  } catch {
    return null;
  }
}

function writeOperation(key: string, operation: unknown) {
  const value = JSON.stringify(operation);
  removedOperations.delete(key);
  memoryOperations.set(key, value);
  const storage = browserSessionStorage();
  if (!storage) return;
  try {
    storage.setItem(key, value);
  } catch {
    // The module-level mirror keeps the operation stable for this page load.
  }
}

function removeOperation(key: string) {
  memoryOperations.delete(key);
  removedOperations.add(key);
  const storage = browserSessionStorage();
  if (!storage) return;
  try {
    storage.removeItem(key);
  } catch {
    // Storage can be disabled independently of the sessionStorage getter.
  }
}

function operationKeys(prefix: string): Set<string> {
  const keys = new Set<string>();
  const storage = browserSessionStorage();
  if (storage) {
    const length = storageLength(storage);
    for (let index = 0; index < length; index += 1) {
      const key = storageKey(storage, index);
      if (key?.startsWith(prefix) && !removedOperations.has(key)) keys.add(key);
    }
  }
  for (const key of memoryOperations.keys()) {
    if (key.startsWith(prefix)) keys.add(key);
  }
  return keys;
}

function storageLength(storage: Storage): number {
  try {
    return storage.length;
  } catch {
    return 0;
  }
}

function storageKey(storage: Storage, index: number): string | null {
  try {
    return storage.key(index);
  } catch {
    return null;
  }
}

function browserSessionStorage(): Storage | null {
  if (typeof window === "undefined") return null;
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
