import { create } from "zustand";

import type { ConversationView, Message } from "../api/types";

function mergeMessages(current: readonly Message[], incoming: readonly Message[]): Message[] {
  const byID = new Map(current.map((message) => [message.id, message]));
  for (const message of incoming) byID.set(message.id, message);
  return [...byID.values()].sort((a, b) => a.id - b.id);
}

function mergeConversations(
  current: readonly ConversationView[],
  incoming: readonly ConversationView[],
): ConversationView[] {
  const byKey = new Map(current.map((conversation) => [conversation.conversation_key, conversation]));
  for (const conversation of incoming) {
    const existing = byKey.get(conversation.conversation_key);
    const existingLastID = existing?.last_message?.id ?? 0;
    const incomingLastID = conversation.last_message?.id ?? 0;
    if (existingLastID > incomingLastID) {
      byKey.set(conversation.conversation_key, {
        ...existing,
        ...conversation,
        last_message: existing?.last_message,
        unread_count: existing?.unread_count ?? 0,
      });
    } else if (existing && existingLastID === incomingLastID) {
      byKey.set(conversation.conversation_key, {
        ...existing,
        ...conversation,
        unread_count: Math.min(existing.unread_count, conversation.unread_count),
      });
    } else {
      byKey.set(conversation.conversation_key, conversation);
    }
  }
  return [...byKey.values()].sort(
    (a, b) => (b.last_message?.id ?? 0) - (a.last_message?.id ?? 0),
  );
}

interface ChatState {
  conversations: ConversationView[];
  conversationsHasMore: boolean;
  messagesByKey: Record<string, Message[]>;
  activeKey: string | null;
  wsOnline: boolean;

  setConversations: (items: ConversationView[], hasMore: boolean) => void;
  setConversationPage: (items: ConversationView[], hasMore: boolean) => void;
  upsertConversation: (conv: ConversationView) => void;
  setActiveKey: (key: string | null) => void;
  setMessages: (key: string, messages: Message[]) => void;
  markConversationReadLocally: (key: string, readThroughID: number) => void;
  setWsOnline: (online: boolean) => void;
  reset: () => void;
  /** 收到/发送一条消息：去重追加、刷新会话预览、按当前会话判定未读与已读。 */
  handleMessage: (message: Message, isOwn: boolean) => void;
}

export const useChatStore = create<ChatState>()((set, get) => ({
  conversations: [],
  conversationsHasMore: false,
  messagesByKey: {},
  activeKey: null,
  wsOnline: false,

  setConversations: (items, hasMore) => set((state) => ({
    conversations: mergeConversations(state.conversations, items),
    conversationsHasMore: state.conversations.length > items.length
      ? state.conversationsHasMore
      : hasMore,
  })),

  setConversationPage: (items, hasMore) => set((state) => ({
    conversations: mergeConversations(state.conversations, items),
    conversationsHasMore: hasMore,
  })),

  upsertConversation: (conv) => {
    set((state) => ({ conversations: mergeConversations(state.conversations, [conv]) }));
  },

  setActiveKey: (key) => set({ activeKey: key }),

  setMessages: (key, messages) =>
    set((state) => ({
      messagesByKey: {
        ...state.messagesByKey,
        [key]: mergeMessages(state.messagesByKey[key] ?? [], messages),
      },
    })),

  markConversationReadLocally: (key, readThroughID) => set((state) => ({
    conversations: state.conversations.map((conversation) => (
      conversation.conversation_key === key
        && (conversation.last_message?.id ?? 0) <= readThroughID
        ? { ...conversation, unread_count: 0 }
        : conversation
    )),
  })),

  setWsOnline: (online) => set({ wsOnline: online }),

  reset: () => set({ conversations: [], conversationsHasMore: false, messagesByKey: {}, activeKey: null, wsOnline: false }),

  handleMessage: (message, isOwn) => {
    const { conversations, messagesByKey, activeKey } = get();
    const key = message.conversation_key;
    const existing = messagesByKey[key] ?? [];
    if (existing.some((m) => m.id === message.id)) return;

    const next = mergeMessages(existing, [message]);
    const isActive = activeKey === key && !isOwn;

    const conv = conversations.find((c) => c.conversation_key === key);
    // 未知会话（WS 先于会话列表到达）：建条目，对方用户名未知占位，轮询到达后替换。
    const peerUserId = conv?.peer_user_id ?? message.sender_id;
    const unread = isActive || isOwn ? 0 : (conv?.unread_count ?? 0) + 1;
    const lastMessage = (conv?.last_message?.id ?? 0) > message.id ? conv?.last_message : message;
    const nextConversations = [...conversations]
      .filter((c) => c.conversation_key !== key)
      .concat({
        conversation_key: key,
        peer_user_id: peerUserId,
        peer_username: conv?.peer_username ?? `用户 #${peerUserId}`,
        last_message: lastMessage,
        unread_count: unread,
      })
      .sort((a, b) => {
        return (b.last_message?.id ?? 0) - (a.last_message?.id ?? 0);
      });

    set({
      messagesByKey: { ...messagesByKey, [key]: next },
      conversations: nextConversations,
    });
  },
}));

export function totalUnread(conversations: ConversationView[]): number {
  return conversations.reduce((sum, c) => sum + c.unread_count, 0);
}
