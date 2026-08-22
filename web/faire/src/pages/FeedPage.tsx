import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  deletePost,
  getFeed,
  getMyPosts,
  getOrders,
  sharePost,
  uploadFile,
} from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { OrderItem, OrderStatus, PostView } from "../api/types";
import { Button, EmptyState, ErrorState, Icon, LoadingBlock, Spinner } from "../components/ui";
import { AuthorizedImage } from "../components/AuthorizedMedia";
import { formatDate } from "../lib/format";
import { IMAGE_ACCEPT, validateImage } from "../lib/media";
import { executeMediaSave, type PreparedMediaUpload } from "../lib/media-save";

type FeedTab = "share" | "feed" | "mine";

const PURCHASED_STATUSES: OrderStatus[] = ["paid", "shipped", "completed"];

export function FeedPage() {
  const [tab, setTab] = useState<FeedTab>("share");
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">好友圈 / friends feed</p>
          <h1 className="mt-3 font-nantes text-5xl">买到了，就晒出来。</h1>
        </div>
        <div className="section-index" aria-hidden="true">08 <span>/</span> feed</div>
      </div>

      {notice && <div className={`notice notice-${notice.kind} mt-6`}>{notice.text}</div>}

      <div className="order-tabs mt-8" role="tablist" aria-label="好友圈分区">
        {(
          [
            ["share", "分享"],
            ["feed", "好友动态"],
            ["mine", "我的动态"],
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

      {tab === "share" && <ShareForm onNotice={setNotice} />}
      {tab === "feed" && <FeedList />}
      {tab === "mine" && <MineList onNotice={setNotice} />}
    </section>
  );
}

// ---- 分享 ----

function ShareForm({
  onNotice,
}: {
  onNotice: (n: { kind: "success" | "error"; text: string } | null) => void;
}) {
  const orders = useQuery({
    queryKey: ["orders", "all-for-share"],
    queryFn: () => getOrders({ page: 1, pageSize: 50 }),
  });
  const [skuId, setSkuId] = useState("");
  const [content, setContent] = useState("");
  const [imageFile, setImageFile] = useState<File | null>(null);
  const [imagePreview, setImagePreview] = useState("");
  const fileInput = useRef<HTMLInputElement>(null);
  const preparedImageRef = useRef<PreparedMediaUpload | null>(null);
  const queryClient = useQueryClient();

  useEffect(() => () => {
    if (imagePreview) URL.revokeObjectURL(imagePreview);
  }, [imagePreview]);

  // 已购订单里的 SKU（paid/shipped/completed），按 sku_id 去重。
  const purchased = useMemo(() => {
    const seen = new Set<number>();
    const items: OrderItem[] = [];
    for (const order of orders.data?.orders ?? []) {
      if (!PURCHASED_STATUSES.includes(order.status as OrderStatus)) continue;
      for (const item of order.items) {
        if (seen.has(item.sku_id)) continue;
        seen.add(item.sku_id);
        items.push(item);
      }
    }
    return items;
  }, [orders.data]);

  const share = useMutation({
    mutationFn: async () => {
      return executeMediaSave(
        imageFile,
        preparedImageRef.current,
        (file) => uploadFile(file, "image"),
        (reference) => sharePost({
          sku_id: Number(skuId),
          content: content.trim() || undefined,
          image_url: reference,
        }),
        (prepared) => {
          preparedImageRef.current = prepared;
        },
      );
    },
    onSuccess: () => {
      onNotice({ kind: "success", text: "已分享到好友圈。" });
      setContent("");
      setImageFile(null);
      preparedImageRef.current = null;
      setImagePreview("");
      if (fileInput.current) fileInput.current.value = "";
      setSkuId("");
      queryClient.invalidateQueries({ queryKey: ["feed"] });
      queryClient.invalidateQueries({ queryKey: ["posts", "mine"] });
    },
    onError: (error) => onNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  function pickImage(event: React.ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    const validationError = validateImage(file, "配图");
    if (validationError) {
      onNotice({ kind: "error", text: validationError });
      return;
    }
    setImageFile(file);
    preparedImageRef.current = null;
    setImagePreview(URL.createObjectURL(file));
    onNotice(null);
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    share.mutate();
  }

  if (orders.isPending) return <LoadingBlock label="正在读取已购商品" />;
  if (orders.isError) {
    return <ErrorState message={getApiErrorMessage(orders.error)} onRetry={() => orders.refetch()} />;
  }

  return (
    <form className="share-form mt-8" onSubmit={submit}>
      <h2 className="font-nantes text-2xl">分享一件已购的好物</h2>
      <p className="mt-1 text-sm text-smoke">只能分享已支付 / 已发货 / 已完成的订单商品。</p>

      {purchased.length === 0 ? (
        <div className="mt-6">
          <EmptyState
            eyebrow="还没有可分享的商品"
            title="先去买点什么"
            description="订单完成后，这里会列出可以分享的 SKU。"
          />
        </div>
      ) : (
        <>
          <label className="form-label mt-6">
            <span>选择商品</span>
            <select className="form-control" value={skuId} onChange={(e) => setSkuId(e.target.value)} required>
              <option value="" disabled>请选择已购商品</option>
              {purchased.map((item) => (
                <option key={item.sku_id} value={item.sku_id}>
                  {item.title}（SKU #{item.sku_id}）
                </option>
              ))}
            </select>
          </label>

          <label className="form-label mt-5">
            <span>想说的话（可选，≤500 字）</span>
            <textarea
              className="form-control min-h-28"
              value={content}
              onChange={(e) => setContent(e.target.value)}
              maxLength={500}
              placeholder="用了两周，真香。"
            />
          </label>

          <div className="mt-5">
            <span className="form-label-span">配图（可选）</span>
            {imagePreview ? (
              <div className="share-image-preview">
                <img src={imagePreview} alt="分享配图" />
                <button type="button" className="icon-button" aria-label="移除配图" onClick={() => { setImageFile(null); preparedImageRef.current = null; setImagePreview(""); }}>
                  <Icon name="close" size={16} />
                </button>
              </div>
            ) : (
              <button
                type="button"
                className="button button-secondary button-small mt-2"
                disabled={share.isPending}
                onClick={() => fileInput.current?.click()}
              >
                <><Icon name="image" size={15} /> 选择图片</>
              </button>
            )}
            <input ref={fileInput} type="file" accept={IMAGE_ACCEPT} hidden onChange={pickImage} />
          </div>

          <div className="mt-6 flex items-center gap-4">
            <Button type="submit" disabled={share.isPending || !skuId}>
              {share.isPending ? <Spinner label="上传并分享中" /> : <><Icon name="send" size={16} /> 分享</>}
            </Button>
            <span className="text-sm text-smoke">好友动态只会出现在好友的时间线上。</span>
          </div>
        </>
      )}
    </form>
  );
}

// ---- 好友动态 ----

function FeedList() {
  const [page, setPage] = useState(1);
  const query = useQuery({
    queryKey: ["feed", page],
    queryFn: () => getFeed({ page, pageSize: 10 }),
  });

  return (
    <PostTimeline
      query={query}
      page={page}
      onPageChange={setPage}
      emptyTitle="时间线还是空的"
      emptyDescription="好友分享的动态会出现在这里。"
    />
  );
}

// ---- 我的动态 ----

function MineList({
  onNotice,
}: {
  onNotice: (n: { kind: "success" | "error"; text: string } | null) => void;
}) {
  const [page, setPage] = useState(1);
  const query = useQuery({
    queryKey: ["posts", "mine", page],
    queryFn: () => getMyPosts({ page, pageSize: 10 }),
  });
  const queryClient = useQueryClient();

  const remove = useMutation({
    mutationFn: (postId: number) => deletePost(postId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["posts", "mine"] });
      queryClient.invalidateQueries({ queryKey: ["feed"] });
    },
    onError: (error) => onNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  return (
    <PostTimeline
      query={query}
      page={page}
      onPageChange={setPage}
      emptyTitle="还没有分享过"
      emptyDescription="买完东西，来发第一条动态吧。"
      renderAction={(post) => (
        <Button variant="ghost" className="button-small" disabled={remove.isPending} onClick={() => remove.mutate(post.id)}>
          <Icon name="trash" size={15} /> 删除
        </Button>
      )}
    />
  );
}

function PostTimeline({
  query,
  page,
  onPageChange,
  emptyTitle,
  emptyDescription,
  renderAction,
}: {
  query: { isPending: boolean; isError: boolean; error: unknown; data?: { items: PostView[]; total: number }; refetch: () => void };
  page: number;
  onPageChange: (page: number) => void;
  emptyTitle: string;
  emptyDescription: string;
  renderAction?: (post: PostView) => React.ReactNode;
}) {
  if (query.isPending) return <LoadingBlock label="正在读取动态" />;
  if (query.isError) {
    return <ErrorState message={getApiErrorMessage(query.error)} onRetry={query.refetch} />;
  }
  const items = query.data?.items ?? [];
  const total = query.data?.total ?? 0;
  const pageSize = 10;
  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  if (items.length === 0) {
    return (
      <div className="mt-8">
        <EmptyState eyebrow="好友圈" title={emptyTitle} description={emptyDescription} />
      </div>
    );
  }

  return (
    <div className="mt-8">
      <ul className="post-list">
        {items.map((post) => (
          <PostCard key={post.id} post={post} action={renderAction?.(post)} />
        ))}
      </ul>
      <div className="pagination mt-8">
        <button disabled={page <= 1} onClick={() => onPageChange(page - 1)}>上一页</button>
        <strong>{page}</strong> <span>/</span> {totalPages}
        <button disabled={page >= totalPages} onClick={() => onPageChange(page + 1)}>下一页</button>
      </div>
    </div>
  );
}

function PostCard({ post, action }: { post: PostView; action?: React.ReactNode }) {
  return (
    <li className="post-card">
      <div className="post-card-top">
        <div className="friend-avatar" aria-hidden="true">{post.author_username.slice(0, 1).toUpperCase()}</div>
        <div className="post-card-author">
          <span className="friend-name">{post.author_username}</span>
          <span className="friend-since">{formatDate(post.created_at)}</span>
        </div>
        {action}
      </div>
      {post.content && <p className="post-card-content">{post.content}</p>}
      {post.image_url && (
        <AuthorizedImage className="post-card-image" reference={post.image_url} alt={post.content || "分享配图"} loading="lazy" />
      )}
      <p className="post-card-sku">引用了 <span className="font-mono">SKU #{post.sku_id}</span></p>
    </li>
  );
}
