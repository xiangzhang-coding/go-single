import { useAuthStore } from "../store/auth";
import { useChatStore } from "../store/chat";
import { queryClient } from "./query-client";
import { chatSocket } from "./ws";

export function endSession() {
  chatSocket.disconnect();
  queryClient.clear();
  useChatStore.getState().reset();
  useAuthStore.getState().logout();
}
