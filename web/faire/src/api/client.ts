import axios, { AxiosError } from "axios";

import { endSession } from "../lib/session";
import { currentSessionRequestSignal } from "../lib/session-request";
import { useAuthStore } from "../store/auth";
import type { ErrorResponse } from "./types";

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
  const token = useAuthStore.getState().token;
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
    const sessionSignal = currentSessionRequestSignal();
    config.signal = config.signal
      ? AbortSignal.any([config.signal as AbortSignal, sessionSignal])
      : sessionSignal;
  }
  return config;
});

api.interceptors.response.use(
  (response) => {
    if (!responseBelongsToCurrentSession(response.config.headers.get("Authorization"))) {
      return Promise.reject(new ApiRequestError("登录账号已切换"));
    }
    return response;
  },
  async (error: AxiosError<ErrorResponse | Blob>) => {
    const status = error.response?.status;
    const message = await responseErrorMessage(error.response?.data);
    const requestError = new ApiRequestError(message, status);

    const requestToken = bearerToken(error.config?.headers.get("Authorization"));
    const currentToken = useAuthStore.getState().token;
    const expiresCurrentSession = requestToken === currentToken;
    if (status === 401 && typeof window !== "undefined" && expiresCurrentSession) {
      endSession();
      if (!window.location.pathname.startsWith("/login")) {
        const returnTo = `${window.location.pathname}${window.location.search}`;
        window.location.assign(`/login?returnTo=${encodeURIComponent(returnTo)}`);
      }
    }

    return Promise.reject(requestError);
  },
);

function bearerToken(value: unknown): string | null {
  if (typeof value !== "string" || !value.startsWith("Bearer ")) return null;
  return value.slice("Bearer ".length);
}

function responseBelongsToCurrentSession(authorization: unknown): boolean {
  const requestToken = bearerToken(authorization);
  if (requestToken === null) return true;
  const currentToken = useAuthStore.getState().token;
  return requestToken === currentToken;
}

async function responseErrorMessage(data: ErrorResponse | Blob | undefined): Promise<string> {
  if (data instanceof Blob) {
    try {
      const parsed = JSON.parse(await data.text()) as Partial<ErrorResponse>;
      return parsed.error || "请求失败，请稍后再试";
    } catch {
      return "请求失败，请稍后再试";
    }
  }
  return data?.error || "请求失败，请稍后再试";
}

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
