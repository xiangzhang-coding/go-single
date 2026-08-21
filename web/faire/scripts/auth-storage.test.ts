import { describe, expect, test } from "bun:test";

import {
  parseStoredSession,
  sessionIdentityChanged,
  type StoredSession,
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

describe("auth session storage", () => {
  test("reads the canonical session document", () => {
    expect(parseStoredSession(JSON.stringify(alice))).toEqual(alice);
  });

  test("migrates the previous Zustand persist document without a second token key", () => {
    expect(parseStoredSession(JSON.stringify({ state: alice, version: 0 }))).toEqual(alice);
  });

  test("only token changes replace the session identity", () => {
    expect(sessionIdentityChanged(alice, { ...alice, user: { ...alice.user, nickname: "A" } })).toBe(false);
    expect(sessionIdentityChanged(alice, { ...alice, token: "other-token" })).toBe(true);
    expect(sessionIdentityChanged(alice, null)).toBe(true);
    expect(sessionIdentityChanged(null, null)).toBe(false);
  });
});
