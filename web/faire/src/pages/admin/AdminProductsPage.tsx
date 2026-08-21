import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { adminApi } from "../../api/endpoints";
import { getApiErrorMessage } from "../../api/client";
import type { Product, SKU } from "../../api/types";
import { formatMoney } from "../../lib/format";
import { Button, EmptyState, ErrorState, LoadingBlock, Spinner } from "../../components/ui";

const statusLabels: Record<string, string> = {
  on_sale: "已上架",
  off_sale: "已下架",
};

const productTabs: Array<{ value: string; label: string }> = [
  { value: "", label: "全部" },
  { value: "on_sale", label: "已上架" },
  { value: "off_sale", label: "已下架" },
];

const emptyProductForm = { category_id: 0, title: "", description: "" };
const emptySKUForm = { specs: "", price: "", stock: "" };

export function AdminProductsPage() {
  const queryClient = useQueryClient();
  const [status, setStatus] = useState("");
  const [page, setPage] = useState(1);
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const [editing, setEditing] = useState<Product | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [expandedSKU, setExpandedSKU] = useState<number | null>(null);

  const categoriesQuery = useQuery({ queryKey: ["admin", "categories"], queryFn: adminApi.getCategories });
  const productsQuery = useQuery({
    queryKey: ["admin", "products", status, page],
    queryFn: () => adminApi.getProducts({ status, page, pageSize: 10 }),
  });

  const products = productsQuery.data?.items || [];
  const total = productsQuery.data?.total || 0;

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["admin", "products"] });
    queryClient.invalidateQueries({ queryKey: ["admin", "categories"] });
  };

  const toggleMutation = useMutation({
    mutationFn: ({ id, on }: { id: number; on: boolean }) =>
      on ? adminApi.publishProduct(id) : adminApi.unpublishProduct(id),
    onSuccess: () => {
      setNotice({ kind: "success", text: "商品状态已更新。" });
      invalidate();
    },
    onError: (error) => setNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  const deleteCategoryMutation = useMutation({
    mutationFn: adminApi.deleteCategory,
    onSuccess: () => {
      setNotice({ kind: "success", text: "类目已删除。" });
      invalidate();
    },
    onError: (error) => setNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  return (
    <div className="mt-8 space-y-10">
      {notice && (
        <div className={`notice ${notice.kind === "success" ? "notice-success" : "notice-error"}`}>
          <p>{notice.text}</p>
        </div>
      )}

      <CategorySection categories={categoriesQuery} onDelete={(id) => deleteCategoryMutation.mutate(id)} />

      <section>
        <div className="flex items-end justify-between gap-6">
          <h2 className="text-subheading">商品（SPU）</h2>
          <div className="admin-tabs admin-tabs-small" role="tablist" aria-label="商品状态筛选">
            {productTabs.map((tab) => (
              <button
                key={tab.label}
                type="button"
                role="tab"
                aria-selected={status === tab.value}
                className={status === tab.value ? "active" : ""}
                onClick={() => {
                  setStatus(tab.value);
                  setPage(1);
                }}
              >
                {tab.label}
              </button>
            ))}
          </div>
        </div>

        <div className="mt-4 flex justify-end">
          <Button variant="secondary" onClick={() => { setShowCreate(true); setEditing(null); }}>
            + 新建商品
          </Button>
        </div>

        {showCreate && (
          <ProductForm
            categories={categoriesQuery.data || []}
            onDone={() => {
              setShowCreate(false);
              setNotice({ kind: "success", text: "商品已创建（默认下架），可继续为其添加 SKU。" });
              invalidate();
            }}
            onError={(text) => setNotice({ kind: "error", text })}
          />
        )}

        {productsQuery.isPending ? (
          <div className="mt-6">
            <LoadingBlock label="正在读取商品" />
          </div>
        ) : productsQuery.isError ? (
          <div className="mt-6">
            <ErrorState message={getApiErrorMessage(productsQuery.error)} onRetry={() => productsQuery.refetch()} />
          </div>
        ) : products.length === 0 ? (
          <div className="mt-6">
            <EmptyState eyebrow="暂无商品" title="这个筛选下还没有商品。" description="新建一个商品，或者换一个状态筛选看看。" />
          </div>
        ) : (
          <div className="admin-list mt-6">
            {products.map((product) => (
              <div key={product.id} className="admin-card">
                <div className="admin-card-top">
                  <div className="min-w-0">
                    <h3 className="line-clamp-1 font-nantes text-xl">{product.title}</h3>
                    <p className="mt-1 text-xs text-smoke">
                      #{product.id} · {categoriesQuery.data?.find((c) => c.id === product.category_id)?.name || `类目 #${product.category_id}`}
                    </p>
                  </div>
                  <div className="flex flex-none items-center gap-2">
                    <span className={`status-badge ${product.status === "on_sale" ? "admin-badge-on" : "admin-badge-off"}`}>
                      {statusLabels[product.status]}
                    </span>
                    <Button
                      variant="ghost"
                      size="small"
                      onClick={() => toggleMutation.mutate({ id: product.id, on: product.status !== "on_sale" })}
                      disabled={toggleMutation.isPending}
                    >
                      {product.status === "on_sale" ? "下架" : "上架"}
                    </Button>
                    <Button
                      variant="ghost"
                      size="small"
                      onClick={() => { setEditing(product); setShowCreate(false); }}
                    >
                      编辑
                    </Button>
                    <Button
                      variant="ghost"
                      size="small"
                      onClick={() => setExpandedSKU(expandedSKU === product.id ? null : product.id)}
                    >
                      {expandedSKU === product.id ? "收起 SKU" : "SKU 管理"}
                    </Button>
                  </div>
                </div>

                {product.description && (
                  <p className="px-5 pb-4 text-sm text-smoke">{product.description}</p>
                )}

                {editing?.id === product.id && (
                  <ProductForm
                    initial={product}
                    categories={categoriesQuery.data || []}
                    onDone={() => {
                      setEditing(null);
                      setNotice({ kind: "success", text: "商品已更新。" });
                      invalidate();
                    }}
                    onError={(text) => setNotice({ kind: "error", text })}
                  />
                )}

                {expandedSKU === product.id && <SKUSection productId={product.id} onNotice={setNotice} />}
              </div>
            ))}
          </div>
        )}

        {total > 10 && (
          <Pagination page={page} total={total} pageSize={10} onPage={setPage} />
        )}
      </section>
    </div>
  );
}

function CategorySection({
  categories,
  onDelete,
}: {
  categories: { data?: Array<{ id: number; name: string }> };
  onDelete: (id: number) => void;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [editing, setEditing] = useState<{ id: number; name: string } | null>(null);
  const [error, setError] = useState("");

  const saveMutation = useMutation({
    mutationFn: () =>
      editing
        ? adminApi.updateCategory(editing.id, name)
        : adminApi.createCategory(name).then(() => undefined),
    onSuccess: () => {
      setName("");
      setEditing(null);
      setError("");
      queryClient.invalidateQueries({ queryKey: ["admin", "categories"] });
    },
    onError: (e) => setError(getApiErrorMessage(e)),
  });

  const list = categories.data || [];
  return (
    <section>
      <h2 className="text-subheading">类目</h2>
      <div className="mt-4 flex flex-wrap items-center gap-2">
        {list.map((category) =>
          editing?.id === category.id ? (
            <form
              key={category.id}
              className="inline-flex items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (name.trim()) saveMutation.mutate();
              }}
            >
              <input
                className="form-control !min-h-0 !w-40 !py-2"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="类目名称"
                autoFocus
              />
              <Button variant="primary" size="small" disabled={saveMutation.isPending}>
                保存
              </Button>
              <Button variant="ghost" size="small" onClick={() => setEditing(null)}>
                取消
              </Button>
            </form>
          ) : (
            <span key={category.id} className="tag inline-flex items-center gap-2">
              {category.name}
              <button
                type="button"
                className="text-ink-black/60 hover:text-ink-black"
                aria-label={`编辑类目 ${category.name}`}
                onClick={() => { setEditing({ id: category.id, name: category.name }); setName(category.name); }}
              >
                改
              </button>
              <button
                type="button"
                className="text-ink-black/60 hover:text-ink-black"
                aria-label={`删除类目 ${category.name}`}
                onClick={() => onDelete(category.id)}
              >
                删
              </button>
            </span>
          ),
        )}
        <form
          className="inline-flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim()) saveMutation.mutate();
          }}
        >
          <input
            className="form-control !min-h-0 !w-40 !py-2"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="新建类目名称"
          />
          <Button variant="secondary" size="small" disabled={saveMutation.isPending}>
            新建
          </Button>
        </form>
      </div>
      {error && <p className="mt-2 text-xs text-smoke">{error}</p>}
    </section>
  );
}

