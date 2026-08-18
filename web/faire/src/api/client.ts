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
  async (error: AxiosError<{ error?: string } | Blob>) => {
    const status = error.response?.status;
    const message = await responseErrorMessage(error.response?.data);
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

async function responseErrorMessage(data: { error?: string } | Blob | undefined): Promise<string> {
  if (data instanceof Blob) {
    try {
      const parsed = JSON.parse(await data.text()) as { error?: string };
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
