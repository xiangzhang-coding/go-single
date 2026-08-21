import type { User } from "../api/types";
import { useAuthStore } from "../store/auth";
import { useChatStore } from "../store/chat";
import { sessionIdentityChanged, type StoredSession } from "./auth-storage";
import { queryClient } from "./query-client";
import { chatSocket } from "./ws";

export function endSession() {
  resetSessionResources();
  useAuthStore.getState().logout();
}

export function startSession(token: string, user: User) {
  const next = { token, user };
  if (sessionIdentityChanged(currentSession(), next)) resetSessionResources();
  useAuthStore.getState().setSession(token, user);
}

export function syncSessionFromStorage(next: StoredSession | null) {
  if (sessionIdentityChanged(currentSession(), next)) resetSessionResources();
  useAuthStore.getState().applyStoredSession(next);
}

function currentSession(): StoredSession | null {
  const { token, user } = useAuthStore.getState();
  return token && user ? { token, user } : null;
}

function resetSessionResources() {
  chatSocket.disconnect();
  queryClient.clear();
  useChatStore.getState().reset();
}