function ProductForm({
  initial,
  categories,
  onDone,
  onError,
}: {
  initial?: Product;
  categories: Array<{ id: number; name: string }>;
  onDone: () => void;
  onError: (text: string) => void;
}) {
  const [form, setForm] = useState(
    initial
      ? { category_id: initial.category_id, title: initial.title, description: initial.description }
      : emptyProductForm,
  );

  const mutation = useMutation({
    mutationFn: () =>
      initial
        ? adminApi.updateProduct(initial.id, form).then(() => undefined)
        : adminApi.createProduct(form).then(() => undefined),
    onSuccess: onDone,
    onError: (error) => onError(getApiErrorMessage(error)),
  });

  return (
    <form
      className="admin-form-panel mt-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (!form.category_id || !form.title.trim()) {
          onError("请选择类目并填写商品标题。");
          return;
        }
        mutation.mutate();
      }}
    >
      <div className="form-grid-2">
        <label className="form-label">
          类目
          <select
            className="form-control"
            value={form.category_id}
            onChange={(e) => setForm({ ...form, category_id: Number(e.target.value) })}
          >
            <option value={0}>选择类目</option>
            {categories.map((c) => (
              <option key={c.id} value={c.id}>{c.name}</option>
            ))}
          </select>
        </label>
        <label className="form-label">
          标题
          <input
            className="form-control"
            value={form.title}
            onChange={(e) => setForm({ ...form, title: e.target.value })}
            placeholder="商品标题"
          />
        </label>
        <label className="form-label sm:col-span-2">
          详情
          <textarea
            className="form-control min-h-20"
            value={form.description}
            onChange={(e) => setForm({ ...form, description: e.target.value })}
            placeholder="商品详情（可选）"
          />
        </label>
      </div>
      <div className="mt-4 flex gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? <Spinner label="保存中" /> : initial ? "保存修改" : "创建商品"}
        </Button>
        {!initial && (
          <Button variant="ghost" onClick={() => setForm(emptyProductForm)}>清空</Button>
        )}
      </div>
    </form>
  );
}

