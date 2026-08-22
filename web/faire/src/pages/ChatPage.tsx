import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";

import { getConversations, getMessages, markConversationRead, sendMessage, uploadFile } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { Message } from "../api/types";
import { Button, EmptyState, Icon, LoadingBlock, Spinner } from "../components/ui";
import { AuthorizedDownload, AuthorizedImage } from "../components/AuthorizedMedia";
import { useAuthStore } from "../store/auth";
import { useChatStore } from "../store/chat";
import { formatDate } from "../lib/format";
import { conversationBeforeID, latestMessageID } from "../lib/chat";
import { IMAGE_ACCEPT, MESSAGE_FILE_ACCEPT, validateImage, validateMessageFile } from "../lib/media";
import {
  createMediaSendOperation,
  createTextSendOperation,
  executeMessageSend,
  type MessageSendOperation,
} from "../lib/message-send";

export function ChatPage() {
  const { token, user } = useAuthStore();
  const {
    conversations,
    conversationsHasMore,
    messagesByKey,
    activeKey,
    setActiveKey,
    setConversationPage,
  } = useChatStore();
  const [searchParams] = useSearchParams();
  const peerParam = searchParams.get("peer");

  // 来自好友页的 ?peer=<userId>：优先激活已有会话；无会话进入新会话模式。
  const peerId = peerParam ? Number(peerParam) : NaN;
  const existing = useMemo(
    () => (Number.isFinite(peerId) ? conversations.find((c) => c.peer_user_id === peerId) : undefined),
    [conversations, peerId],
  );
  const [newPeerId, setNewPeerId] = useState<number | null>(
    Number.isFinite(peerId) && !existing ? peerId : null,
  );

  useEffect(() => {
    if (!existing) return;
    setActiveKey(existing.conversation_key);
    setNewPeerId(null);
  }, [existing, setActiveKey]);

  useEffect(() => {
    if (Number.isFinite(peerId) && !existing && newPeerId !== peerId) setNewPeerId(peerId);
  }, [peerId, existing, newPeerId]);

  useEffect(() => () => {
    setActiveKey(null);
  }, [setActiveKey]);

  const loadMoreConversations = useMutation({
    mutationFn: async () => {
      const beforeId = conversationBeforeID(conversations);
      if (!beforeId) return null;
      return getConversations({ beforeId, limit: 20 });
    },
    onSuccess: (page) => {
      if (page) setConversationPage(page.items, page.has_more);
    },
  });

  const activeConversation = conversations.find((c) => c.conversation_key === activeKey) ?? null;
  const activeMessages = activeKey ? (messagesByKey[activeKey] ?? []) : [];
  const me = user?.id ?? 0;

  if (!token) return null;

  return (
    <section className="site-container page-section pt-8 sm:pt-14 chat-page">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">聊天 / chat</p>
          <h1 className="mt-3 font-nantes text-5xl">说句实在的。</h1>
        </div>
        <div className="section-index" aria-hidden="true">09 <span>/</span> chat</div>
      </div>

      <div className="chat-layout mt-8">
        <ConversationList
          conversations={conversations}
          activeKey={activeKey}
          onSelect={(key) => {
            setActiveKey(key);
            setNewPeerId(null);
          }}
          hasMore={conversationsHasMore}
          loadingMore={loadMoreConversations.isPending}
          onLoadMore={() => loadMoreConversations.mutate()}
          hidden={Boolean(activeKey)}
        />
        <div className={`chat-thread ${activeKey || newPeerId ? "" : "chat-thread-empty"}`}>
          {activeConversation ? (
            <ActiveThread
              key={activeConversation.conversation_key}
              conversationKey={activeConversation.conversation_key}
              peerUsername={activeConversation.peer_username}
              peerUserId={activeConversation.peer_user_id}
              messages={activeMessages}
              me={me}
              onBack={() => setActiveKey(null)}
            />
          ) : newPeerId ? (
            <NewThread
              peerUserId={newPeerId}
              onCreated={(key) => {
                setNewPeerId(null);
                setActiveKey(key);
              }}
              onBack={() => {
                setNewPeerId(null);
                setActiveKey(null);
              }}
            />
          ) : (
            <div className="chat-thread-placeholder">
              <EmptyState
                eyebrow="选择会话"
                title="开始一段对话"
                description="从左侧选择会话，或到好友页向好友发起第一条消息。"
              />
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

// ---- 会话列表 ----

function ConversationList({
  conversations,
  activeKey,
  onSelect,
  hasMore,
  loadingMore,
  onLoadMore,
  hidden,
}: {
  conversations: Array<{ conversation_key: string; peer_username: string; last_message?: Message; unread_count: number }>;
  activeKey: string | null;
  onSelect: (key: string) => void;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
  hidden: boolean;
}) {
  const navigate = useNavigate();

  return (
    <aside className={`chat-conversations ${hidden ? "chat-conversations-hidden" : ""}`}>
      <div className="chat-conversations-head">
        <h2 className="font-nantes text-2xl">会话</h2>
        <Button variant="ghost" className="button-small" onClick={() => navigate("/friends")}>
          <Icon name="user-plus" size={15} /> 好友
        </Button>
      </div>
      {conversations.length === 0 ? (
        <p className="chat-empty-tip">还没有会话。到<button className="text-link" onClick={() => navigate("/friends")}>好友页</button>找个人聊聊。</p>
      ) : (
        <ul className="chat-conversation-list">
          {conversations.map((conv) => (
            <li key={conv.conversation_key}>
              <button
                className={`chat-conversation ${conv.conversation_key === activeKey ? "active" : ""}`}
                onClick={() => onSelect(conv.conversation_key)}
              >
                <div className="friend-avatar" aria-hidden="true">{conv.peer_username.slice(0, 1).toUpperCase()}</div>
                <div className="chat-conversation-copy">
                  <span className="chat-conversation-name">{conv.peer_username}</span>
                  <span className="chat-conversation-preview">
                    {conv.last_message
                      ? messagePreview(conv.last_message)
                      : "还没有消息"}
                  </span>
                </div>
                {conv.unread_count > 0 && <span className="chat-unread">{conv.unread_count}</span>}
              </button>
            </li>
          ))}
          {hasMore && (
            <li className="flex justify-center py-2">
              <button type="button" className="chat-load-older" disabled={loadingMore} onClick={onLoadMore}>
                {loadingMore ? "加载中…" : "加载更多会话"}
              </button>
            </li>
          )}
        </ul>
      )}
    </aside>
  );
}

function messagePreview(message: Message): string {
  if (message.type === "text") return message.content ?? "";
  if (message.type === "image") return "[图片]";
  return "[文件]";
}

// ---- 消息线程（已有会话）----

function ActiveThread({
  conversationKey,
  peerUsername,
  peerUserId,
  messages,
  me,
  onBack,
}: {
  conversationKey: string;
  peerUsername: string;
  peerUserId: number;
  messages: Message[];
  me: number;
  onBack: () => void;
}) {
  const { setMessages, handleMessage, markConversationReadLocally } = useChatStore();
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const bottomRef = useRef<HTMLDivElement>(null);

  // 首次进入始终拉最近 30 条并与 WS 缓存合并。缓存可能只有一条实时消息，
  // 不能据此使用 after_id，否则该消息之前的历史将永远不可见。
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void getMessages(conversationKey, { limit: 30 })
      .then(({ items, has_more }) => {
        if (cancelled) return;
        setMessages(conversationKey, items);
        setHasMore(has_more);
        const readThroughID = latestMessageID(messages, items);
        if (readThroughID) {
          void markConversationRead(conversationKey, readThroughID)
            .then(() => markConversationReadLocally(conversationKey, readThroughID))
            .catch(() => undefined);
        }
      })
      .catch(() => undefined)
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // messages 仅作初始引用：拉取结果按分支整体替换/追加。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conversationKey, markConversationReadLocally, setMessages]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [messages.length, loading]);

  const loadOlder = useMutation({
    mutationFn: async () => {
      const first = messages[0];
      if (!first) return;
      const { items, has_more } = await getMessages(conversationKey, { beforeId: first.id, limit: 30 });
      if (items.length > 0) {
        setMessages(conversationKey, [...items, ...messages]);
      }
      setHasMore(has_more);
    },
  });

  return (
    <div className="chat-thread-inner">
      <div className="chat-thread-head">
        <button className="icon-button chat-back" aria-label="返回会话列表" onClick={onBack}>
          <Icon name="arrow-left" size={17} />
        </button>
        <div className="chat-thread-peer">
          <span className="chat-thread-name">{peerUsername}</span>
          <span className="chat-thread-sub">用户 #{peerUserId}</span>
        </div>
      </div>

      <div className="chat-messages">
        {loading ? (
          <LoadingBlock label="正在读取消息" />
        ) : (
          <>
            {hasMore && (
              <button
                className="chat-load-older"
                disabled={loadOlder.isPending}
                onClick={() => loadOlder.mutate()}
              >
                {loadOlder.isPending ? "加载中…" : "加载更早的消息"}
              </button>
            )}
            {messages.length === 0 ? (
              <p className="chat-empty-tip">和 {peerUsername} 还没有消息，说点什么吧。</p>
            ) : (
              messages.map((msg) => <MessageBubble key={msg.id} message={msg} own={msg.sender_id === me} />)
            )}
            <div ref={bottomRef} />
          </>
        )}
      </div>

      <MessageComposer
        peerUserId={peerUserId}
        onSent={(msg) => handleMessage(msg, true)}
        disabled={loading}
      />
    </div>
  );
}

// ---- 新会话（从好友页跳转，尚无会话）----

function NewThread({
  peerUserId,
  onCreated,
  onBack,
}: {
  peerUserId: number;
  onCreated: (key: string) => void;
  onBack: () => void;
}) {
  const { setMessages, handleMessage, upsertConversation } = useChatStore();

  return (
    <div className="chat-thread-inner">
      <div className="chat-thread-head">
        <button className="icon-button chat-back" aria-label="返回会话列表" onClick={onBack}>
          <Icon name="arrow-left" size={17} />
        </button>
        <div className="chat-thread-peer">
          <span className="chat-thread-name">用户 #{peerUserId}</span>
          <span className="chat-thread-sub">新会话 · 第一条消息即建立会话</span>
        </div>
      </div>
      <div className="chat-messages">
        <p className="chat-empty-tip">发出第一条消息后，会话会出现在左侧列表。</p>
      </div>
      <MessageComposer
        peerUserId={peerUserId}
        onSent={(msg) => {
          handleMessage(msg, true);
          setMessages(msg.conversation_key, [msg]);
          // 立即建立会话条目（对方用户名未知，占位；会话轮询到达后替换）。
          upsertConversation({
            conversation_key: msg.conversation_key,
            peer_user_id: peerUserId,
            peer_username: `用户 #${peerUserId}`,
            last_message: msg,
            unread_count: 0,
          });
          onCreated(msg.conversation_key);
        }}
      />
    </div>
  );
}

// ---- 消息气泡 ----

function MessageBubble({ message, own }: { message: Message; own: boolean }) {
  return (
    <div className={`chat-message ${own ? "chat-message-own" : ""}`}>
      <div className="chat-bubble">
        {message.type === "image" && message.url ? (
          <AuthorizedImage className="chat-bubble-image" reference={message.url} alt="图片消息" />
        ) : message.type === "file" && message.url ? (
          <AuthorizedDownload reference={message.url} />
        ) : (
          <span className="chat-bubble-text">{message.content || ""}</span>
        )}
      </div>
      <span className="chat-message-time">{formatDate(message.created_at)}</span>
    </div>
  );
}

// ---- 输入区 ----

function MessageComposer({
  peerUserId,
  onSent,
  disabled,
}: {
  peerUserId: number;
  onSent: (msg: Message) => void;
  disabled?: boolean;
}) {
  const [text, setText] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pendingOperation, setPendingOperation] = useState<MessageSendOperation | null>(null);
  const pendingOperationRef = useRef<MessageSendOperation | null>(null);
  const imageInput = useRef<HTMLInputElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  function rememberOperation(operation: MessageSendOperation | null) {
    pendingOperationRef.current = operation;
    setPendingOperation(operation);
  }

  const send = useMutation({
    mutationFn: (operation: MessageSendOperation) => executeMessageSend(
      operation,
      peerUserId,
      uploadFile,
      sendMessage,
      rememberOperation,
    ),
    onSuccess: ({ result: message }, operation) => {
      if (operation.type === "text") setText("");
      rememberOperation(null);
      setError(null);
      onSent(message);
    },
    onError: (err) => setError(getApiErrorMessage(err)),
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const content = text.trim();
    if (!content || send.isPending) return;
    const pending = pendingOperationRef.current;
    const operation = pending?.type === "text" && pending.content === content
      ? pending
      : createTextSendOperation(content);
    rememberOperation(operation);
    send.mutate(operation);
  }

  function pickMedia(event: React.ChangeEvent<HTMLInputElement>, type: "image" | "file") {
    const selected = event.target.files?.[0];
    event.target.value = "";
    if (!selected || send.isPending) return;
    setError(null);
    if (type === "image") {
      const validationError = validateImage(selected);
      if (validationError) {
        setError(validationError);
        return;
      }
    } else {
      const validationError = validateMessageFile(selected);
      if (validationError) {
        setError(validationError);
        return;
      }
    }
    const operation = createMediaSendOperation(type, selected);
    rememberOperation(operation);
    send.mutate(operation);
  }

  return (
    <div className="chat-composer">
      {error && (
        <div className="notice notice-error mb-3">
          <p>{error}</p>
          {pendingOperation && (
            <Button
              type="button"
              variant="secondary"
              className="button-small mt-2"
              disabled={send.isPending}
              onClick={() => send.mutate(pendingOperation)}
            >
              <Icon name="refresh" size={15} /> 重试发送
            </Button>
          )}
        </div>
      )}
      <form className="chat-composer-row" onSubmit={submit}>
        <input
          className="form-control chat-composer-input"
          value={text}
          onChange={(e) => {
            const next = e.target.value;
            setText(next);
            const pending = pendingOperationRef.current;
            if (send.isError && pending?.type === "text" && pending.content !== next.trim()) {
              rememberOperation(null);
              setError(null);
              send.reset();
            }
          }}
          placeholder="输入消息，Enter 发送"
          maxLength={2000}
          disabled={disabled || send.isPending}
        />
        <button
          type="button"
          className="icon-button chat-composer-image"
          aria-label="发送图片"
          disabled={disabled || send.isPending}
          onClick={() => imageInput.current?.click()}
        >
          {send.isPending ? <Spinner label="发送中" /> : <Icon name="image" size={19} />}
        </button>
        <button
          type="button"
          className="icon-button chat-composer-image"
          aria-label="发送文件"
          disabled={disabled || send.isPending}
          onClick={() => fileInput.current?.click()}
        >
          <Icon name="pin" size={19} />
        </button>
        <Button type="submit" disabled={disabled || send.isPending || !text.trim()}>
          <Icon name="send" size={16} /> 发送
        </Button>
        <input ref={imageInput} type="file" accept={IMAGE_ACCEPT} hidden onChange={(event) => pickMedia(event, "image")} />
        <input ref={fileInput} type="file" accept={MESSAGE_FILE_ACCEPT} hidden onChange={(event) => pickMedia(event, "file")} />
      </form>
      <p className="mt-2 text-xs text-smoke">图片 ≤5MB；文件支持 PDF / ZIP / TXT / CSV / MD，≤20MB。</p>
    </div>
  );
}
