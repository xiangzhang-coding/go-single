import { useEffect, useState } from "react";
import { Link, useLocation, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { cancelOrder, confirmOrder, getOrder, mockPay } from "../api/endpoints";
import { ApiRequestError, getApiErrorMessage } from "../api/client";
import { formatAddress, formatDate, formatMoney, formatSpecs } from "../lib/format";
import { shouldRetryOrderDetail } from "../lib/order";
import { clearCheckoutOperationByOrderNo, isCheckoutOrderProcessing } from "../lib/pending-operations";
import { Button, ErrorState, Icon, LoadingBlock, ProductVisual, Spinner, StatusBadge } from "../components/ui";
import { useAuthStore } from "../store/auth";

export function OrderDetailPage() {
  const { orderNo = "" } = useParams();
  const location = useLocation();
  const queryClient = useQueryClient();
  const userId = useAuthStore((state) => state.user?.id ?? 0);
  const [actionError, setActionError] = useState("");
  const awaitingCreation = Boolean((location.state as { awaitingCreation?: boolean } | null)?.awaitingCreation)
    || (Boolean(orderNo && userId) && isCheckoutOrderProcessing(userId, orderNo));
  const orderQuery = useQuery({
    queryKey: ["order", orderNo],
    queryFn: () => getOrder(orderNo),
    enabled: Boolean(orderNo),
    retry: (failureCount, error) => shouldRetryOrderDetail(
      failureCount,
      error instanceof ApiRequestError ? error.status : undefined,
      awaitingCreation,
    ),
    retryDelay: 1500,
  });
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["order", orderNo] });
    queryClient.invalidateQueries({ queryKey: ["orders"] });
  };
  useEffect(() => {
    if (orderQuery.data && userId) clearCheckoutOperationByOrderNo(userId, orderNo);
  }, [orderNo, orderQuery.data, userId]);
  const payMutation = useMutation({
    mutationFn: (result: "success" | "fail") => mockPay(orderNo, orderQuery.data!.pay_amount, result),
    onSuccess: (payment) => {
      setActionError(payment.result === "fail" ? "模拟支付失败，订单仍可重试。" : "支付成功，订单状态已更新。");
      refresh();
    },
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });
  const cancelMutation = useMutation({
    mutationFn: () => cancelOrder(orderNo),
    onSuccess: refresh,
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });
  const confirmMutation = useMutation({
    mutationFn: () => confirmOrder(orderNo),
    onSuccess: refresh,
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });

  if (orderQuery.isPending) {
    if (awaitingCreation) {
      return <section className="site-container page-section"><div className="processing-panel"><span className="processing-mark"><Icon name="clock" size={23} /></span><div><h1 className="font-nantes text-3xl">订单正在生成。</h1><p className="mt-2 text-sm leading-6 text-smoke">安全的重复提交仍在落库，页面会继续查询结果。</p></div><Spinner label="每 1.5 秒更新" /></div></section>;
    }
    return <div className="site-container page-section"><LoadingBlock label="正在读取订单详情" /></div>;
  }
  if (orderQuery.isError || !orderQuery.data) {
    return <div className="site-container page-section"><ErrorState message={getApiErrorMessage(orderQuery.error, "订单不存在或暂时无法读取")} onRetry={() => orderQuery.refetch()} /></div>;
  }

  const order = orderQuery.data;
  const isPending = order.status === "pending_payment";
  const busy = payMutation.isPending || cancelMutation.isPending || confirmMutation.isPending;

  function pay(result: "success" | "fail") {
    setActionError("");
    payMutation.mutate(result);
  }

  function cancel() {
    if (window.confirm("确定取消这个待支付订单吗？库存和优惠券会按服务端事务回退。")) {
      setActionError("");
      cancelMutation.mutate();
    }
  }

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <Link to="/orders" className="back-link"><Icon name="arrow-left" size={16} /> 返回订单列表</Link>
      <div className="section-heading-row mt-8 sm:mt-12">
        <div><p className="eyebrow text-smoke">订单详情 / {order.order_type === "seckill" ? "秒杀" : "普通"}订单</p><h1 className="mt-3 font-nantes text-5xl">这一单，已被记下。</h1><p className="mt-3 font-mono text-xs text-smoke">{order.order_no}</p></div>
        <StatusBadge status={order.status} />
      </div>

      {actionError && <div className={`notice mt-8 ${actionError.includes("成功") ? "notice-success" : "notice-error"}`}>{actionError}</div>}

      <div className="order-detail-layout mt-10">
          <div className="order-detail-main">
            <section className="detail-panel">
              <div className="detail-panel-heading"><div><p className="eyebrow text-smoke">商品明细</p><h2 className="mt-2 font-nantes text-3xl">你选择的东西</h2></div><span className="text-sm text-smoke">{order.items.length} 件</span></div>
              <div className="order-items mt-6">{order.items.map((item) => <div className="order-item" key={item.id}><ProductVisual seed={item.product_id} title={item.title} /><div className="min-w-0 flex-1"><h3 className="text-base">{item.title}</h3><p className="mt-2 text-sm text-smoke">{formatSpecs(item.specs)} × {item.quantity}</p></div><strong>{formatMoney(item.subtotal)}</strong></div>)}</div>
            </section>
            <section className="detail-panel mt-8"><div className="detail-panel-heading"><div><p className="eyebrow text-smoke">地址快照</p><h2 className="mt-2 font-nantes text-3xl">寄到这里</h2></div><Icon name="pin" size={20} /></div><div className="saved-address mt-6"><strong>{order.receiver}</strong><span>{order.phone}</span><p>{formatAddress(order)}</p></div></section>
          </div>

          <aside className="order-detail-side">
            <div className="summary-card">
              <p className="eyebrow text-smoke">金额明细</p><h2 className="mt-3 font-nantes text-3xl">订单合计</h2>
              <dl className="summary-lines mt-8"><div><dt>商品总额</dt><dd>{formatMoney(order.total_amount)}</dd></div><div><dt>优惠券</dt><dd className={order.discount_amount ? "discount-text" : ""}>{order.discount_amount ? `- ${formatMoney(order.discount_amount)}` : "未使用"}</dd></div><div className="summary-total"><dt>应付金额</dt><dd>{formatMoney(order.pay_amount)}</dd></div></dl>
              <div className="order-times mt-8"><div><span>创建时间</span><strong>{formatDate(order.created_at)}</strong></div>{order.expire_at && isPending && <div><span>支付截止</span><strong>{formatDate(order.expire_at)}</strong></div>}</div>
            </div>

            {isPending && (
              <div className="payment-panel mt-8"><p className="eyebrow text-smoke">模拟支付</p><h2 className="mt-3 font-nantes text-3xl">现在完成支付</h2><p className="mt-3 text-sm leading-6 text-smoke">这是内部演示接口，不会连接真实支付渠道。成功后订单进入“已支付”。</p><Button className="mt-6 w-full justify-center" onClick={() => pay("success")} disabled={busy}>{payMutation.isPending ? <Spinner label="支付处理中" /> : <>模拟支付成功 <Icon name="check" size={17} /></>}</Button><Button variant="ghost" className="mt-2 w-full justify-center text-sm" onClick={() => pay("fail")} disabled={busy}>模拟支付失败</Button><Button variant="danger" className="mt-5 w-full justify-center" onClick={cancel} disabled={busy}>取消订单</Button></div>
            )}
            {order.status === "paid" && <div className="waiting-panel mt-8"><p className="eyebrow text-smoke">配送中</p><h2 className="mt-3 font-nantes text-3xl">等待发货</h2><p className="mt-3 text-sm leading-6 text-smoke">订单已支付，等待后台完成发货。</p></div>}
            {order.status === "shipped" && <div className="payment-panel mt-8"><p className="eyebrow text-smoke">确认收货</p><h2 className="mt-3 font-nantes text-3xl">东西到了吗？</h2><p className="mt-3 text-sm leading-6 text-smoke">确认后订单会进入已完成状态。</p><Button className="mt-6 w-full justify-center" onClick={() => confirmMutation.mutate()} disabled={busy}>{confirmMutation.isPending ? <Spinner label="正在确认" /> : <>确认收货 <Icon name="check" size={17} /></>}</Button></div>}
            {order.status === "completed" && <div className="waiting-panel mt-8"><p className="eyebrow text-smoke">订单已完成</p><h2 className="mt-3 font-nantes text-3xl">谢谢你的选择。</h2><p className="mt-3 text-sm leading-6 text-smoke">这笔订单的生命周期已经结束。</p></div>}
            {order.status === "cancelled" && <div className="waiting-panel mt-8"><p className="eyebrow text-smoke">订单已取消</p><h2 className="mt-3 font-nantes text-3xl">这次先到这里。</h2><p className="mt-3 text-sm leading-6 text-smoke">如果需要，可以重新回到商品目录。</p><Link to="/" className="button button-secondary mt-6 w-full justify-center">返回目录 <Icon name="arrow-right" size={16} /></Link></div>}
          </aside>
      </div>
    </section>
  );
}
