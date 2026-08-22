import { afterEach, describe, expect, test } from "bun:test";

import type { User } from "../src/api/types";
import { queryClient } from "../src/lib/query-client";
import { startSession, syncSessionFromStorage } from "../src/lib/session";
import { currentSessionRequestSignal } from "../src/lib/session-request";
import { useAuthStore } from "../src/store/auth";
import { useChatStore } from "../src/store/chat";

const user = (id: number, username: string): User => ({
  id,
  username,
  nickname: username,
  avatar_url: "",
  role: "user",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
});

afterEach(() => {
  queryClient.clear();
  useChatStore.getState().reset();
  useAuthStore.setState({ token: null, user: null });
});

describe("session resources", () => {
  test("switching identities aborts old requests and starts a fresh request generation", () => {
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });
    const aliceSignal = currentSessionRequestSignal();

    startSession("bob-token", user(2, "bob"));

    expect(aliceSignal.aborted).toBe(true);
    expect(currentSessionRequestSignal()).not.toBe(aliceSignal);
    expect(currentSessionRequestSignal().aborted).toBe(false);
  });

  test("switching accounts clears server cache and chat state before applying the new session", () => {
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });
    queryClient.setQueryData(["private-profile"], { username: "alice" });
    useChatStore.setState({
      activeKey: "1:2",
      wsOnline: true,
      messagesByKey: { "1:2": [] },
    });

    startSession("bob-token", user(2, "bob"));

    expect(queryClient.getQueryData(["private-profile"])).toBeUndefined();
    expect(useChatStore.getState()).toMatchObject({
      conversations: [],
      messagesByKey: {},
      activeKey: null,
      wsOnline: false,
    });
    expect(useAuthStore.getState().token).toBe("bob-token");
    expect(useAuthStore.getState().user?.username).toBe("bob");
  });

  test("updating the user under the same token keeps current query and chat resources", () => {
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });
    queryClient.setQueryData(["cart"], { total: 2 });
    useChatStore.setState({ activeKey: "1:2", wsOnline: true });

    startSession("alice-token", { ...user(1, "alice"), nickname: "Alice Updated" });

    expect(queryClient.getQueryData(["cart"])).toEqual({ total: 2 });
    expect(useChatStore.getState().activeKey).toBe("1:2");
    expect(useChatStore.getState().wsOnline).toBe(true);
    expect(useAuthStore.getState().user?.nickname).toBe("Alice Updated");
  });

  test("storage logout clears resources and authentication together", () => {
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });
    queryClient.setQueryData(["orders"], [1]);
    useChatStore.setState({ activeKey: "1:2", wsOnline: true });

    syncSessionFromStorage(null);

    expect(queryClient.getQueryData(["orders"])).toBeUndefined();
    expect(useChatStore.getState().activeKey).toBeNull();
    expect(useChatStore.getState().wsOnline).toBe(false);
    expect(useAuthStore.getState().token).toBeNull();
  });

  test("cross-tab account switching clears old resources before observers see the new account", () => {
    useAuthStore.setState({ token: "alice-token", user: user(1, "alice") });
    queryClient.setQueryData(["friends"], ["alice-private"]);
    useChatStore.setState({ activeKey: "1:2", wsOnline: true });
    let resourcesWhenBobAppeared: { query: unknown; activeKey: string | null; wsOnline: boolean } | null = null;
    const unsubscribe = useAuthStore.subscribe((state) => {
      if (state.token !== "bob-token") return;
      resourcesWhenBobAppeared = {
        query: queryClient.getQueryData(["friends"]),
        activeKey: useChatStore.getState().activeKey,
        wsOnline: useChatStore.getState().wsOnline,
      };
    });

    syncSessionFromStorage({ token: "bob-token", user: user(2, "bob") });
    unsubscribe();

    expect(resourcesWhenBobAppeared).toEqual({ query: undefined, activeKey: null, wsOnline: false });
    expect(useAuthStore.getState().user?.username).toBe("bob");
  });
});
