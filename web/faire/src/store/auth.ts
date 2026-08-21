import { create } from "zustand";

import type { User } from "../api/types";
import {
  clearStoredSession,
  readStoredSession,
  writeStoredSession,
  type StoredSession,
} from "../lib/auth-storage";

interface AuthState {
  token: string | null;
  user: User | null;
  setSession: (token: string, user: User) => void;
  applyStoredSession: (session: StoredSession | null) => void;
  setUser: (user: User) => void;
  logout: () => void;
}

const initialSession = readStoredSession();

export const useAuthStore = create<AuthState>()((set, get) => ({
  token: initialSession?.token ?? null,
  user: initialSession?.user ?? null,
  setSession: (token, user) => {
    writeStoredSession({ token, user });
    set({ token, user });
  },
  applyStoredSession: (session) => set({
    token: session?.token ?? null,
    user: session?.user ?? null,
  }),
  setUser: (user) => {
    const token = get().token;
    if (token) writeStoredSession({ token, user });
    set({ user });
  },
  logout: () => {
    clearStoredSession();
    set({ token: null, user: null });
  },
}));
