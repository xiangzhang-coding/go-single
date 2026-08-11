import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { deleteCartItem, getCart, updateCartItem } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import { formatMoney, parseSpecs } from "../lib/format";
import { Button, EmptyState, ErrorState, Icon, LoadingBlock, ProductVisual, QuantityStepper } from "../components/ui";

export function CartPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [actionError, setActionError] = useState("");
  const cartQuery = useQuery({ queryKey: ["cart"], queryFn: getCart });
  const updateMutation = useMutation({
    mutationFn: ({ itemId, quantity }: { itemId: number; quantity: number }) => updateCartItem(itemId, quantity),
    onSuccess: () => {
      setActionError("");
      queryClient.invalidateQueries({ queryKey: ["cart"] });
    },
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });
  const deleteMutation = useMutation({
    mutationFn: (itemId: number) => deleteCartItem(itemId),
    onSuccess: () => {
      setActionError("");
      queryClient.invalidateQueries({ queryKey: ["cart"] });
    },
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });

  if (cartQuery.isPending) {
    return <div className="site-container page-section"><LoadingBlock label="正在读取购物车" /></div>;
  }
  if (cartQuery.isError) {
    return <div className="site-container page-section"><ErrorState message={getApiErrorMessage(cartQuery.error)} onRetry={() => cartQuery.refetch()} /></div>;
  }

  const items = cartQuery.data.items;
  const subtotal = items.reduce((sum, item) => sum + item.price * item.quantity, 0);
  const hasStockIssue = items.some((item) => item.quantity > item.stock || item.stock < 1);
  const isMutating = updateMutation.isPending || deleteMutation.isPending;

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">购物车 / {items.length} 个条目</p>
          <h1 className="mt-3 font-nantes text-5xl">暂存，之后再决定。</h1>
        </div>
        <div className="section-index" aria-hidden="true">02 <span>/</span> cart</div>
      </div>

      {actionError && <div className="notice notice-error mt-8">{actionError}</div>}

      {items.length === 0 ? (
        <div className="mt-10">
          <EmptyState
            eyebrow="购物车是空的"
            title="先去目录里逛逛。"
            description="喜欢的商品会在这里等你，直到你准备好结算。"
            action={<Link to="/"><Button>返回目录 <Icon name="arrow-right" size={16} /></Button></Link>}
          />
        </div>
      ) : (
        <div className="cart-layout mt-10">
          <div className="cart-list">
            {items.map((item) => {
              const stockIssue = item.quantity > item.stock || item.stock < 1;
              return (
                <article className="cart-item" key={item.id}>
                  <Link to={`/products/${item.product_id}`} className="cart-item-visual">
                    <ProductVisual seed={item.product_id} title={item.title} />
                  </Link>
                  <div className="cart-item-copy">
                    <div className="flex items-start justify-between gap-4">
                      <div>
                        <p className="eyebrow text-smoke">SKU / {item.sku_id}</p>
                        <Link to={`/products/${item.product_id}`} className="mt-2 block text-lg text-ink-black hover:underline underline-offset-4">{item.title}</Link>
                        <p className="mt-2 text-sm text-smoke">{parseSpecs(item.specs).map(([key, value]) => `${key}: ${value}`).join(" · ") || "标准规格"}</p>
                      </div>
                      <button type="button" className="icon-button icon-button-subtle" aria-label={`删除 ${item.title}`} onClick={() => deleteMutation.mutate(item.id)} disabled={isMutating}>
                        <Icon name="trash" size={17} />
                      </button>
                    </div>
                    <div className="mt-6 flex flex-wrap items-center justify-between gap-4">
                      <QuantityStepper
                        value={item.quantity}
                        max={99}
                        onChange={(quantity) => updateMutation.mutate({ itemId: item.id, quantity })}
                        disabled={isMutating}
                      />
                      <strong className="text-base">{formatMoney(item.price * item.quantity)}</strong>
                    </div>
                    {stockIssue && <p className="mt-3 text-sm text-charcoal">库存已变化，请把数量调到 {Math.max(item.stock, 0)} 件以内。</p>}
                  </div>
                </article>
              );
            })}
          </div>

          <aside className="summary-card">
            <p className="eyebrow text-smoke">订单预览</p>
            <h2 className="mt-3 font-nantes text-3xl">准备结算</h2>
            <dl className="summary-lines mt-8">
              <div><dt>商品小计</dt><dd>{formatMoney(subtotal)}</dd></div>
              <div><dt>优惠券</dt><dd>结算时选择</dd></div>
              <div className="summary-total"><dt>预计应付</dt><dd>{formatMoney(subtotal)}</dd></div>
            </dl>
            <Button className="mt-8 w-full justify-center" disabled={hasStockIssue} onClick={() => navigate("/checkout")}>
              去结算 <Icon name="arrow-right" size={17} />
            </Button>
            {hasStockIssue && <p className="mt-3 text-xs leading-5 text-smoke">先处理库存数量，才能继续结算。</p>}
            <Link to="/" className="mt-5 block text-center text-sm text-smoke underline underline-offset-4">继续浏览商品</Link>
          </aside>
        </div>
      )}
    </section>
  );
}
