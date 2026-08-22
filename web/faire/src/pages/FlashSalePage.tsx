import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { getFlashSalePurchase, getFlashSales, purchaseFlashSale } from "../api/endpoints";
import { ApiRequestError, getApiErrorMessage } from "../api/client";
import type { FlashSaleActivity } from "../api/types";
import { formatMoney, formatSpecs } from "../lib/format";
import { recordPollAttempt, resetPollBudget, type PollBudget } from "../lib/flashsale-poll";
import {
  acceptFlashSaleOperation,
  beginFlashSaleOperation,
  clearFlashSaleOperation,
  readFlashSaleOperations,
  updateFlashSaleOperation,
  type PendingFlashSaleOperation,
} from "../lib/pending-operations";
import { Button, EmptyState, ErrorState, Icon, LoadingBlock, ProductVisual, Spinner } from "../components/ui";
import { useAuthStore } from "../store/auth";

const POLL_INTERVAL_MS = 1500;
const POLL_MAX_ATTEMPTS = 30;

type SeckillPhase = "idle" | "queued" | "success" | "rolled_back" | "timeout";
type SeckillOutcome =
  | { phase: "success"; orderNo?: string }
  | { phase: "rolled_back" };

function pad2(n: number) {
  return String(n).padStart(2, "0");
}

function formatCountdown(ms: number) {
  if (ms <= 0) {
    return "00:00:00";
  }
  const totalSeconds = Math.floor(ms / 1000);
  const days = Math.floor(totalSeconds / 86400);
  const hours = Math.floor((totalSeconds % 86400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  return days > 0
    ? `${days}天 ${pad2(hours)}:${pad2(minutes)}:${pad2(seconds)}`
    : `${pad2(hours)}:${pad2(minutes)}:${pad2(seconds)}`;
}

// useServerClock 以列表接口携带的 server_time 为基准修正本地时钟偏移，
// 每秒刷新，倒计时与服务端时间对齐。
function useServerClock(serverTime?: string) {
  const [offset, setOffset] = useState(0);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!serverTime) {
      return;
    }
    setOffset(new Date(serverTime).getTime() - Date.now());
  }, [serverTime]);

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  return now + offset;
}

// 轮询持久预扣状态，而不是猜测订单是否已经生成。即使首次 MQ 发布失败，
// 后台恢复也会把同一生命周期收敛为 ordered 或 rolled_back。
function useSeckillPoll(
  operation: PendingFlashSaleOperation | undefined,
  onOperationChange: (patch: Partial<Pick<PendingFlashSaleOperation, "orderNo" | "phase" | "attempts">>) => void,
  onTerminal: (outcome: SeckillOutcome) => void,
) {
  const initialPhase = operation?.phase === "timeout"
    ? "timeout"
    : operation?.preDeductionId
      ? "queued"
      : "idle";
  const [phase, setPhase] = useState<SeckillPhase>(initialPhase);
  const [attempt, setAttempt] = useState(operation?.attempts ?? 0);
  const attemptsRef = useRef<PollBudget>({
    lifecycleID: operation?.preDeductionId,
    attempts: operation?.attempts ?? 0,
  });

  useEffect(() => {
    attemptsRef.current = resetPollBudget(attemptsRef.current, operation?.preDeductionId);
    attemptsRef.current.attempts = operation?.attempts ?? attemptsRef.current.attempts;
    setAttempt(attemptsRef.current.attempts);
    setPhase(operation?.phase === "timeout" ? "timeout" : operation?.preDeductionId ? "queued" : "idle");
  }, [operation?.attempts, operation?.phase, operation?.preDeductionId]);

  const query = useQuery({
    queryKey: ["seckill-poll", operation?.preDeductionId],
    // 尝试次数在 queryFn（异步执行）中累计：refetchInterval 回调会在渲染期
    // 被调用，若在其中 setState 会引发渲染循环、interval 反复重建（轮询中断）。
    queryFn: () => {
      attemptsRef.current = recordPollAttempt(attemptsRef.current, operation!.preDeductionId!);
      setAttempt(attemptsRef.current.attempts);
      onOperationChange({ attempts: attemptsRef.current.attempts });
      return getFlashSalePurchase(operation!.preDeductionId!);
    },
    enabled: operation?.phase === "queued" && Boolean(operation.preDeductionId),
    retry: false,
    refetchInterval: (pollQuery) => {
      // 已落单或达到次数上限：停止轮询。
      if (
        pollQuery.state.data?.status === "ordered" ||
        pollQuery.state.data?.status === "rolled_back" ||
        attemptsRef.current.attempts >= POLL_MAX_ATTEMPTS
      ) {
        return false;
      }
      return POLL_INTERVAL_MS;
    },
  });

  useEffect(() => {
    if (operation?.phase !== "queued") return;
    if (query.data?.status === "ordered") {
      if (query.data.order_no && query.data.order_no !== operation?.orderNo) {
        onOperationChange({ orderNo: query.data.order_no });
      }
      onTerminal({ phase: "success", orderNo: query.data.order_no || operation.orderNo });
      setPhase("success");
    } else if (query.data?.status === "rolled_back") {
      onTerminal({ phase: "rolled_back" });
      setPhase("rolled_back");
    } else if (query.data?.order_no && query.data.order_no !== operation?.orderNo) {
      onOperationChange({ orderNo: query.data.order_no });
    } else if (
      operation?.phase === "queued" &&
      attemptsRef.current.attempts >= POLL_MAX_ATTEMPTS &&
      query.fetchStatus === "idle"
    ) {
      onOperationChange({ phase: "timeout", attempts: attemptsRef.current.attempts });
      setPhase("timeout");
    }
  }, [operation?.orderNo, operation?.phase, query.data, query.fetchStatus]);

  function retry() {
    attemptsRef.current = { lifecycleID: operation?.preDeductionId, attempts: 0 };
    setAttempt(0);
    setPhase("queued");
    onOperationChange({ phase: "queued", attempts: 0 });
    void query.refetch();
  }

  return {
    phase,
    attempt: Math.max(attempt, 1),
    orderNo: query.data?.order_no || operation?.orderNo,
    retry,
  };
}

