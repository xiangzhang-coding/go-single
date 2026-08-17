import { create } from "zustand";
import { persist } from "zustand/middleware";

import type { User } from "../api/types";
import { ACCESS_TOKEN_KEY } from "../lib/auth-storage";

interface AuthState {
  token: string | null;
  user: User | null;
  setSession: (token: string, user: User) => void;
  setUser: (user: User) => void;
  logout: () => void;
}

export const useAuthStore = create<AuthState>()(
  persist(
    (set) => ({
      token: null,
      user: null,
      setSession: (token, user) => {
        localStorage.setItem(ACCESS_TOKEN_KEY, token);
        set({ token, user });
      },
      setUser: (user) => set({ user }),
      logout: () => {
        localStorage.removeItem(ACCESS_TOKEN_KEY);
        set({ token: null, user: null });
      },
    }),
    {
      name: "faire-auth",
      partialize: (state) => ({ token: state.token, user: state.user }),
    },
  ),
);
