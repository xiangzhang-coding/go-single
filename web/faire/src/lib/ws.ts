import type { Message } from "../api/types";

export interface ChatSocketEvent {
  event: "new_message";
  data: Message;
}

export type ChatSocketHandler = (event: ChatSocketEvent) => void;

const WS_BASE = import.meta.env.VITE_WS_BASE || "/ws";

type SocketStatus = "idle" | "connecting" | "open" | "closed";

/**
 * 聊天实时通道单例：GET /ws?token=<jwt>（后端约定 token 走 query）。
 * 仅接收服务端推送（new_message）；断线自动指数退避重连（30s 上限），
 * 登出时 disconnect 停止。连接由登录态驱动：chat-hooks 在 token 变化时
 * connect/disconnect（WS 无 token 失效推送，重连至会话结束为止）。
 */
class ChatSocket {
  private socket: WebSocket | null = null;
  private handlers = new Set<ChatSocketHandler>();
  private status: SocketStatus = "idle";
  private statusListeners = new Set<(status: SocketStatus) => void>();
  private retry = 0;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private token: string | null = null;
  private closing = false;

  connect(token: string) {
    this.token = token;
    this.closing = false;
    if (this.socket) return;
    this.open();
  }

  disconnect() {
    this.closing = true;
    this.retry = 0;
    if (this.retryTimer) {
      clearTimeout(this.retryTimer);
      this.retryTimer = null;
    }
    if (this.socket) {
      this.socket.close();
      this.socket = null;
    }
    this.token = null;
    this.setStatus("idle");
  }

  subscribe(handler: ChatSocketHandler): () => void {
    this.handlers.add(handler);
    return () => this.handlers.delete(handler);
  }

  onStatusChange(listener: (status: SocketStatus) => void): () => void {
    this.statusListeners.add(listener);
    listener(this.status);
    return () => this.statusListeners.delete(listener);
  }

  private open() {
    if (!this.token) return;
    this.setStatus("connecting");
    const url = `${WS_BASE}?token=${encodeURIComponent(this.token)}`;
    const socket = new WebSocket(url);
    this.socket = socket;

    socket.onopen = () => {
      this.retry = 0;
      this.setStatus("open");
    };

    socket.onmessage = (event: MessageEvent<string>) => {
      let envelope: ChatSocketEvent;
      try {
        envelope = JSON.parse(event.data) as ChatSocketEvent;
      } catch {
        return;
      }
      if (envelope?.event === "new_message") {
        for (const handler of this.handlers) {
          handler(envelope);
        }
      }
    };

    socket.onclose = () => {
      this.socket = null;
      this.setStatus("closed");
      if (this.closing || !this.token) return;
      // 指数退避重连：1s → 2s → 4s → … 上限 30s。
      const delay = Math.min(30_000, 1_000 * 2 ** this.retry);
      this.retry += 1;
      this.retryTimer = setTimeout(() => this.open(), delay);
    };

    socket.onerror = () => {
      socket.close();
    };
  }

  private setStatus(status: SocketStatus) {
    this.status = status;
    for (const listener of this.statusListeners) {
      listener(status);
    }
  }
}

export const chatSocket = new ChatSocket();
