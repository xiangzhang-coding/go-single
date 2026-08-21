import type { User } from "../api/types";

export const AUTH_SESSION_KEY = "faire-auth";

const LEGACY_ACCESS_TOKEN_KEY = "faire-access-token";

export interface StoredSession {
  token: string;
  user: User;
}

export function parseStoredSession(value: string | null): StoredSession | null {
  if (!value) return null;
  try {
    const parsed: unknown = JSON.parse(value);
    if (isStoredSession(parsed)) return parsed;
    if (isRecord(parsed) && isStoredSession(parsed.state)) return parsed.state;
    return null;
  } catch {
    return null;
  }
}

export function readStoredSession(): StoredSession | null {
  const storage = browserStorage();
  if (!storage) return null;
  storage.removeItem(LEGACY_ACCESS_TOKEN_KEY);
  return parseStoredSession(storage.getItem(AUTH_SESSION_KEY));
}

export function writeStoredSession(session: StoredSession) {
  const storage = browserStorage();
  if (!storage) return;
  storage.removeItem(LEGACY_ACCESS_TOKEN_KEY);
  storage.setItem(AUTH_SESSION_KEY, JSON.stringify(session));
}

export function clearStoredSession() {
  const storage = browserStorage();
  if (!storage) return;
  storage.removeItem(LEGACY_ACCESS_TOKEN_KEY);
  storage.removeItem(AUTH_SESSION_KEY);
}

export function sessionIdentityChanged(
  current: StoredSession | null,
  next: StoredSession | null,
): boolean {
  return current?.token !== next?.token;
}

function browserStorage(): Storage | null {
  if (typeof window === "undefined") return null;
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

function isStoredSession(value: unknown): value is StoredSession {
  if (!isRecord(value) || typeof value.token !== "string" || !value.token) return false;
  const user = value.user;
  return (
    isRecord(user) &&
    typeof user.id === "number" &&
    typeof user.username === "string" &&
    typeof user.nickname === "string" &&
    typeof user.avatar_url === "string" &&
    (user.role === "user" || user.role === "admin") &&
    typeof user.created_at === "string" &&
    typeof user.updated_at === "string"
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