export function FlashSalePage() {
  const queryClient = useQueryClient();
  const userId = useAuthStore((state) => state.user?.id ?? 0);
  const [operations, setOperations] = useState<Record<number, PendingFlashSaleOperation>>(() => (
    userId ? readFlashSaleOperations(userId) : {}
  ));
  const [outcomes, setOutcomes] = useState<Record<number, SeckillOutcome>>({});
  const [errors, setErrors] = useState<Record<number, string>>({});
  const [pendingId, setPendingId] = useState<number | null>(null);

  const listQuery = useQuery({
    queryKey: ["flash-sales"],
    queryFn: getFlashSales,
    refetchInterval: 15_000,
  });
  const serverClock = useServerClock(listQuery.data?.server_time);

  const purchaseMutation = useMutation({
    mutationFn: ({ activityId, operation }: { activityId: number; operation: PendingFlashSaleOperation }) => (
      purchaseFlashSale(activityId, operation.clientRequestId)
    ),
    onSuccess: (result, { activityId, operation }) => {
      if (result.status === "rolled_back") {
        clearFlashSaleOperation(userId, activityId);
        setOperations((current) => {
          const next = { ...current };
          delete next[activityId];
          return next;
        });
        setOutcomes((current) => ({ ...current, [activityId]: { phase: "rolled_back" } }));
      } else {
        const accepted = acceptFlashSaleOperation(userId, activityId, {
          preDeductionId: result.pre_deduction_id,
          ...(result.order_no ? { orderNo: result.order_no } : {}),
        }, operation);
        setOperations((current) => ({ ...current, [activityId]: accepted }));
      }
      setErrors((prev) => {
        const next = { ...prev };
        delete next[activityId];
        return next;
      });
      setPendingId(null);
      queryClient.invalidateQueries({ queryKey: ["flash-sales"] });
    },
    onError: (error, { activityId }) => {
      if (error instanceof ApiRequestError && error.status && error.status < 500) {
        clearFlashSaleOperation(userId, activityId);
        setOperations((current) => {
          const next = { ...current };
          delete next[activityId];
          return next;
        });
      }
      setErrors((prev) => ({ ...prev, [activityId]: getApiErrorMessage(error) }));
      setPendingId(null);
    },
  });

  const activities = listQuery.data?.items || [];

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">秒杀 / {activities.length} 场在售</p>
          <h1 className="mt-3 font-nantes text-5xl">限时的价格，留到准点。</h1>
        </div>
        <div className="section-index" aria-hidden="true">05 <span>/</span> flash sale</div>
      </div>

      {listQuery.isPending ? (
        <LoadingBlock label="正在读取秒杀活动" />
      ) : listQuery.isError ? (
        <div className="mt-8">
          <ErrorState message={getApiErrorMessage(listQuery.error)} onRetry={() => listQuery.refetch()} />
        </div>
      ) : activities.length === 0 ? (
        <div className="mt-8">
          <EmptyState
            eyebrow="暂无在售活动"
            title="最近的场次还没有开始。"
            description="回到商品目录，或者过一会儿再来看看即将开始的场次。"
            action={
              <Link to="/">
                <Button>返回目录 <Icon name="arrow-right" size={16} /></Button>
              </Link>
            }
          />
        </div>
      ) : (
        <div className="flash-grid mt-10">
          {activities.map((activity) => (
            <FlashSaleCard
              key={activity.id}
              activity={activity}
              serverClock={serverClock}
              operation={operations[activity.id]}
              outcome={outcomes[activity.id]}
              error={errors[activity.id]}
              busy={pendingId === activity.id}
              onBuy={() => {
                setErrors((prev) => {
                  const next = { ...prev };
                  delete next[activity.id];
                  return next;
                });
                setOutcomes((current) => {
                  const next = { ...current };
                  delete next[activity.id];
                  return next;
                });
                setPendingId(activity.id);
                const operation = operations[activity.id]
                  ?? beginFlashSaleOperation(userId, activity.id);
                setOperations((current) => ({ ...current, [activity.id]: operation }));
                purchaseMutation.mutate({
                  activityId: activity.id,
                  operation,
                });
              }}
              onOperationChange={(patch) => {
                setOperations((current) => {
                  const operation = current[activity.id];
                  if (!operation) return current;
                  const next = updateFlashSaleOperation(userId, activity.id, patch, operation);
                  return { ...current, [activity.id]: next };
                });
              }}
              onTerminal={(outcome) => {
                clearFlashSaleOperation(userId, activity.id);
                setOperations((current) => {
                  const next = { ...current };
                  delete next[activity.id];
                  return next;
                });
                setOutcomes((current) => ({ ...current, [activity.id]: outcome }));
              }}
            />
          ))}
        </div>
      )}
    </section>
  );
}

