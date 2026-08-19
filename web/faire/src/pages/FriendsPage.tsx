import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  acceptFriendRequest,
  getFriendRequests,
  getFriends,
  rejectFriendRequest,
  searchUsers,
  sendFriendRequest,
} from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { FriendRequestView, FriendView } from "../api/types";
import { Button, EmptyState, ErrorState, Icon, LoadingBlock, Spinner } from "../components/ui";
import { formatDate } from "../lib/format";

type FriendsTab = "friends" | "incoming" | "outgoing";

export function FriendsPage() {
  const [tab, setTab] = useState<FriendsTab>("friends");
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">好友 / friends</p>
          <h1 className="mt-3 font-nantes text-5xl">有分享，才有交情。</h1>
        </div>
        <div className="section-index" aria-hidden="true">07 <span>/</span> friends</div>
      </div>

      {notice && <div className={`notice notice-${notice.kind} mt-6`}>{notice.text}</div>}

      <div className="order-tabs mt-8" role="tablist" aria-label="好友页面分区">
        {(
          [
            ["friends", "我的好友"],
            ["incoming", "收到的申请"],
            ["outgoing", "发出的申请"],
          ] as const
        ).map(([value, label]) => (
          <button
            key={value}
            role="tab"
            aria-selected={tab === value}
            className={tab === value ? "active" : ""}
            onClick={() => setTab(value)}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "friends" && <FriendsTab onNotice={setNotice} />}
      {tab === "incoming" && <IncomingTab onNotice={setNotice} />}
      {tab === "outgoing" && <OutgoingTab />}
    </section>
  );
}

function FriendsTab({ onNotice }: { onNotice: (n: { kind: "success" | "error"; text: string } | null) => void }) {
  const query = useQuery({ queryKey: ["friends"], queryFn: () => getFriends() });

  return (
    <div className="mt-8 grid gap-6 lg:grid-cols-2">
      <div>
        <h2 className="font-nantes text-2xl">我的好友</h2>
        {query.isPending ? (
          <LoadingBlock label="正在读取好友列表" />
        ) : query.isError ? (
          <div className="mt-4">
            <ErrorState message={getApiErrorMessage(query.error)} onRetry={() => query.refetch()} />
          </div>
        ) : query.data.length === 0 ? (
          <div className="mt-4">
            <EmptyState
              eyebrow="还没有好友"
              title="先发出第一份申请"
              description="在右侧搜索用户名，或等别人来加你。"
            />
          </div>
        ) : (
          <ul className="friend-list mt-4">
            {query.data.map((friend) => (
              <FriendRow key={friend.user_id} friend={friend} />
            ))}
          </ul>
        )}
      </div>

      <div>
        <h2 className="font-nantes text-2xl">添加好友</h2>
        <AddFriendPanel onNotice={onNotice} />
      </div>
    </div>
  );
}

function FriendRow({ friend }: { friend: FriendView }) {
  const navigate = useNavigate();
  return (
    <li className="friend-row">
      <div className="friend-avatar" aria-hidden="true">{friend.username.slice(0, 1).toUpperCase()}</div>
      <div className="friend-row-copy">
        <span className="friend-name">{friend.username}</span>
        <span className="friend-since">{formatDate(friend.since)} 起</span>
      </div>
      <Button variant="secondary" className="button-small" onClick={() => navigate(`/chat?peer=${friend.user_id}`)}>
        <Icon name="message" size={15} /> 发消息
      </Button>
    </li>
  );
}

function AddFriendPanel({
  onNotice,
}: {
  onNotice: (n: { kind: "success" | "error"; text: string } | null) => void;
}) {
  const [username, setUsername] = useState("");
  const [searched, setSearched] = useState("");
  const queryClient = useQueryClient();

  const search = useQuery({
    queryKey: ["users", "search", searched],
    queryFn: () => searchUsers(searched),
    enabled: searched.length > 0,
  });

  const send = useMutation({
    mutationFn: (userId: number) => sendFriendRequest(userId),
    onSuccess: () => {
      onNotice({ kind: "success", text: "申请已发出，等对方通过吧。" });
      queryClient.invalidateQueries({ queryKey: ["friend-requests"] });
    },
    onError: (error) => onNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const keyword = username.trim();
    if (keyword) setSearched(keyword);
  }

  return (
    <div className="mt-4">
      <form className="flex gap-2" onSubmit={submit}>
        <input
          className="form-control flex-1"
          value={username}
          onChange={(event) => setUsername(event.target.value)}
          placeholder="输入用户名（前缀匹配）"
          minLength={1}
          maxLength={32}
          autoComplete="off"
        />
        <Button type="submit" disabled={search.isFetching}>
          {search.isFetching ? <Spinner label="搜索中" /> : <><Icon name="search" size={16} /> 搜索</>}
        </Button>
      </form>

      {search.isError && (
        <p className="notice notice-error mt-4">{getApiErrorMessage(search.error)}</p>
      )}

      {search.isSuccess && searched && (
        <div className="mt-4">
          {search.data.length === 0 ? (
            <p className="notice notice-error">没有找到匹配的用户。</p>
          ) : (
            <ul className="friend-list">
              {search.data.map((user) => (
                <li key={user.id} className="friend-row">
                  <div className="friend-avatar" aria-hidden="true">{user.username.slice(0, 1).toUpperCase()}</div>
                  <div className="friend-row-copy">
                    <span className="friend-name">{user.username}</span>
                    <span className="friend-since">用户 #{user.id}</span>
                  </div>
                  <Button
                    variant="secondary"
                    className="button-small"
                    disabled={send.isPending}
                    onClick={() => send.mutate(user.id)}
                  >
                    <Icon name="user-plus" size={15} /> 加好友
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}

function IncomingTab({
  onNotice,
}: {
  onNotice: (n: { kind: "success" | "error"; text: string } | null) => void;
}) {
  const [page, setPage] = useState(1);
  const query = useQuery({
    queryKey: ["friend-requests", "incoming", page],
    queryFn: () => getFriendRequests({ scope: "incoming", page }),
  });
  const queryClient = useQueryClient();

  const pending = (query.data?.items ?? []).filter((r) => r.status === "pending");
  const history = (query.data?.items ?? []).filter((r) => r.status !== "pending");

  const decide = useMutation({
    mutationFn: async ({ id, accept }: { id: number; accept: boolean }) => {
      if (accept) {
        await acceptFriendRequest(id);
      } else {
        await rejectFriendRequest(id);
      }
    },
    onSuccess: (_data, vars) => {
      onNotice({ kind: "success", text: vars.accept ? "已通过，现在可以聊天了。" : "已拒绝。" });
      queryClient.invalidateQueries({ queryKey: ["friend-requests"] });
      queryClient.invalidateQueries({ queryKey: ["friends"] });
    },
    onError: (error) => onNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  if (query.isPending) return <LoadingBlock label="正在读取申请" />;
  if (query.isError) {
    return <ErrorState message={getApiErrorMessage(query.error)} onRetry={() => query.refetch()} />;
  }

  return (
    <div className="mt-8">
      <h2 className="font-nantes text-2xl">待处理</h2>
      {pending.length === 0 ? (
        <div className="mt-4">
          <EmptyState eyebrow="没有待处理申请" title="都处理完了" />
        </div>
      ) : (
        <ul className="friend-list mt-4">
          {pending.map((req) => (
            <li key={req.id} className="friend-row">
              <div className="friend-avatar" aria-hidden="true">{req.peer_username.slice(0, 1).toUpperCase()}</div>
              <div className="friend-row-copy">
                <span className="friend-name">{req.peer_username}</span>
                <span className="friend-since">{formatDate(req.created_at)} 申请加你为好友</span>
              </div>
              <div className="flex gap-2">
                <Button className="button-small" disabled={decide.isPending} onClick={() => decide.mutate({ id: req.id, accept: true })}>
                  <Icon name="check" size={15} /> 通过
                </Button>
                <Button variant="ghost" className="button-small" disabled={decide.isPending} onClick={() => decide.mutate({ id: req.id, accept: false })}>
                  拒绝
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}

      {history.length > 0 && (
        <>
          <h2 className="mt-10 font-nantes text-2xl">已处理</h2>
          <ul className="friend-list mt-4">
            {history.map((req) => (
              <RequestRow key={req.id} req={req} />
            ))}
          </ul>
        </>
      )}
      <RequestPager page={page} total={query.data.total} onPage={setPage} />
    </div>
  );
}

function OutgoingTab() {
  const [page, setPage] = useState(1);
  const query = useQuery({
    queryKey: ["friend-requests", "outgoing", page],
    queryFn: () => getFriendRequests({ scope: "outgoing", page }),
  });

  if (query.isPending) return <LoadingBlock label="正在读取申请" />;
  if (query.isError) {
    return <ErrorState message={getApiErrorMessage(query.error)} onRetry={() => query.refetch()} />;
  }
  if (query.data.items.length === 0) {
    return (
      <div className="mt-8">
        <EmptyState eyebrow="没有发出的申请" title="去添加好友吧" description="在「我的好友」页搜索用户名发起申请。" />
      </div>
    );
  }
  return (
    <div className="mt-8">
      <ul className="friend-list">
        {query.data.items.map((req) => (
          <RequestRow key={req.id} req={req} />
        ))}
      </ul>
      <RequestPager page={page} total={query.data.total} onPage={setPage} />
    </div>
  );
}

function RequestPager({ page, total, onPage }: { page: number; total: number; onPage: (page: number) => void }) {
  const pageCount = Math.max(1, Math.ceil(total / 20));
  if (pageCount === 1) return null;
  return (
    <div className="mt-6 flex items-center justify-between gap-3">
      <Button variant="secondary" disabled={page === 1} onClick={() => onPage(page - 1)}>
        上一页
      </Button>
      <span className="text-sm text-smoke">第 {page} / {pageCount} 页，共 {total} 条</span>
      <Button variant="secondary" disabled={page >= pageCount} onClick={() => onPage(page + 1)}>
        下一页
      </Button>
    </div>
  );
}

const STATUS_LABEL: Record<string, string> = {
  pending: "待对方处理",
  accepted: "已通过",
  rejected: "已被拒绝",
};

function RequestRow({ req }: { req: FriendRequestView }) {
  return (
    <li className="friend-row">
      <div className="friend-avatar" aria-hidden="true">{req.peer_username.slice(0, 1).toUpperCase()}</div>
      <div className="friend-row-copy">
        <span className="friend-name">{req.peer_username}</span>
        <span className="friend-since">{formatDate(req.created_at)}</span>
      </div>
      <span className={`tag ${req.status === "accepted" ? "tag-live" : ""}`}>{STATUS_LABEL[req.status]}</span>
    </li>
  );
}

export function FriendRequestsLink() {
  return (
    <p className="mt-6 text-sm text-smoke">
      想找人聊天？去 <Link to="/friends" className="underline underline-offset-4 text-ink-black">好友页</Link> 添加或通过申请。
    </p>
  );
}
