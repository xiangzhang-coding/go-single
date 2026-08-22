import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { AxiosError } from "axios";

import { api, ApiRequestError, getApiErrorMessage } from "../src/api/client";
import { uploadFile } from "../src/api/endpoints";
import type { User } from "../src/api/types";
import { useAuthStore } from "../src/store/auth";

const user = (id: number, username: string): User => ({
  id,
  username,
  nickname: username,
  avatar_url: "",
  role: "user",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
});

const originalAdapter = api.defaults.adapter;

beforeEach(() => {
  useAuthStore.setState({ token: null, user: null });
});

afterEach(() => {
  api.defaults.adapter = originalAdapter;
  useAuthStore.setState({ token: null, user: null });
  Reflect.deleteProperty(globalThis, "window");
});

describe("API client session boundaries", () => {
  test("attaches the current bearer token to requests", async () => {
    let authorization: unknown;
    api.defaults.adapter = async (config) => {
      authorization = config.headers.get("Authorization");
      return { data: { ok: true }, status: 200, statusText: "OK", headers: {}, config };
    };
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });

    await api.get("/probe");

    expect(authorization).toBe("Bearer alice-token");
  });

  test("rejects a late response after the signed-in account changes", async () => {
    api.defaults.adapter = async (config) => {
      useAuthStore.setState({ token: "bob-token", user: user(2, "bob") });
      return { data: { owner: "alice" }, status: 200, statusText: "OK", headers: {}, config };
    };
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });

    await expect(api.get("/slow-profile")).rejects.toMatchObject({
      name: "ApiRequestError",
      message: "登录账号已切换",
    });
  });

  test("a 401 for the current token ends the session and preserves the return path", async () => {
    let redirect = "";
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: {
        location: {
          pathname: "/orders/42",
          search: "?tab=payment",
          assign: (value: string) => {
            redirect = value;
          },
        },
      },
    });
    api.defaults.adapter = async (config) => {
      throw new AxiosError("unauthorized", "ERR_BAD_REQUEST", config, undefined, {
        data: { error: "token expired" },
        status: 401,
        statusText: "Unauthorized",
        headers: {},
        config,
      });
    };
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });

    await expect(api.get("/orders/42")).rejects.toMatchObject({
      name: "ApiRequestError",
      message: "token expired",
      status: 401,
    });
    expect(useAuthStore.getState().token).toBeNull();
    expect(redirect).toBe("/login?returnTo=%2Forders%2F42%3Ftab%3Dpayment");
  });

  test("a late 401 from the previous account cannot log out the current account", async () => {
    let redirect = "";
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: {
        location: {
          pathname: "/chat",
          search: "",
          assign: (value: string) => {
            redirect = value;
          },
        },
      },
    });
    api.defaults.adapter = async (config) => {
      useAuthStore.setState({ token: "bob-token", user: user(2, "bob") });
      throw new AxiosError("unauthorized", "ERR_BAD_REQUEST", config, undefined, {
        data: { error: "old token expired" },
        status: 401,
        statusText: "Unauthorized",
        headers: {},
        config,
      });
    };
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });

    await expect(api.get("/slow-request")).rejects.toMatchObject({
      message: "old token expired",
      status: 401,
    });
    expect(useAuthStore.getState().token).toBe("bob-token");
    expect(useAuthStore.getState().user?.username).toBe("bob");
    expect(redirect).toBe("");
  });

  test("upload retries reuse their idempotency key until a successful response", async () => {
    const requestIDs: string[] = [];
    let attempt = 0;
    api.defaults.adapter = async (config) => {
      requestIDs.push(String(config.headers.get("Idempotency-Key")));
      attempt += 1;
      if (attempt === 1) throw new AxiosError("network result unknown", "ERR_NETWORK", config);
      return {
        data: { url: "/files/ref", kind: "image", filename: "photo.png", content_type: "image/png", size: 5 },
        status: attempt === 2 ? 200 : 201,
        statusText: "OK",
        headers: {},
        config,
      };
    };
    const file = new File(["image"], "photo.png", { type: "image/png" });

    await expect(uploadFile(file)).rejects.toBeInstanceOf(ApiRequestError);
    await uploadFile(file);
    await uploadFile(file);

    expect(requestIDs[0]).toBeTruthy();
    expect(requestIDs[1]).toBe(requestIDs[0]);
    expect(requestIDs[2]).not.toBe(requestIDs[1]);
  });

  test("normalizes known, native, and unknown errors for page messages", () => {
    expect(getApiErrorMessage(new ApiRequestError("conflict", 409))).toBe("conflict");
    expect(getApiErrorMessage(new Error("network down"))).toBe("network down");
    expect(getApiErrorMessage(null, "fallback")).toBe("fallback");
  });
});
