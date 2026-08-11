import axios, { AxiosError } from "axios";

import { ACCESS_TOKEN_KEY } from "../lib/auth-storage";

export const api = axios.create({
  baseURL: import.meta.env.VITE_API_BASE || "/api",
  headers: {
    "Content-Type": "application/json",
  },
});

export class ApiRequestError extends Error {
  readonly status?: number;

  constructor(message: string, status?: number) {
    super(message);
    this.name = "ApiRequestError";
    this.status = status;
  }
}

api.interceptors.request.use((config) => {
  const token = localStorage.getItem(ACCESS_TOKEN_KEY);
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

api.interceptors.response.use(
  (response) => response,
  (error: AxiosError<{ error?: string }>) => {
    const status = error.response?.status;
    const message = error.response?.data?.error || "请求失败，请稍后再试";
    const requestError = new ApiRequestError(message, status);

    if (status === 401 && typeof window !== "undefined") {
      localStorage.removeItem(ACCESS_TOKEN_KEY);
      window.dispatchEvent(new Event("faire:session-expired"));
      if (!window.location.pathname.startsWith("/login")) {
        const returnTo = `${window.location.pathname}${window.location.search}`;
        window.location.assign(`/login?returnTo=${encodeURIComponent(returnTo)}`);
      }
    }

    return Promise.reject(requestError);
  },
);

export function getApiErrorMessage(
  error: unknown,
  fallback = "请求失败，请稍后再试",
): string {
  if (error instanceof ApiRequestError) {
    return error.message;
  }
  if (error instanceof Error && error.message) {
    return error.message;
  }
  return fallback;
}
