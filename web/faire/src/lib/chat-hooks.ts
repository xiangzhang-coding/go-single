import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";

import { getConversations, markConversationRead } from "../api/endpoints";
import { useAuthStore } from "../store/auth";
import { useChatStore } from "../store/chat";
import { chatSocket } from "./ws";

/**
 * 聊天实时链路（App 级单例）：
 * - 登录态建立 WS 连接、登出断开；
 * - new_message 事件按会话路由：当前会话即时显示并推进已读游标，
 *   其他会话累加未读（AppShell 角标读取）；
 * - 会话列表首次拉取 + 60s 轮询兜底（WS 断开期间的会话变化）。
 */
export function useChatRealtime() {
  const token = useAuthStore((state) => state.token);
  const userId = useAuthStore((state) => state.user?.id);
  const setConversations = useChatStore((state) => state.setConversations);
  const setWsOnline = useChatStore((state) => state.setWsOnline);

  const conversationsQuery = useQuery({
    queryKey: ["conversations", userId],
    queryFn: async () => (await getConversations({ limit: 50 })).items,
    enabled: Boolean(token && userId),
    refetchInterval: 60_000,
    staleTime: 30_000,
  });

  useEffect(() => {
    if (token && userId && conversationsQuery.data) {
      setConversations(conversationsQuery.data);
    }
  }, [conversationsQuery.data, setConversations, token, userId]);

  useEffect(() => {
    if (!token) {
      chatSocket.disconnect();
      setWsOnline(false);
      return;
    }
    chatSocket.connect(token);

    const unsubscribe = chatSocket.subscribe(({ data }) => {
      const { activeKey, handleMessage } = useChatStore.getState();
      const isActive = activeKey === data.conversation_key;
      if (isActive) {
        // 当前会话：即时已读（游标推进用新消息 id）。
        void markConversationRead(data.conversation_key, data.id).catch(() => undefined);
      }
      handleMessage(data, false);
    });
    const offStatus = chatSocket.onStatusChange((status) => {
      setWsOnline(status === "open");
    });
    return () => {
      unsubscribe();
      offStatus();
    };
  }, [token, setWsOnline]);
}