function FlashSaleCard({
  activity,
  serverClock,
  operation,
  outcome,
  error,
  busy,
  onBuy,
  onOperationChange,
  onTerminal,
}: {
  activity: FlashSaleActivity;
  serverClock: number;
  operation?: PendingFlashSaleOperation;
  outcome?: SeckillOutcome;
  error?: string;
  busy: boolean;
  onBuy: () => void;
  onOperationChange: (patch: Partial<Pick<PendingFlashSaleOperation, "orderNo" | "phase" | "attempts">>) => void;
  onTerminal: (outcome: SeckillOutcome) => void;
}) {
  const poll = useSeckillPoll(operation, onOperationChange, onTerminal);
  const phase = outcome?.phase ?? (operation ? poll.phase : "idle");
  const orderNo = outcome?.phase === "success" ? outcome.orderNo : poll.orderNo;

  const inProgress = activity.state === "in_progress";
  const soldOut = activity.stock <= 0;
  const startAt = new Date(activity.start_at).getTime();
  const endAt = new Date(activity.end_at).getTime();
  const countdown = inProgress ? endAt - serverClock : startAt - serverClock;
  const countdownLabel = inProgress ? "距结束" : "距开始";
  // 列表 15s 刷新窗口内活动可能已到点：本地按时钟兜底"已结束"，
  // 避免倒计时归零后按钮仍可点（后端拒绝则展示错误）。
  const locallyEnded = inProgress && countdown <= 0;
  const canBuy = inProgress && !locallyEnded && !soldOut && phase === "idle" && !busy;

  return (
    <article className={`flash-card ${phase === "queued" ? "is-queued" : ""}`}>
      <Link
        to={`/products/${activity.sku.product_id}`}
        className="flash-card-visual"
        aria-label={`查看商品 ${activity.product_title}`}
      >
        <ProductVisual seed={activity.sku.product_id} title={activity.product_title} compact />
      </Link>
      <div className="flash-card-copy">
        <div className="flash-card-top">
          <div className="min-w-0">
            <h2 className="line-clamp-2 text-lg">{activity.product_title}</h2>
            <p className="mt-1 text-sm text-smoke">
              {activity.title}
              {activity.sku.specs ? ` · ${formatSpecs(activity.sku.specs)}` : ""}
            </p>
          </div>
          {locallyEnded ? <span className="tag">已结束</span> : inProgress ? <span className="tag tag-live">进行中</span> : <span className="tag">即将开始</span>}
        </div>

        <div className="flash-card-price">
          <strong>{formatMoney(activity.price)}</strong>
          {activity.sku.price > activity.price && <del>{formatMoney(activity.sku.price)}</del>}
        </div>

        <p className="flash-card-meta">
          剩余 <strong>{activity.stock}</strong> 件 · 每人限购 <strong>{activity.per_user_limit}</strong> 件
        </p>

        <div className="flash-card-countdown" aria-live="off">
          <span>{locallyEnded ? "已结束" : countdownLabel}</span>
          <strong className="font-mono">{locallyEnded ? "--:--:--" : formatCountdown(countdown)}</strong>
        </div>

        {phase === "queued" ? (
          <div className="processing-panel processing-panel-sm" aria-live="polite">
            <span className="processing-mark"><Icon name="clock" size={22} /></span>
            <div>
              <p><strong>排队中</strong>，正在等待落单结果。</p>
              <p className="mt-1 text-sm text-smoke">第 {poll.attempt} / {POLL_MAX_ATTEMPTS} 次查询（1.5s 间隔）</p>
            </div>
            <Spinner label="查询中" />
          </div>
        ) : phase === "success" ? (
          <div className="notice notice-success">
            <p><strong>抢购成功</strong>，订单已生成。</p>
            {orderNo && (
              <Link to={`/orders/${orderNo}`} className="mt-2 inline-block text-sm underline underline-offset-3">
                去查看订单 <span aria-hidden="true">↗</span>
              </Link>
            )}
          </div>
        ) : phase === "timeout" ? (
          <div className="notice notice-error">
            <p><strong>仍在处理中</strong>，{POLL_MAX_ATTEMPTS} 次查询后未等到落单结果。</p>
            <div className="mt-3 flex items-center gap-4">
              <Button variant="secondary" className="button-small" onClick={poll.retry}><Icon name="refresh" size={15} /> 再查一次</Button>
              <Link to="/orders" className="text-sm underline underline-offset-3">
                到订单页确认 <span aria-hidden="true">↗</span>
              </Link>
            </div>
          </div>
        ) : phase === "rolled_back" ? (
          <div className="notice notice-error">
            <p><strong>抢购未完成</strong>，预扣库存已完整退回，可以重新尝试。</p>
          </div>
        ) : error ? (
          <div className="notice notice-error">
            <p>{error}</p>
          </div>
        ) : operation?.phase === "submitting" ? (
          <div className="notice">
            <p><strong>上次提交结果未知</strong>，再次尝试会复用同一个请求标识。</p>
          </div>
        ) : null}

        <div className="flash-card-action">
          {phase === "queued" ? (
            <Button variant="secondary" disabled><Spinner label="排队中" /></Button>
          ) : phase === "success" && activity.per_user_limit > 1 ? (
            <Button onClick={onBuy} disabled={!inProgress || locallyEnded || soldOut || busy}>{busy ? "提交中…" : "继续抢购"}</Button>
          ) : phase === "success" && orderNo ? (
            <Link to={`/orders/${orderNo}`}><Button>查看订单 <Icon name="arrow-right" size={15} /></Button></Link>
          ) : phase === "timeout" ? (
            <Button variant="secondary" disabled>等待落单</Button>
          ) : phase === "rolled_back" ? (
            <Button onClick={onBuy} disabled={!inProgress || soldOut || busy}>{busy ? "提交中…" : "重新抢购"}</Button>
          ) : operation?.phase === "submitting" ? (
            <Button onClick={onBuy} disabled={!canBuy}>{busy ? "提交中…" : "继续抢购"}</Button>
          ) : soldOut ? (
            <Button variant="secondary" disabled>已抢光</Button>
          ) : locallyEnded || !inProgress ? (
            <Button variant="secondary" disabled>{locallyEnded ? "已结束" : `${formatCountdown(countdown)} 后开抢`}</Button>
          ) : (
            <Button onClick={onBuy} disabled={!canBuy}>
              {busy ? "提交中…" : "立即抢购"}
            </Button>
          )}
        </div>
      </div>
    </article>
  );
}