function SKUSection({ productId, onNotice }: { productId: number; onNotice: (n: { kind: "success" | "error"; text: string }) => void }) {
  const queryClient = useQueryClient();
  const [editingSKU, setEditingSKU] = useState<SKU | null>(null);

  const detailQuery = useQuery({
    queryKey: ["admin", "product-detail", productId],
    queryFn: () => adminApi.getProductDetail(productId),
  });

  const skus = detailQuery.data?.skus || [];

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["admin", "product-detail", productId] });
  };

  const deleteMutation = useMutation({
    mutationFn: adminApi.deleteSKU,
    onSuccess: () => {
      onNotice({ kind: "success", text: "SKU 已删除。" });
      refresh();
    },
    onError: (error) => onNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  return (
    <div className="admin-sku-panel">
      <p className="eyebrow text-smoke">SKU / 规格 · 价格 · 库存</p>
      {detailQuery.isPending ? (
        <Spinner label="读取 SKU 中" />
      ) : detailQuery.isError ? (
        <ErrorState message={getApiErrorMessage(detailQuery.error)} onRetry={() => detailQuery.refetch()} />
      ) : skus.length === 0 ? (
        <p className="mt-2 text-sm text-smoke">还没有 SKU，添加一个规格组合即可上架售卖。</p>
      ) : (
        <div className="admin-sku-table mt-3">
          <div className="admin-sku-row admin-sku-head" aria-hidden="true">
            <span>规格</span>
            <span>价格</span>
            <span>库存</span>
            <span>操作</span>
          </div>
          {skus.map((sku) =>
            editingSKU?.id === sku.id ? (
              <SKUForm
                key={sku.id}
                initial={sku}
                productId={productId}
                onDone={() => {
                  setEditingSKU(null);
                  onNotice({ kind: "success", text: "SKU 已更新。" });
                  refresh();
                }}
                onError={(text) => onNotice({ kind: "error", text })}
              />
            ) : (
              <div key={sku.id} className="admin-sku-row">
                <span className="truncate">{formatSpecsText(sku.specs)}</span>
                <span>{formatMoney(sku.price)}</span>
                <span>{sku.stock}</span>
                <span className="flex gap-2">
                  <button type="button" className="text-link" onClick={() => setEditingSKU(sku)}>编辑</button>
                  <button type="button" className="text-link" onClick={() => deleteMutation.mutate(sku.id)}>删除</button>
                </span>
              </div>
            ),
          )}
        </div>
      )}
      {!editingSKU && (
        <div className="mt-4">
          <SKUForm productId={productId} onDone={refresh} onError={(text) => onNotice({ kind: "error", text })} />
        </div>
      )}
    </div>
  );
}

function SKUForm({
  productId,
  initial,
  onDone,
  onError,
}: {
  productId: number;
  initial?: SKU;
  onDone: () => void;
  onError: (text: string) => void;
}) {
  const [form, setForm] = useState(
    initial
      ? { specs: JSON.stringify(initial.specs ?? {}), price: String(initial.price), stock: String(initial.stock) }
      : emptySKUForm,
  );

  const mutation = useMutation({
    mutationFn: () => {
      const request = {
        specs: JSON.parse(form.specs || "{}"),
        price: Number(form.price),
        stock: Number(form.stock),
      };
      return initial
        ? adminApi.updateSKU(initial.id, { ...request, expected_stock: initial.stock }).then(() => undefined)
        : adminApi.createSKU(productId, request).then(() => undefined);
    },
    onSuccess: () => {
      if (!initial) setForm(emptySKUForm);
      onDone();
    },
    onError: (error) => onError(getApiErrorMessage(error)),
  });

  return (
    <form
      className="admin-sku-form"
      onSubmit={(e) => {
        e.preventDefault();
        try {
          JSON.parse(form.specs || "{}");
        } catch {
          onError("规格必须是合法 JSON 对象，如 {\"color\":\"红\"}。");
          return;
        }
        if (!form.price || !form.stock) {
          onError("请填写价格与库存。");
          return;
        }
        mutation.mutate();
      }}
    >
      <label className="form-label">
        规格 JSON
        <input
          className="form-control"
          value={form.specs}
          onChange={(e) => setForm({ ...form, specs: e.target.value })}
          placeholder={`{"color":"红","size":"M"}`}
        />
      </label>
      <label className="form-label">
        价格（分）
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
        库存
        <input
          className="form-control"
          type="number"
          min={0}
          value={form.stock}
          onChange={(e) => setForm({ ...form, stock: e.target.value })}
          placeholder="10"
        />
      </label>
      <div className="flex items-end gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? <Spinner label="保存中" /> : initial ? "保存 SKU" : "添加 SKU"}
        </Button>
        {initial && (
          <Button variant="ghost" onClick={() => onDone()}>取消</Button>
        )}
      </div>
    </form>
  );
}

function formatSpecsText(value: unknown): string {
  if (typeof value === "string") {
    try {
      const parsed = JSON.parse(value) as unknown;
      return Object.entries(parsed as Record<string, unknown>).map(([k, v]) => `${k}: ${String(v)}`).join(" · ") || value;
    } catch {
      return value;
    }
  }
  if (!value || typeof value !== "object") {
    return "标准规格";
  }
  return Object.entries(value as Record<string, unknown>).map(([k, v]) => `${k}: ${String(v)}`).join(" · ");
}

function Pagination({ page, total, pageSize, onPage }: { page: number; total: number; pageSize: number; onPage: (p: number) => void }) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  return (
    <div className="mt-6 flex items-center justify-between gap-4">
      <p className="text-xs text-smoke">
        共 {total} 件 · 第 {page} / {pages} 页
      </p>
      <div className="flex gap-2">
        <Button variant="ghost" size="small" disabled={page <= 1} onClick={() => onPage(page - 1)}>
          上一页
        </Button>
        <Button variant="ghost" size="small" disabled={page >= pages} onClick={() => onPage(page + 1)}>
          下一页
        </Button>
      </div>
    </div>
  );
}
