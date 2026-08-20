import { afterEach, describe, expect, test } from "bun:test";

import { latestMessageID } from "../web/faire/src/lib/chat";
import { useChatStore } from "../web/faire/src/store/chat";

const message = (id: number) => ({
  id,
  conversation_key: "1:2",
  sender_id: 2,
  recipient_id: 1,
  type: "text" as const,
  content: `message-${id}`,
  url: "",
  created_at: "2026-08-20T00:00:00Z",
});

afterEach(() => useChatStore.getState().reset());

describe("chat read state", () => {
  test("uses the cached latest message when the incremental pull is empty", () => {
    expect(latestMessageID([message(41), message(42)], [])).toBe(42);
  });

  test("uses a newer message returned by the incremental pull", () => {
    expect(latestMessageID([message(42)], [message(43)])).toBe(43);
  });

  test("clears the selected conversation unread count locally", () => {
    useChatStore.setState({
      conversations: [{
        conversation_key: "1:2",
        peer_user_id: 2,
        peer_username: "bob",
        last_message: message(42),
        unread_count: 3,
      }],
    });

    useChatStore.getState().markConversationReadLocally("1:2", 42);

    expect(useChatStore.getState().conversations[0]?.unread_count).toBe(0);
  });

  test("a late read response does not clear a newer unread message", () => {
    useChatStore.setState({
      conversations: [{
        conversation_key: "1:2",
        peer_user_id: 2,
        peer_username: "bob",
        last_message: message(43),
        unread_count: 1,
      }],
    });

    useChatStore.getState().markConversationReadLocally("1:2", 42);

    expect(useChatStore.getState().conversations[0]?.unread_count).toBe(1);
  });

  test("keeps cached messages and the conversation preview monotonic by id", () => {
    useChatStore.getState().handleMessage(message(43), false);
    useChatStore.getState().handleMessage(message(42), false);

    expect(useChatStore.getState().messagesByKey["1:2"]?.map((item) => item.id)).toEqual([42, 43]);
    expect(useChatStore.getState().conversations[0]?.last_message?.id).toBe(43);
    expect(useChatStore.getState().conversations[0]?.unread_count).toBe(2);
  });

  test("an older HTTP message page cannot overwrite a newer WebSocket message", () => {
    useChatStore.getState().handleMessage(message(44), false);

    useChatStore.getState().setMessages("1:2", [message(43)]);

    expect(useChatStore.getState().messagesByKey["1:2"]?.map((item) => item.id)).toEqual([43, 44]);
  });

  test("an older conversation poll cannot overwrite a newer WebSocket preview", () => {
    useChatStore.getState().handleMessage(message(44), false);

    useChatStore.getState().setConversations([{
      conversation_key: "1:2",
      peer_user_id: 2,
      peer_username: "bob",
      last_message: message(43),
      unread_count: 0,
    }]);

    expect(useChatStore.getState().conversations[0]?.last_message?.id).toBe(44);
    expect(useChatStore.getState().conversations[0]?.unread_count).toBe(1);
  });

  test("a stale poll with the same last message cannot restore cleared unread state", () => {
    useChatStore.setState({
      conversations: [{
        conversation_key: "1:2",
        peer_user_id: 2,
        peer_username: "bob",
        last_message: message(44),
        unread_count: 0,
      }],
    });

    useChatStore.getState().setConversations([{
      conversation_key: "1:2",
      peer_user_id: 2,
      peer_username: "bob",
      last_message: message(44),
      unread_count: 1,
    }]);

    expect(useChatStore.getState().conversations[0]?.unread_count).toBe(0);
  });
});
