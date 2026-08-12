import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { adminApi } from "../../api/endpoints";
import { getApiErrorMessage } from "../../api/client";
import type { OrderView } from "../../api/types";
import { formatAddress, formatDate, formatMoney, formatSpecs } from "../../lib/format";
import { Button, EmptyState, ErrorState, LoadingBlock, StatusBadge } from "../../components/ui";

const orderTabs: Array<{ value: string; label: string }> = [
  { value: "", label: "全部" },
  { value: "pending_payment", label: "待支付" },
  { value: "paid", label: "已支付" },
  { value: "shipped", label: "已发货" },
  { value: "completed", label: "已完成" },
  { value: "cancelled", label: "已取消" },
];

const orderTypeLabels: Record<string, string> = {
  normal: "普通订单",
  seckill: "秒杀订单",
};

export function AdminOrdersPage() {
  const queryClient = useQueryClient();
  const [status, setStatus] = useState("");
  const [page, setPage] = useState(1);
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  const ordersQuery = useQuery({
    queryKey: ["admin", "orders", status, page],
    queryFn: () => adminApi.getOrders({ status, page, pageSize: 10 }),
  });

  const shipMutation = useMutation({
    mutationFn: adminApi.shipOrder,
    onSuccess: () => {
      setNotice({ kind: "success", text: "订单已发货。" });
      queryClient.invalidateQueries({ queryKey: ["admin", "orders"] });
    },
    onError: (error) => setNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  const orders = ordersQuery.data?.orders || [];
  const total = ordersQuery.data?.total || 0;

  return (
    <div className="mt-8">
      {notice && (
        <div className={`notice ${notice.kind === "success" ? "notice-success" : "notice-error"}`}>
          <p>{notice.text}</p>
        </div>
      )}

      <div className="admin-tabs admin-tabs-small" role="tablist" aria-label="订单状态筛选">
        {orderTabs.map((tab) => (
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

      {ordersQuery.isPending ? (
        <div className="mt-6">
          <LoadingBlock label="正在读取订单" />
        </div>
      ) : ordersQuery.isError ? (
        <div className="mt-6">
          <ErrorState message={getApiErrorMessage(ordersQuery.error)} onRetry={() => ordersQuery.refetch()} />
        </div>
      ) : orders.length === 0 ? (
        <div className="mt-6">
          <EmptyState eyebrow="暂无订单" title="这个状态下还没有订单。" description="换个状态筛选看看，或等用户下单后再来处理。" />
        </div>
      ) : (
        <div className="admin-list mt-6">
          {orders.map((order) => (
            <AdminOrderCard key={order.order_no} order={order} shipping={shipMutation.isPending} onShip={() => shipMutation.mutate(order.order_no)} />
          ))}
        </div>
      )}

      {total > 10 && (
        <div className="mt-6 flex items-center justify-between gap-4">
          <p className="text-xs text-smoke">
            共 {total} 单 · 第 {page} / {Math.max(1, Math.ceil(total / 10))} 页
          </p>
          <div className="flex gap-2">
            <Button variant="ghost" size="small" disabled={page <= 1} onClick={() => setPage(page - 1)}>
              上一页
            </Button>
            <Button variant="ghost" size="small" disabled={page >= Math.ceil(total / 10)} onClick={() => setPage(page + 1)}>
              下一页
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

function AdminOrderCard({ order, shipping, onShip }: { order: OrderView; shipping: boolean; onShip: () => void }) {
  return (
    <div className="admin-card">
      <div className="admin-card-top">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="font-nantes text-lg">#{order.order_no}</h3>
            <span className="tag">{orderTypeLabels[order.order_type] || order.order_type}</span>
            <StatusBadge status={order.status} />
          </div>
          <p className="mt-1 text-xs text-smoke">
            用户 #{order.user_id} · {formatDate(order.created_at)}
          </p>
        </div>
        <div className="flex flex-none items-center gap-3">
          <div className="text-right">
            <p className="text-xs text-smoke">应付金额</p>
            <strong className="text-base font-normal">{formatMoney(order.pay_amount)}</strong>
            {order.discount_amount > 0 && (
              <p className="text-xs text-smoke">已优惠 {formatMoney(order.discount_amount)}</p>
            )}
          </div>
          {order.status === "paid" && (
            <Button size="small" disabled={shipping} onClick={onShip}>
              发货
            </Button>
          )}
        </div>
      </div>

      <div className="admin-card-body">
        <div className="min-w-0 flex-1">
          {order.items.map((item) => (
            <div key={item.id} className="flex items-baseline justify-between gap-4 py-2 text-sm">
              <span className="min-w-0 truncate">
                {item.title} <span className="text-xs text-smoke">· {formatSpecs(item.specs)}</span>
              </span>
              <span className="flex-none text-xs text-smoke">
                {formatMoney(item.price)} × {item.quantity}
              </span>
            </div>
          ))}
        </div>
        <div className="w-64 flex-none border-l border-ash pl-4 text-xs leading-5 text-smoke">
          <p>{formatAddress(order)}</p>
          <p className="mt-1">{order.receiver} · {order.phone}</p>
        </div>
      </div>
    </div>
  );
}
