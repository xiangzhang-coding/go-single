import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { adminApi } from "../../api/endpoints";
import { getApiErrorMessage } from "../../api/client";
import type { CouponTemplateView } from "../../api/types";
import { describeCouponRule, formatDate, formatMoney, toLocalInput } from "../../lib/format";
import { Button, EmptyState, ErrorState, LoadingBlock, Spinner } from "../../components/ui";

export function AdminCouponsPage() {
  const queryClient = useQueryClient();
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const [editing, setEditing] = useState<CouponTemplateView | null>(null);
  const [showCreate, setShowCreate] = useState(false);

  const templatesQuery = useQuery({
    queryKey: ["admin", "coupons"],
    queryFn: adminApi.getCouponTemplates,
  });

  const templates = templatesQuery.data || [];

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["admin", "coupons"] });

  return (
    <div className="mt-8">
      {notice && (
        <div className={`notice ${notice.kind === "success" ? "notice-success" : "notice-error"}`}>
          <p>{notice.text}</p>
        </div>
      )}

      <div className="flex justify-end">
        <Button variant="secondary" onClick={() => { setShowCreate(true); setEditing(null); }}>
          + 发布券模板
        </Button>
      </div>

      {(showCreate || editing) && (
        <TemplateForm
          initial={editing}
          onDone={() => {
            setShowCreate(false);
            setEditing(null);
            setNotice({ kind: "success", text: editing ? "券模板已更新。" : "券模板已发布，用户可在优惠券中心领取。" });
            invalidate();
          }}
          onError={(text) => setNotice({ kind: "error", text })}
        />
      )}

      {templatesQuery.isPending ? (
        <div className="mt-6">
          <LoadingBlock label="正在读取券模板" />
        </div>
      ) : templatesQuery.isError ? (
        <div className="mt-6">
          <ErrorState message={getApiErrorMessage(templatesQuery.error)} onRetry={() => templatesQuery.refetch()} />
        </div>
      ) : templates.length === 0 ? (
        <div className="mt-6">
          <EmptyState eyebrow="暂无模板" title="还没有发布过券模板。" description="发布一张直减或满减券，用户即可在优惠券中心领取。" />
        </div>
      ) : (
        <div className="admin-list mt-6">
          {templates.map((template) => (
            <div key={template.id} className="admin-card">
              <div className="admin-card-top">
                <div className="min-w-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <h3 className="font-nantes text-xl">{template.name}</h3>
                    <span className="tag">{template.type === "threshold" ? "满减券" : "直减券"}</span>
                  </div>
                  <p className="mt-1 text-xs text-smoke">
                    {formatDate(template.valid_from)} 至 {formatDate(template.valid_until)} · 每人限领 {template.per_user_limit} 张
                  </p>
                </div>
                <div className="flex flex-none items-center gap-4">
                  <div className="text-right">
                    <strong className="text-base font-normal">{formatMoney(template.value)}</strong>
                    <p className="text-xs text-smoke">{describeCouponRule(template.type, template.min_amount)}</p>
                    <p className="text-xs text-smoke">
                      已领 {template.claimed_count} / {template.total}
                    </p>
                  </div>
                  <Button variant="ghost" size="small" onClick={() => { setEditing(template); setShowCreate(false); }}>
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

function TemplateForm({
  initial,
  onDone,
  onError,
}: {
  initial?: CouponTemplateView | null;
  onDone: () => void;
  onError: (text: string) => void;
}) {
  const [form, setForm] = useState({
    name: initial?.name || "",
    type: initial?.type === "threshold" ? "threshold" : "direct",
    value: initial ? String(initial.value) : "",
    min_amount: initial ? String(initial.min_amount) : "0",
    total: initial ? String(initial.total) : "",
    per_user_limit: initial ? String(initial.per_user_limit) : "1",
    valid_from: initial ? toLocalInput(initial.valid_from) : "",
    valid_until: initial ? toLocalInput(initial.valid_until) : "",
  });

  const mutation = useMutation({
    mutationFn: () => {
      const request = {
        name: form.name.trim(),
        type: form.type as "direct" | "threshold",
        value: Number(form.value),
        min_amount: Number(form.min_amount) || 0,
        total: Number(form.total),
        per_user_limit: Number(form.per_user_limit) || 1,
        valid_from: new Date(form.valid_from).toISOString(),
        valid_until: new Date(form.valid_until).toISOString(),
      };
      return initial
        ? adminApi.updateCouponTemplate(initial.id, request).then(() => undefined)
        : adminApi.createCouponTemplate(request).then(() => undefined);
    },
    onSuccess: onDone,
    onError: (error) => onError(getApiErrorMessage(error)),
  });

  return (
    <form
      className="admin-form-panel mt-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (!form.name || !form.value || !form.total) {
          onError("请填写模板名称、面额与总量。");
          return;
        }
        if (!form.valid_from || !form.valid_until || new Date(form.valid_from) >= new Date(form.valid_until)) {
          onError("有效期无效：开始需早于结束。");
          return;
        }
        mutation.mutate();
      }}
    >
      <div className="form-grid-2">
        <label className="form-label">
          模板名称
          <input
            className="form-control"
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
            placeholder="如：新人满 100 减 20"
          />
        </label>
        <label className="form-label">
          类型
          <select
            className="form-control"
            value={form.type}
            onChange={(e) => {
              const type = e.target.value as "direct" | "threshold";
              setForm({ ...form, type, min_amount: type === "direct" ? "0" : form.min_amount });
            }}
          >
            <option value="direct">直减（无门槛）</option>
            <option value="threshold">满减</option>
          </select>
        </label>
        <label className="form-label">
          面额（分）
          <input
            className="form-control"
            type="number"
            min={0}
            value={form.value}
            onChange={(e) => setForm({ ...form, value: e.target.value })}
            placeholder="2000"
          />
        </label>
        <label className="form-label">
          使用门槛（分，满减时填写）
          <input
            className="form-control"
            type="number"
            min={0}
            value={form.min_amount}
            disabled={form.type === "direct"}
            onChange={(e) => setForm({ ...form, min_amount: e.target.value })}
            placeholder="10000"
          />
        </label>
        <label className="form-label">
          总量
          <input
            className="form-control"
            type="number"
            min={1}
            value={form.total}
            onChange={(e) => setForm({ ...form, total: e.target.value })}
            placeholder="100"
          />
        </label>
        <label className="form-label">
          每人限领
          <input
            className="form-control"
            type="number"
            min={1}
            value={form.per_user_limit}
            onChange={(e) => setForm({ ...form, per_user_limit: e.target.value })}
          />
        </label>
        <label className="form-label">
          生效时间
          <input
            className="form-control"
            type="datetime-local"
            value={form.valid_from}
            onChange={(e) => setForm({ ...form, valid_from: e.target.value })}
          />
        </label>
        <label className="form-label">
          失效时间
          <input
            className="form-control"
            type="datetime-local"
            value={form.valid_until}
            onChange={(e) => setForm({ ...form, valid_until: e.target.value })}
          />
        </label>
      </div>
      <div className="mt-4 flex gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? <Spinner label="保存中" /> : initial ? "保存修改" : "发布模板"}
        </Button>
        {!initial && (
          <Button variant="ghost" type="button" onClick={onDone}>取消</Button>
        )}
      </div>
    </form>
  );
}
