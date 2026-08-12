import { create } from "zustand";

import type { ConversationView, Message } from "../api/types";

interface ChatState {
  conversations: ConversationView[];
  messagesByKey: Record<string, Message[]>;
  activeKey: string | null;
  wsOnline: boolean;

  setConversations: (items: ConversationView[]) => void;
  upsertConversation: (conv: ConversationView) => void;
  setActiveKey: (key: string | null) => void;
  setMessages: (key: string, messages: Message[]) => void;
  setWsOnline: (online: boolean) => void;
  /** 收到/发送一条消息：去重追加、刷新会话预览、按当前会话判定未读与已读。 */
  handleMessage: (message: Message, isOwn: boolean) => void;
}

export const useChatStore = create<ChatState>()((set, get) => ({
  conversations: [],
  messagesByKey: {},
  activeKey: null,
  wsOnline: false,

  setConversations: (items) => set({ conversations: items }),

  upsertConversation: (conv) => {
    const rest = get().conversations.filter((c) => c.conversation_key !== conv.conversation_key);
    set({ conversations: [conv, ...rest] });
  },

  setActiveKey: (key) => set({ activeKey: key }),

  setMessages: (key, messages) =>
    set((state) => ({ messagesByKey: { ...state.messagesByKey, [key]: messages } })),

  setWsOnline: (online) => set({ wsOnline: online }),

  handleMessage: (message, isOwn) => {
    const { conversations, messagesByKey, activeKey } = get();
    const key = message.conversation_key;
    const existing = messagesByKey[key] ?? [];
    if (existing.some((m) => m.id === message.id)) return;

    const next = [...existing, message];
    const isActive = activeKey === key && !isOwn;

    const conv = conversations.find((c) => c.conversation_key === key);
    // 未知会话（WS 先于会话列表到达）：建条目，对方用户名未知占位，轮询到达后替换。
    const peerUserId = conv?.peer_user_id ?? message.sender_id;
    const unread = isActive || isOwn ? 0 : (conv?.unread_count ?? 0) + 1;
    const nextConversations = [...conversations]
      .filter((c) => c.conversation_key !== key)
      .concat({
        conversation_key: key,
        peer_user_id: peerUserId,
        peer_username: conv?.peer_username ?? `用户 #${peerUserId}`,
        last_message: message,
        unread_count: unread,
      })
      .sort((a, b) => {
        const at = new Date(a.last_message?.created_at ?? 0).getTime();
        const bt = new Date(b.last_message?.created_at ?? 0).getTime();
        return bt - at;
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
