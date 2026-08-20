import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  createAddress,
  createOrder,
  getAddresses,
  getCart,
  getMyCoupons,
  getProductDetail,
} from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { Address, CreateAddressRequest, UserCouponView } from "../api/types";
import { buildOrderRequest, parseCheckoutIntent } from "../lib/checkout";
import { isOrderProcessingResponse } from "../lib/order";
import { formatAddress, formatMoney, formatSpecs, isCouponUsable, makeClientRequestID } from "../lib/format";
import { Button, EmptyState, ErrorState, Icon, LoadingBlock, Spinner } from "../components/ui";

const emptyDraft: CreateAddressRequest = {
  receiver: "",
  phone: "",
  province: "",
  city: "",
  district: "",
  detail: "",
  is_default: false,
};

interface CheckoutItem {
  key: string;
  title: string;
  specs: unknown;
  price: number;
  quantity: number;
}

export function CheckoutPage() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const queryClient = useQueryClient();
  const checkoutIntent = parseCheckoutIntent(searchParams);
  const directProductId = checkoutIntent?.kind === "direct" ? checkoutIntent.productId : 0;
  const [clientRequestId] = useState(() => makeClientRequestID());
  const [selectedAddressId, setSelectedAddressId] = useState<number | null>(null);
  const [selectedCouponId, setSelectedCouponId] = useState<number | null>(null);
  const [showAddressForm, setShowAddressForm] = useState(false);
  const [draft, setDraft] = useState<CreateAddressRequest>(emptyDraft);
  const [actionError, setActionError] = useState("");

  const cartQuery = useQuery({
    queryKey: ["cart"],
    queryFn: getCart,
    enabled: checkoutIntent?.kind === "cart",
  });
  const directProductQuery = useQuery({
    queryKey: ["product", directProductId],
    queryFn: () => getProductDetail(directProductId),
    enabled: checkoutIntent?.kind === "direct",
  });
  const addressesQuery = useQuery({ queryKey: ["addresses"], queryFn: getAddresses });
  const couponsQuery = useQuery({ queryKey: ["coupons", "unused"], queryFn: () => getMyCoupons("unused") });

  useEffect(() => {
    if (selectedAddressId !== null || !addressesQuery.data?.length) {
      return;
    }
    const defaultAddress = addressesQuery.data.find((address) => address.is_default) || addressesQuery.data[0];
    setSelectedAddressId(defaultAddress.id);
  }, [addressesQuery.data, selectedAddressId]);

  const directSKU = checkoutIntent?.kind === "direct"
    ? directProductQuery.data?.skus.find((sku) => sku.id === checkoutIntent.skuId)
    : undefined;
  const items: CheckoutItem[] = checkoutIntent?.kind === "direct" && directProductQuery.data && directSKU
    ? [{
        key: `direct-${directSKU.id}`,
        title: directProductQuery.data.title,
        specs: directSKU.specs,
        price: directSKU.price,
        quantity: checkoutIntent.quantity,
      }]
    : (cartQuery.data?.items || []).map((item) => ({ ...item, key: `cart-${item.id}` }));
  const subtotal = items.reduce((sum, item) => sum + item.price * item.quantity, 0);
  const coupons = couponsQuery.data?.items || [];
  const eligibleCoupons = coupons.filter((coupon) => isCouponUsable(coupon, subtotal));
  const selectedCoupon = eligibleCoupons.find((coupon) => coupon.id === selectedCouponId);
  const discount = selectedCoupon ? Math.min(selectedCoupon.value, subtotal) : 0;
  const payAmount = subtotal - discount;

  const addressMutation = useMutation({
    mutationFn: () => createAddress(draft),
    onSuccess: (address) => {
      queryClient.invalidateQueries({ queryKey: ["addresses"] });
      setSelectedAddressId(address.id);
      setShowAddressForm(false);
      setDraft(emptyDraft);
      setActionError("");
    },
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });
  const orderMutation = useMutation({
    mutationFn: () => createOrder(buildOrderRequest(
      checkoutIntent!,
      clientRequestId,
      selectedAddressId!,
      selectedCouponId || 0,
    )),
    onSuccess: (order) => {
      if (checkoutIntent?.kind === "cart") {
        queryClient.invalidateQueries({ queryKey: ["cart"] });
      } else if (checkoutIntent?.kind === "direct") {
        queryClient.invalidateQueries({ queryKey: ["product", checkoutIntent.productId] });
      }
      queryClient.invalidateQueries({ queryKey: ["orders"] });
      queryClient.invalidateQueries({ queryKey: ["coupons"] });
      navigate(`/orders/${order.order_no}`, {
        state: isOrderProcessingResponse(order) ? { awaitingCreation: true } : undefined,
      });
    },
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });

  if (!checkoutIntent) {
    return <div className="site-container page-section"><ErrorState message="直购参数无效，请返回商品详情重新选择规格和数量。" /></div>;
  }
  const sourcePending = checkoutIntent.kind === "cart" ? cartQuery.isPending : directProductQuery.isPending;
  if (sourcePending || addressesQuery.isPending || couponsQuery.isPending) {
    return <div className="site-container page-section"><LoadingBlock label="正在准备结算页" /></div>;
  }
  if (checkoutIntent.kind === "cart" && cartQuery.isError) {
    return <div className="site-container page-section"><ErrorState message={getApiErrorMessage(cartQuery.error)} onRetry={() => cartQuery.refetch()} /></div>;
  }
  if (checkoutIntent.kind === "direct" && directProductQuery.isError) {
    return <div className="site-container page-section"><ErrorState message={getApiErrorMessage(directProductQuery.error)} onRetry={() => directProductQuery.refetch()} /></div>;
  }
  if (addressesQuery.isError || couponsQuery.isError) {
    const error = addressesQuery.error || couponsQuery.error;
    return <div className="site-container page-section"><ErrorState message={getApiErrorMessage(error)} onRetry={() => { addressesQuery.refetch(); couponsQuery.refetch(); }} /></div>;
  }
  if (checkoutIntent.kind === "direct" && (!directSKU || directSKU.stock < checkoutIntent.quantity)) {
    return <div className="site-container page-section"><ErrorState message="所选 SKU 已不可购买或库存不足，请返回商品详情重新选择。" /></div>;
  }
  if (items.length === 0) {
    return (
      <section className="site-container page-section">
        <EmptyState eyebrow="没有可结算的商品" title="购物车还没有内容。" action={<Link to="/"><Button>返回目录 <Icon name="arrow-right" size={16} /></Button></Link>} />
      </section>
    );
  }

  function updateDraft(field: keyof CreateAddressRequest, value: string | boolean) {
    setDraft((current) => ({ ...current, [field]: value }));
  }

  function submitAddress(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setActionError("");
    addressMutation.mutate();
  }

  function submitOrder() {
    if (!selectedAddressId) {
      setActionError("请选择或新增一条地址簿记录");
      return;
    }
    setActionError("");
    orderMutation.mutate();
  }

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <Link to={checkoutIntent.kind === "direct" ? `/products/${checkoutIntent.productId}` : "/cart"} className="back-link"><Icon name="arrow-left" size={16} /> {checkoutIntent.kind === "direct" ? "返回商品详情" : "返回购物车"}</Link>
      <div className="section-heading-row mt-8 sm:mt-12">
        <div>
          <p className="eyebrow text-smoke">结算 / 最后确认</p>
          <h1 className="mt-3 font-nantes text-5xl">把选择寄出去。</h1>
        </div>
        <div className="section-index" aria-hidden="true">03 <span>/</span> checkout</div>
      </div>

      {actionError && <div className="notice notice-error mt-8">{actionError}</div>}

      <div className="checkout-layout mt-10">
        <div className="checkout-main space-y-8">
          <section className="checkout-section">
            <div className="checkout-section-heading">
              <div><p className="eyebrow text-smoke">01 / 地址簿</p><h2 className="mt-2 font-nantes text-3xl">送到哪里？</h2></div>
              {addressesQuery.data?.length ? <Button variant="secondary" onClick={() => setShowAddressForm((visible) => !visible)}><Icon name="plus" size={16} />{showAddressForm ? "收起" : "新增地址"}</Button> : null}
            </div>

            {addressesQuery.data?.length ? (
              <div className="address-list mt-6">
                {addressesQuery.data.map((address) => <AddressOption key={address.id} address={address} selected={selectedAddressId === address.id} onSelect={() => setSelectedAddressId(address.id)} />)}
              </div>
            ) : (
              <p className="mt-4 text-sm text-smoke">地址簿还没有记录，先添加一条地址才能下单。</p>
            )}

            {showAddressForm || !addressesQuery.data?.length ? (
              <form className="address-form mt-6" onSubmit={submitAddress}>
                <div className="form-grid-2">
                  <label className="form-label"><span>收货人</span><input className="form-control" required value={draft.receiver} onChange={(event) => updateDraft("receiver", event.target.value)} placeholder="姓名" /></label>
                  <label className="form-label"><span>手机号</span><input className="form-control" required value={draft.phone} onChange={(event) => updateDraft("phone", event.target.value)} placeholder="11 位手机号" inputMode="tel" /></label>
                  <label className="form-label"><span>省</span><input className="form-control" required value={draft.province} onChange={(event) => updateDraft("province", event.target.value)} placeholder="例如 江苏省" /></label>
                  <label className="form-label"><span>市</span><input className="form-control" required value={draft.city} onChange={(event) => updateDraft("city", event.target.value)} placeholder="例如 南通市" /></label>
                  <label className="form-label"><span>区 / 县</span><input className="form-control" required value={draft.district} onChange={(event) => updateDraft("district", event.target.value)} placeholder="例如 崇川区" /></label>
                  <label className="form-label"><span>详细地址</span><input className="form-control" required value={draft.detail} onChange={(event) => updateDraft("detail", event.target.value)} placeholder="街道、门牌号" /></label>
                </div>
                {Boolean(addressesQuery.data?.length) && <label className="checkbox-label mt-5"><input type="checkbox" checked={draft.is_default} onChange={(event) => updateDraft("is_default", event.target.checked)} />设为默认地址</label>}
                <Button type="submit" variant="secondary" className="mt-5" disabled={addressMutation.isPending}>{addressMutation.isPending ? <Spinner label="正在保存" /> : "保存地址"}</Button>
              </form>
            ) : null}
          </section>

          <section className="checkout-section">
            <div className="checkout-section-heading">
              <div><p className="eyebrow text-smoke">02 / 优惠券</p><h2 className="mt-2 font-nantes text-3xl">这次要用哪张？</h2></div>
              <span className="text-sm text-smoke">{eligibleCoupons.length} 张可用</span>
            </div>
            {eligibleCoupons.length ? (
              <div className="coupon-list mt-6">
                <button type="button" className={`coupon-option ${selectedCouponId === null ? "selected" : ""}`} onClick={() => setSelectedCouponId(null)}><span><strong>不使用优惠券</strong><small>保留这次选择</small></span><span className="coupon-value">-</span></button>
                {eligibleCoupons.map((coupon) => <CouponOption key={coupon.id} coupon={coupon} selected={selectedCouponId === coupon.id} onSelect={() => setSelectedCouponId(coupon.id)} />)}
              </div>
            ) : (
              <p className="mt-4 text-sm text-smoke">当前没有满足本单金额和有效期的优惠券。</p>
            )}
          </section>

          <section className="checkout-section">
            <div className="checkout-section-heading"><div><p className="eyebrow text-smoke">03 / 商品</p><h2 className="mt-2 font-nantes text-3xl">确认这几件。</h2></div></div>
            <div className="checkout-items mt-6">
              {items.map((item) => <div className="checkout-item" key={item.key}><div><strong>{item.title}</strong><p>{formatSpecs(item.specs)} × {item.quantity}</p></div><strong>{formatMoney(item.price * item.quantity)}</strong></div>)}
            </div>
          </section>
        </div>

        <aside className="summary-card checkout-summary">
          <p className="eyebrow text-smoke">金额确认</p>
          <h2 className="mt-3 font-nantes text-3xl">订单合计</h2>
          <dl className="summary-lines mt-8">
            <div><dt>商品总额</dt><dd>{formatMoney(subtotal)}</dd></div>
            <div><dt>优惠券</dt><dd className={discount ? "discount-text" : ""}>{discount ? `- ${formatMoney(discount)}` : "未选择"}</dd></div>
            <div className="summary-total"><dt>应付金额</dt><dd>{formatMoney(payAmount)}</dd></div>
          </dl>
          <div className="summary-address mt-8"><div className="flex items-center gap-2 text-sm text-smoke"><Icon name="pin" size={16} /> 地址簿记录</div>{selectedAddressId && <p className="mt-3 text-sm leading-6">{formatSelectedAddress(addressesQuery.data || [], selectedAddressId)}</p>}</div>
          <Button className="mt-8 w-full justify-center" onClick={submitOrder} disabled={orderMutation.isPending || !selectedAddressId}>{orderMutation.isPending ? <Spinner label="正在创建订单" /> : <>提交订单 <Icon name="arrow-right" size={17} /></>}</Button>
          <p className="mt-4 text-xs leading-5 text-smoke">提交后会生成待支付订单，商品库存和优惠券会在服务端事务内确认。</p>
        </aside>
      </div>
    </section>
  );
}

