import { afterEach, describe, expect, test } from "bun:test";

import type { ConversationView } from "../src/api/types";
import { conversationBeforeID } from "../src/lib/chat";
import { useChatStore } from "../src/store/chat";

function conversation(id: number): ConversationView {
  return {
    conversation_key: `1:${id}`,
    peer_user_id: id,
    peer_username: `user-${id}`,
    last_message: {
      id,
      conversation_key: `1:${id}`,
      sender_id: id,
      recipient_id: 1,
      type: "text",
      content: `message-${id}`,
      created_at: "2026-08-22T00:00:00Z",
    },
    unread_count: 0,
  };
}

afterEach(() => {
  useChatStore.getState().reset();
});

describe("conversation cursor pagination", () => {
  test("merges later pages and keeps loading until has_more is false", () => {
    useChatStore.getState().setConversationPage([conversation(5), conversation(4)], true);
    expect(conversationBeforeID(useChatStore.getState().conversations)).toBe(4);

    useChatStore.getState().setConversationPage([conversation(3), conversation(2)], false);

    expect(useChatStore.getState().conversations.map((item) => item.last_message?.id)).toEqual([5, 4, 3, 2]);
    expect(useChatStore.getState().conversationsHasMore).toBe(false);
  });
});
