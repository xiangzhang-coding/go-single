import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";

import { getMessages, markConversationRead, sendMessage, uploadFile } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { Message } from "../api/types";
import { Button, EmptyState, Icon, LoadingBlock, Spinner } from "../components/ui";
import { AuthorizedDownload, AuthorizedImage } from "../components/AuthorizedMedia";
import { useAuthStore } from "../store/auth";
import { useChatStore } from "../store/chat";
import { formatDate } from "../lib/format";
import { makeClientRequestID } from "../lib/format";
import { IMAGE_ACCEPT, MESSAGE_FILE_ACCEPT, validateImage, validateMessageFile } from "../lib/media";

export function ChatPage() {
  const { token, user } = useAuthStore();
  const { conversations, messagesByKey, activeKey, setActiveKey } = useChatStore();
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
  hidden,
}: {
  conversations: Array<{ conversation_key: string; peer_username: string; last_message?: Message; unread_count: number }>;
  activeKey: string | null;
  onSelect: (key: string) => void;
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
  const { setMessages, handleMessage } = useChatStore();
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const bottomRef = useRef<HTMLDivElement>(null);

  // 进入会话：无本地缓存拉最近 30 条；已有缓存按 after_id 补拉（追加不覆盖，
  // 避免补拉空结果清掉本地已显示的消息）。拉取完成后标记已读。
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    const lastId = messages[messages.length - 1]?.id;
    const pull = lastId
      ? getMessages(conversationKey, { afterId: lastId, limit: 30 })
      : getMessages(conversationKey, { limit: 30 });
    void pull
      .then(({ items, has_more }) => {
        if (cancelled) return;
        if (lastId) {
          if (items.length > 0) setMessages(conversationKey, [...messages, ...items]);
        } else {
          setMessages(conversationKey, items);
          setHasMore(has_more);
        }
        const newest = items[items.length - 1];
        if (newest) {
          void markConversationRead(conversationKey, newest.id).catch(() => undefined);
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
  }, [conversationKey, setMessages]);

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
  const imageInput = useRef<HTMLInputElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  const send = useMutation({
    mutationFn: async (payload: { type: "text"; content: string } | { type: "image" | "file"; file: File }) => {
      const uploaded = payload.type === "text" ? null : await uploadFile(payload.file, payload.type);
      return sendMessage({
        to_user_id: peerUserId,
        type: payload.type,
        content: payload.type === "text" ? payload.content : undefined,
        url: uploaded?.url,
        client_request_id: makeClientRequestID(),
      });
    },
    onSuccess: (msg, payload) => {
      if (payload.type === "text") setText("");
      setError(null);
      onSent(msg);
    },
    onError: (err) => setError(getApiErrorMessage(err)),
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const content = text.trim();
    if (!content || send.isPending) return;
    send.mutate({ type: "text", content });
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
    send.mutate({ type, file: selected });
  }

  return (
    <div className="chat-composer">
      {error && <p className="notice notice-error mb-3">{error}</p>}
      <form className="chat-composer-row" onSubmit={submit}>
        <input
          className="form-control chat-composer-input"
          value={text}
          onChange={(e) => setText(e.target.value)}
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