function AddressOption({ address, selected, onSelect }: { address: Address; selected: boolean; onSelect: () => void }) {
  return (
    <button type="button" className={`address-option ${selected ? "selected" : ""}`} onClick={onSelect}>
      <span className="address-radio">{selected && <Icon name="check" size={14} />}</span>
      <span className="min-w-0 flex-1 text-left"><strong>{address.receiver}</strong><span className="ml-3 text-sm text-smoke">{address.phone}</span><small>{formatAddress(address)}</small></span>
      {address.is_default && <span className="tag">默认</span>}
    </button>
  );
}

function CouponOption({ coupon, selected, onSelect }: { coupon: UserCouponView; selected: boolean; onSelect: () => void }) {
  return (
    <button type="button" className={`coupon-option ${selected ? "selected" : ""}`} onClick={onSelect}>
      <span><strong>{coupon.name}</strong><small>{coupon.type === "threshold" ? `满 ${formatMoney(coupon.min_amount)} 可用` : "无门槛直减"}</small></span>
      <span className="coupon-value">{formatMoney(coupon.value)}</span>
    </button>
  );
}

function formatSelectedAddress(addresses: Address[], id: number) {
  const address = addresses.find((item) => item.id === id);
  return address ? `${address.receiver} · ${address.phone}\n${formatAddress(address)}` : "请选择地址";
}
