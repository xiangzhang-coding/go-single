import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import {
  AUTH_SESSION_KEY,
  clearStoredSession,
  parseStoredSession,
  readStoredSession,
  sessionIdentityChanged,
  type StoredSession,
  writeStoredSession,
} from "../src/lib/auth-storage";

const alice: StoredSession = {
  token: "alice-token",
  user: {
    id: 1,
    username: "alice",
    nickname: "Alice",
    avatar_url: "",
    role: "user",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
};

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

let storage: MemoryStorage;

beforeEach(() => {
  storage = new MemoryStorage();
  Object.defineProperty(globalThis, "window", {
    value: { localStorage: storage },
    configurable: true,
  });
});

afterEach(() => {
  Reflect.deleteProperty(globalThis, "window");
});

describe("auth session storage", () => {
  test("reads the canonical session document", () => {
    expect(parseStoredSession(JSON.stringify(alice))).toEqual(alice);
  });

  test("migrates the previous Zustand persist document without a second token key", () => {
    expect(parseStoredSession(JSON.stringify({ state: alice, version: 0 }))).toEqual(alice);
  });

  test("rejects malformed JSON and incomplete or unsafe session shapes", () => {
    expect(parseStoredSession("{")).toBeNull();
    expect(parseStoredSession(JSON.stringify({ token: "", user: alice.user }))).toBeNull();
    expect(parseStoredSession(JSON.stringify({ token: "token", user: { ...alice.user, role: "root" } }))).toBeNull();
    expect(parseStoredSession(JSON.stringify({ token: "token", user: [] }))).toBeNull();
  });

  test("writes, reads, and clears only the canonical session document", () => {
    storage.setItem("faire-access-token", "legacy-token");
    writeStoredSession(alice);
    expect(storage.getItem("faire-access-token")).toBeNull();
    expect(JSON.parse(storage.getItem(AUTH_SESSION_KEY)!)).toEqual(alice);
    expect(readStoredSession()).toEqual(alice);

    clearStoredSession();
    expect(storage.getItem(AUTH_SESSION_KEY)).toBeNull();
  });

  test("only token changes replace the session identity", () => {
    expect(sessionIdentityChanged(alice, { ...alice, user: { ...alice.user, nickname: "A" } })).toBe(false);
    expect(sessionIdentityChanged(alice, { ...alice, token: "other-token" })).toBe(true);
    expect(sessionIdentityChanged(alice, null)).toBe(true);
    expect(sessionIdentityChanged(null, null)).toBe(false);
  });
});
