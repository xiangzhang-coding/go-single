import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { chatSocket, type ChatSocketEvent } from "../src/lib/ws";

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  closeCalls = 0;

  constructor(readonly url: string, readonly protocols: string[]) {
    FakeWebSocket.instances.push(this);
  }

  close() {
    this.closeCalls += 1;
  }
}

let browserWindow: EventTarget;
let originalWindow: PropertyDescriptor | undefined;
let originalWebSocket: PropertyDescriptor | undefined;
let originalSetTimeout: PropertyDescriptor | undefined;
let originalClearTimeout: PropertyDescriptor | undefined;

beforeEach(() => {
  originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
  originalWebSocket = Object.getOwnPropertyDescriptor(globalThis, "WebSocket");
  originalSetTimeout = Object.getOwnPropertyDescriptor(globalThis, "setTimeout");
  originalClearTimeout = Object.getOwnPropertyDescriptor(globalThis, "clearTimeout");
  browserWindow = new EventTarget();
  Object.defineProperty(globalThis, "window", { value: browserWindow, configurable: true });
  Object.defineProperty(globalThis, "WebSocket", { value: FakeWebSocket, configurable: true });
  FakeWebSocket.instances = [];
});

afterEach(() => {
  chatSocket.disconnect();
  if (originalWebSocket) Object.defineProperty(globalThis, "WebSocket", originalWebSocket);
  else Reflect.deleteProperty(globalThis, "WebSocket");
  if (originalWindow) Object.defineProperty(globalThis, "window", originalWindow);
  else Reflect.deleteProperty(globalThis, "window");
  if (originalSetTimeout) Object.defineProperty(globalThis, "setTimeout", originalSetTimeout);
  if (originalClearTimeout) Object.defineProperty(globalThis, "clearTimeout", originalClearTimeout);
  FakeWebSocket.instances = [];
});

const message = {
  id: 7,
  conversation_key: "1:2",
  sender_id: 2,
  recipient_id: 1,
  type: "text" as const,
  content: "hello",
  url: "",
  created_at: "2026-08-22T00:00:00Z",
};

describe("chat socket", () => {
  test("uses the bearer subprotocol, deduplicates a live token, and distributes valid messages", () => {
    const statuses: string[] = [];
    const events: ChatSocketEvent[] = [];
    const stopStatus = chatSocket.onStatusChange((status) => statuses.push(status));
    const stopMessages = chatSocket.subscribe((event) => events.push(event));

    chatSocket.connect("jwt-token");
    expect(FakeWebSocket.instances).toHaveLength(1);
    const socket = FakeWebSocket.instances[0]!;
    expect(socket.url).toBe("/ws");
    expect(socket.protocols).toEqual(["bearer", "jwt-token"]);
    expect(statuses).toEqual(["idle", "connecting"]);

    chatSocket.connect("jwt-token");
    expect(FakeWebSocket.instances).toHaveLength(1);
    socket.onopen?.(new Event("open"));
    socket.onmessage?.({ data: "not-json" } as MessageEvent<string>);
    socket.onmessage?.({ data: JSON.stringify({ event: "ignored", data: message }) } as MessageEvent<string>);
    socket.onmessage?.({ data: JSON.stringify({ event: "new_message", data: message }) } as MessageEvent<string>);
    expect(statuses.at(-1)).toBe("open");
    expect(events).toEqual([{ event: "new_message", data: message }]);

    chatSocket.disconnect();
    expect(socket.closeCalls).toBe(1);
    expect(statuses.at(-1)).toBe("idle");
    stopMessages();
    stopStatus();
  });

  test("dispatches session expiration without scheduling a reconnect", async () => {
    let expired = 0;
    browserWindow.addEventListener("faire:session-expired", () => {
      expired += 1;
    });
    const statuses: string[] = [];
    const stopStatus = chatSocket.onStatusChange((status) => statuses.push(status));

    chatSocket.connect("expired-token");
    const socket = FakeWebSocket.instances[0]!;
    socket.onclose?.({ code: 4001 } as CloseEvent);
    await Bun.sleep(1_050);

    expect(expired).toBe(1);
    expect(statuses.at(-1)).toBe("closed");
    expect(FakeWebSocket.instances).toHaveLength(1);
    stopStatus();
  });

  test("reconnects after an unexpected close and stops retries on disconnect", async () => {
    chatSocket.connect("retry-token");
    const first = FakeWebSocket.instances[0]!;
    first.onclose?.({ code: 1006 } as CloseEvent);
    await Bun.sleep(1_050);

    expect(FakeWebSocket.instances).toHaveLength(2);
    chatSocket.disconnect();
    const second = FakeWebSocket.instances[1]!;
    expect(second.closeCalls).toBe(1);
  });

  test("disconnect cancels a pending reconnect before the old token is reused", () => {
    let scheduled: (() => void) | null = null;
    let cleared = false;
    const timer = { id: 1 };
    Object.defineProperty(globalThis, "setTimeout", {
      configurable: true,
      value: (handler: () => void) => {
        scheduled = handler;
        return timer;
      },
    });
    Object.defineProperty(globalThis, "clearTimeout", {
      configurable: true,
      value: (candidate: unknown) => {
        if (candidate === timer) cleared = true;
      },
    });
    chatSocket.connect("old-token");
    const first = FakeWebSocket.instances[0]!;
    first.onclose?.({ code: 1006 } as CloseEvent);
    chatSocket.disconnect();

    expect(cleared).toBe(true);
    scheduled?.();
    expect(FakeWebSocket.instances).toHaveLength(1);
  });
});
