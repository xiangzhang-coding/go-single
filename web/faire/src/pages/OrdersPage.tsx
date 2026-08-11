import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { getOrders } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { OrderStatus, OrderView } from "../api/types";
import { formatDate, formatMoney, formatSpecs } from "../lib/format";
import { Button, EmptyState, ErrorState, Icon, LoadingBlock, ProductVisual, StatusBadge } from "../components/ui";

const tabs: Array<{ value: "all" | OrderStatus; label: string }> = [
  { value: "all", label: "全部" },
  { value: "pending_payment", label: "待支付" },
  { value: "paid", label: "已支付" },
  { value: "shipped", label: "已发货" },
  { value: "completed", label: "已完成" },
  { value: "cancelled", label: "已取消" },
];

export function OrdersPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const page = Math.max(1, Number(searchParams.get("page")) || 1);
  const rawStatus = searchParams.get("status") as OrderStatus | null;
  const status: "all" | OrderStatus = rawStatus && tabs.some((tab) => tab.value === rawStatus) ? rawStatus : "all";
  const ordersQuery = useQuery({
    queryKey: ["orders", status, page],
    queryFn: () => getOrders({ status: status === "all" ? undefined : status, page, pageSize: 8 }),
  });
  const orders = ordersQuery.data?.orders || [];
  const total = ordersQuery.data?.total || 0;
  const totalPages = Math.max(1, Math.ceil(total / 8));

  function chooseStatus(nextStatus: "all" | OrderStatus) {
    const next = new URLSearchParams(searchParams);
    if (nextStatus === "all") {
      next.delete("status");
    } else {
      next.set("status", nextStatus);
    }
    next.delete("page");
    setSearchParams(next);
  }

  function changePage(nextPage: number) {
    const safePage = Math.min(totalPages, Math.max(1, nextPage));
    const next = new URLSearchParams(searchParams);
    if (safePage === 1) next.delete("page");
    else next.set("page", String(safePage));
    setSearchParams(next);
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">订单 / {total} 条记录</p>
          <h1 className="mt-3 font-nantes text-5xl">每一次下单，都有去处。</h1>
        </div>
        <div className="section-index" aria-hidden="true">04 <span>/</span> orders</div>
      </div>

      <div className="order-tabs mt-10" role="tablist" aria-label="订单状态筛选">
        {tabs.map((tab) => <button key={tab.value} type="button" role="tab" aria-selected={status === tab.value} className={status === tab.value ? "active" : ""} onClick={() => chooseStatus(tab.value)}>{tab.label}</button>)}
      </div>

      {ordersQuery.isPending ? (
        <LoadingBlock label="正在读取订单" />
      ) : ordersQuery.isError ? (
        <div className="mt-8"><ErrorState message={getApiErrorMessage(ordersQuery.error)} onRetry={() => ordersQuery.refetch()} /></div>
      ) : orders.length === 0 ? (
        <div className="mt-8"><EmptyState eyebrow="还没有订单" title="你的购买记录会出现在这里。" description="回到商品目录，选一件真正想带回家的东西。" action={<Link to="/"><Button>返回目录 <Icon name="arrow-right" size={16} /></Button></Link>} /></div>
      ) : (
        <div className="orders-list mt-8">
          {orders.map((order) => <OrderCard key={order.order_no} order={order} />)}
        </div>
      )}

      {totalPages > 1 && <div className="pagination mt-10"><button type="button" disabled={page <= 1} onClick={() => changePage(page - 1)} aria-label="上一页"><Icon name="arrow-left" size={17} /></button><span><strong>{page}</strong> / {totalPages}</span><button type="button" disabled={page >= totalPages} onClick={() => changePage(page + 1)} aria-label="下一页"><Icon name="arrow-right" size={17} /></button></div>}
    </section>
  );
}

function OrderCard({ order }: { order: OrderView }) {
  const firstItem = order.items[0];
  const remaining = Math.max(0, order.items.length - 1);
  return (
    <article className="order-card">
      <div className="order-card-top">
        <div><p className="eyebrow text-smoke">下单时间 / {formatDate(order.created_at)}</p><p className="mt-2 text-sm text-charcoal">订单号 <span className="font-mono text-xs">{order.order_no}</span></p></div>
        <StatusBadge status={order.status} />
      </div>
      <div className="order-card-body">
        {firstItem && <ProductVisual seed={firstItem.product_id} title={firstItem.title} />}
        <div className="min-w-0 flex-1"><h2 className="text-lg">{firstItem?.title || "订单商品"}</h2><p className="mt-2 text-sm text-smoke">{firstItem ? `${formatSpecs(firstItem.specs)} × ${firstItem.quantity}` : "订单正在准备中"}{remaining > 0 && ` · 另有 ${remaining} 件商品`}</p></div>
        <div className="order-card-money"><span>应付金额</span><strong>{formatMoney(order.pay_amount)}</strong></div>
      </div>
      <div className="order-card-bottom"><span className="text-sm text-smoke">{order.order_type === "seckill" ? "秒杀订单" : "普通订单"}</span><Link to={`/orders/${order.order_no}`} className="button button-secondary button-small">查看订单 <Icon name="arrow-right" size={15} /></Link></div>
    </article>
  );
}
