import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { adminApi } from "../../api/endpoints";
import { getApiErrorMessage } from "../../api/client";
import type { FlashSaleActivity } from "../../api/types";
import { formatDate, formatMoney, formatSpecs, toLocalInput } from "../../lib/format";
import { Button, EmptyState, ErrorState, LoadingBlock, Spinner } from "../../components/ui";

const stateLabels: Record<FlashSaleActivity["state"], string> = {
  off_sale: "已下架",
  not_started: "未开始",
  in_progress: "进行中",
  ended: "已结束",
};

export function AdminFlashSalesPage() {
  const queryClient = useQueryClient();
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const [editing, setEditing] = useState<FlashSaleActivity | null>(null);
  const [showCreate, setShowCreate] = useState(false);

  const activitiesQuery = useQuery({
    queryKey: ["admin", "flashsales"],
    queryFn: adminApi.getFlashSales,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["admin", "flashsales"] });

  const toggleMutation = useMutation({
    mutationFn: ({ id, on }: { id: number; on: boolean }) =>
      on ? adminApi.publishFlashSale(id) : adminApi.unpublishFlashSale(id),
    onSuccess: () => {
      setNotice({ kind: "success", text: "活动状态已更新。" });
      invalidate();
    },
    onError: (error) => setNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  const activities = activitiesQuery.data || [];

  return (
    <div className="mt-8">
      {notice && (
        <div className={`notice ${notice.kind === "success" ? "notice-success" : "notice-error"}`}>
          <p>{notice.text}</p>
        </div>
      )}

      <div className="flex justify-end">
        <Button variant="secondary" onClick={() => { setShowCreate(true); setEditing(null); }}>
          + 新建活动
        </Button>
      </div>

      {(showCreate || editing) && (
        <ActivityForm
          initial={editing}
          onDone={() => {
            setShowCreate(false);
            setEditing(null);
            setNotice({ kind: "success", text: editing ? "活动已更新。" : "活动已创建（下架状态），记得上架并预热库存。" });
            invalidate();
          }}
          onError={(text) => setNotice({ kind: "error", text })}
        />
      )}

      {activitiesQuery.isPending ? (
        <div className="mt-6">
          <LoadingBlock label="正在读取活动" />
        </div>
      ) : activitiesQuery.isError ? (
        <div className="mt-6">
          <ErrorState message={getApiErrorMessage(activitiesQuery.error)} onRetry={() => activitiesQuery.refetch()} />
        </div>
      ) : activities.length === 0 ? (
        <div className="mt-6">
          <EmptyState eyebrow="暂无活动" title="还没有秒杀活动。" description="新建一个活动，选好商品与时间窗口后上架即可。" />
        </div>
      ) : (
        <div className="admin-list mt-6">
          {activities.map((activity) => (
            <div key={activity.id} className="admin-card">
              <div className="admin-card-top">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <h3 className="font-nantes text-xl">{activity.title}</h3>
                    <span className="tag">{activity.status === "on_sale" ? "已上架" : "已下架"}</span>
                    <span className={`status-badge admin-badge-${activity.state}`}>{stateLabels[activity.state]}</span>
                  </div>
                  <p className="mt-1 text-xs text-smoke">
                    #{activity.id} · {activity.product_title || `SKU #${activity.sku_id}`}
                    {activity.sku.id ? ` · ${formatSpecs(activity.sku.specs)}` : ""}
                  </p>
                  <p className="mt-1 text-xs text-smoke">
                    {formatDate(activity.start_at)} 至 {formatDate(activity.end_at)}
                  </p>
                </div>
                <div className="flex flex-none items-center gap-3">
                  <div className="text-right">
                    <p className="text-xs text-smoke">秒杀价 / 库存</p>
                    <strong className="text-base font-normal">
                      {formatMoney(activity.price)} <span className="text-xs text-smoke">· 剩 {activity.stock}</span>
                    </strong>
                    <p className="text-xs text-smoke">每人限购 {activity.per_user_limit} 件</p>
                  </div>
                  <Button
                    variant="ghost"
                    size="small"
                    disabled={toggleMutation.isPending || (activity.status !== "on_sale" && activity.state === "ended")}
                    onClick={() => toggleMutation.mutate({ id: activity.id, on: activity.status !== "on_sale" })}
                  >
                    {activity.status === "on_sale"
                      ? "下架"
                      : activity.state === "ended"
                        ? "已结束"
                        : "上架"}
                  </Button>
                  <Button variant="ghost" size="small" onClick={() => { setEditing(activity); setShowCreate(false); }}>
                    编辑
                  </Button>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ActivityForm({
  initial,
  onDone,
  onError,
}: {
  initial?: FlashSaleActivity | null;
  onDone: () => void;
  onError: (text: string) => void;
}) {
  const queryClient = useQueryClient();
  const [productId, setProductId] = useState(initial?.sku.product_id || 0);
  const [form, setForm] = useState({
    sku_id: initial?.sku_id || 0,
    title: initial?.title || "",
    price: initial ? String(initial.price) : "",
    stock: initial ? String(initial.stock) : "",
    per_user_limit: initial ? String(initial.per_user_limit) : "1",
    start_at: initial ? toLocalInput(initial.start_at) : "",
    end_at: initial ? toLocalInput(initial.end_at) : "",
  });

  const productsQuery = useQuery({
    queryKey: ["admin", "products", "picker"],
    queryFn: () => adminApi.getProducts({ page: 1, pageSize: 50 }),
  });

  const detailQuery = useQuery({
    queryKey: ["admin", "product-detail", productId],
    queryFn: () => adminApi.getProductDetail(productId),
    enabled: productId > 0,
  });

  const products = productsQuery.data?.items || [];
  const skus = detailQuery.data?.skus || [];
  const selectedProduct = useMemo(() => products.find((p) => p.id === productId), [products, productId]);

  const mutation = useMutation({
    mutationFn: () => {
      const request = {
        sku_id: form.sku_id,
        title: form.title.trim(),
        price: Number(form.price),
        stock: Number(form.stock),
        per_user_limit: Number(form.per_user_limit) || 1,
        start_at: new Date(form.start_at).toISOString(),
        end_at: new Date(form.end_at).toISOString(),
      };
      return initial
        ? adminApi.updateFlashSale(initial.id, request).then(() => undefined)
        : adminApi.createFlashSale(request).then(() => undefined);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["admin", "products", "picker"] });
      onDone();
    },
    onError: (error) => onError(getApiErrorMessage(error)),
  });

  const pickProduct = (id: number) => {
    setProductId(id);
    setForm((f) => ({ ...f, sku_id: 0 }));
  };

  const pickSKU = (skuId: number) => {
    const sku = skus.find((s) => s.id === skuId);
    setForm((f) => ({
      ...f,
      sku_id: skuId,
      title: f.title || selectedProduct?.title || "",
      price: sku ? String(sku.price) : f.price,
    }));
  };

  return (
    <form
      className="admin-form-panel mt-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (!form.sku_id) {
          onError("请先选择商品与 SKU。");
          return;
        }
        if (!form.title || !form.price || !form.stock) {
          onError("请填写标题、秒杀价与库存。");
          return;
        }
        if (!form.start_at || !form.end_at || new Date(form.start_at) >= new Date(form.end_at)) {
          onError("时间窗口无效：开始时间需早于结束时间。");
          return;
        }
        mutation.mutate();
      }}
    >
      <div className="form-grid-2">
        <label className="form-label">
          商品
          <select className="form-control" value={productId} onChange={(e) => pickProduct(Number(e.target.value))}>
            <option value={0}>选择商品</option>
            {products.map((p) => (
              <option key={p.id} value={p.id}>{p.title}</option>
            ))}
          </select>
        </label>
        <label className="form-label">
          SKU
          <select
            className="form-control"
            value={form.sku_id}
            onChange={(e) => pickSKU(Number(e.target.value))}
            disabled={!productId}
          >
            <option value={0}>{productId ? "选择规格" : "先选商品"}</option>
            {skus.map((sku) => (
              <option key={sku.id} value={sku.id}>
                {formatSpecs(sku.specs)} · {formatMoney(sku.price)} · 库存 {sku.stock}
              </option>
            ))}
          </select>
        </label>
        <label className="form-label">
          活动标题
          <input
            className="form-control"
            value={form.title}
            onChange={(e) => setForm({ ...form, title: e.target.value })}
            placeholder="如：周末限时秒杀"
          />
        </label>
        <label className="form-label">
          秒杀价（分）
          <input
            className="form-control"
            type="number"
            min={0}
            value={form.price}
            onChange={(e) => setForm({ ...form, price: e.target.value })}
            placeholder="9900"
          />
        </label>
        <label className="form-label">
          活动库存
          <input
            className="form-control"
            type="number"
            min={0}
            value={form.stock}
            onChange={(e) => setForm({ ...form, stock: e.target.value })}
            placeholder="100"
          />
        </label>
        <label className="form-label">
          每人限购
          <input
            className="form-control"
            type="number"
            min={1}
            value={form.per_user_limit}
            onChange={(e) => setForm({ ...form, per_user_limit: e.target.value })}
          />
        </label>
        <label className="form-label">
          开始时间
          <input
            className="form-control"
            type="datetime-local"
            value={form.start_at}
            onChange={(e) => setForm({ ...form, start_at: e.target.value })}
          />
        </label>
        <label className="form-label">
          结束时间
          <input
            className="form-control"
            type="datetime-local"
            value={form.end_at}
            onChange={(e) => setForm({ ...form, end_at: e.target.value })}
          />
        </label>
      </div>
      <div className="mt-4 flex gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? <Spinner label="保存中" /> : initial ? "保存修改" : "创建活动"}
        </Button>
        {!initial && (
          <Button variant="ghost" type="button" onClick={onDone}>取消</Button>
        )}
      </div>
    </form>
  );
}
