import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { claimCoupon, getClaimableCoupons, getMyCoupons } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { ClaimableCouponTemplateView, UserCouponListStatus, UserCouponView } from "../api/types";
import { describeCouponRule, formatDate, formatMoney } from "../lib/format";
import { Button, EmptyState, ErrorState, LoadingBlock, Spinner } from "../components/ui";

const couponStateLabels: Record<ClaimableCouponTemplateView["state"], string> = {
  claimable: "可领取",
  not_started: "未开始",
  ended: "已结束",
  sold_out: "已抢光",
  limit_reached: "已领满",
};

const mineTabs: Array<{ value: UserCouponListStatus; label: string }> = [
  { value: "", label: "全部" },
  { value: "unused", label: "未用" },
  { value: "used", label: "已用" },
  { value: "expired", label: "已过期" },
];

export function CouponsPage() {
  const queryClient = useQueryClient();
  const [tab, setTab] = useState<UserCouponListStatus>("");
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string } | null>(null);

  const claimableQuery = useQuery({
    queryKey: ["coupons", "claimable"],
    queryFn: getClaimableCoupons,
  });
  const mineQuery = useQuery({
    queryKey: ["coupons", "mine", tab],
    // tab 为空字符串即"全部"：后端 status="" 返回全部（与"未用"区分）。
    queryFn: () => getMyCoupons(tab),
  });

  const claimMutation = useMutation({
    mutationFn: claimCoupon,
    onSuccess: (coupon) => {
      setNotice({ kind: "success", text: `已领取一张优惠券（模板 #${coupon.template_id}），可在下方查看。` });
      queryClient.invalidateQueries({ queryKey: ["coupons"] });
    },
    onError: (error) => {
      setNotice({ kind: "error", text: getApiErrorMessage(error) });
    },
  });

  const claimables = claimableQuery.data || [];
  const mine = mineQuery.data?.items || [];

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">优惠券 / {claimables.length} 张可领</p>
          <h1 className="mt-3 font-nantes text-5xl">把折扣留到结账那一刻。</h1>
        </div>
        <div className="section-index" aria-hidden="true">06 <span>/</span> coupons</div>
      </div>

      {notice && (
        <div className={`notice ${notice.kind === "success" ? "notice-success" : "notice-error"} mt-8`}>
          <p>{notice.text}</p>
        </div>
      )}

      <h2 className="mt-12 text-subheading">可领券列表</h2>
      {claimableQuery.isPending ? (
        <LoadingBlock label="正在读取可领券" />
      ) : claimableQuery.isError ? (
        <div className="mt-6">
          <ErrorState message={getApiErrorMessage(claimableQuery.error)} onRetry={() => claimableQuery.refetch()} />
        </div>
      ) : claimables.length === 0 ? (
        <div className="mt-6">
          <EmptyState eyebrow="暂无券可领" title="新的券模板还没有发布。" description="回到商品目录逛逛，说不定结算时用得上。" />
        </div>
      ) : (
        <div className="coupon-center-grid mt-6">
          {claimables.map((template) => (
            <CouponCenterCard
              key={template.id}
              template={template}
              busy={claimMutation.isPending}
              onClaim={() => {
                setNotice(null);
                claimMutation.mutate(template.id);
              }}
            />
          ))}
        </div>
      )}

      <div className="mt-16 flex items-end justify-between gap-6">
        <h2 className="text-subheading">我的券</h2>
        <div className="order-tabs" role="tablist" aria-label="我的券状态筛选">
          {mineTabs.map((tabItem) => (
            <button
              key={tabItem.label}
              type="button"
              role="tab"
              aria-selected={tab === tabItem.value}
              className={tab === tabItem.value ? "active" : ""}
              onClick={() => setTab(tabItem.value)}
            >
              {tabItem.label}
            </button>
          ))}
        </div>
      </div>

      {mineQuery.isPending ? (
        <LoadingBlock label="正在读取我的券" />
      ) : mineQuery.isError ? (
        <div className="mt-6">
          <ErrorState message={getApiErrorMessage(mineQuery.error)} onRetry={() => mineQuery.refetch()} />
        </div>
      ) : mine.length === 0 ? (
        <div className="mt-6">
          <EmptyState eyebrow="还没有券" title="领取一张，结账时少付一点。" description="上面的列表里挑一张顺眼的领取，满减与直减都有。" />
        </div>
      ) : (
        <div className="coupon-mine-list mt-6">
          {mine.map((coupon) => <MyCouponRow key={coupon.id} coupon={coupon} />)}
        </div>
      )}
    </section>
  );
}

function CouponCenterCard({
  template,
  busy,
  onClaim,
}: {
  template: ClaimableCouponTemplateView;
  busy: boolean;
  onClaim: () => void;
}) {
  const claimable = template.state === "claimable";
  return (
    <article className="coupon-center-card">
      <div className="coupon-center-value">
        <strong>{formatMoney(template.value)}</strong>
        <span>{describeCouponRule(template.type, template.min_amount)}</span>
      </div>
      <div className="coupon-center-copy">
        <h3 className="line-clamp-1">{template.name}</h3>
        <p className="mt-1 text-xs text-smoke">
          {formatDate(template.valid_from)} 至 {formatDate(template.valid_until)}
        </p>
        <p className="mt-1 text-xs text-smoke">
          已领 {template.claimed_count} / {template.total} · 每人限领 {template.per_user_limit} 张
        </p>
      </div>
      <div className="coupon-center-action">
        <span className="tag">{couponStateLabels[template.state]}</span>
        <Button variant={claimable ? "primary" : "secondary"} disabled={!claimable || busy} onClick={onClaim}>
          {busy ? <Spinner label="领取中" /> : claimable ? "立即领取" : "暂不可领"}
        </Button>
      </div>
    </article>
  );
}

function MyCouponRow({ coupon }: { coupon: UserCouponView }) {
  const stateLabel =
    coupon.status === "used" ? "已用" : coupon.status === "expired" ? "已过期" : "未用";
  return (
    <div className={`coupon-mine-row status-${coupon.status}`}>
      <div className="coupon-mine-value">
        <strong>{formatMoney(coupon.value)}</strong>
      </div>
      <div className="min-w-0 flex-1">
        <h3 className="line-clamp-1">{coupon.name}</h3>
        <p className="mt-1 text-xs text-smoke">
          {describeCouponRule(coupon.type, coupon.min_amount)} · 有效期至 {formatDate(coupon.valid_until)}
        </p>
        {coupon.used_at && <p className="mt-1 text-xs text-smoke">使用于 {formatDate(coupon.used_at)}</p>}
      </div>
      <span className="tag">{stateLabel}</span>
    </div>
  );
}
