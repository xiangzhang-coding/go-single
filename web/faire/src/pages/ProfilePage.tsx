import { useEffect, useRef, useState, type ChangeEvent, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  authApi,
  createAddress,
  deleteAddress,
  getAddresses,
  setDefaultAddress,
  updateAddress,
  uploadFile,
} from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import type { Address, CreateAddressRequest, UpdateProfileRequest } from "../api/types";
import { formatAddress } from "../lib/format";
import { Button, EmptyState, ErrorState, Icon, LoadingBlock, Spinner } from "../components/ui";
import { useAuthStore } from "../store/auth";

const emptyDraft: CreateAddressRequest = {
  receiver: "",
  phone: "",
  province: "",
  city: "",
  district: "",
  detail: "",
  is_default: false,
};

// 头像上传上限（与后端 platform/file 一致：类型白名单 png/jpeg/webp/gif）。
const maxAvatarBytes = 5 * 1024 * 1024;

export function ProfilePage() {
  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">个人中心 / 资料与地址</p>
          <h1 className="mt-3 font-nantes text-5xl">我的档案。</h1>
        </div>
        <div className="section-index" aria-hidden="true">09 <span>/</span> profile</div>
      </div>
      <div className="mt-12 grid gap-8 lg:grid-cols-2">
        <ProfileSection />
        <AddressSection />
      </div>
    </section>
  );
}

// ---- 个人资料 ----

function ProfileSection() {
  const user = useAuthStore((state) => state.user);
  const setUser = useAuthStore((state) => state.setUser);
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);

  const [nickname, setNickname] = useState(user?.nickname || "");
  const [previewUrl, setPreviewUrl] = useState(""); // 本地预览（objectURL），空 = 未换头像
  const [pendingAvatar, setPendingAvatar] = useState<File | null>(null);
  const [notice, setNotice] = useState<{ kind: "ok" | "error"; text: string } | null>(null);
  const [avatarBroken, setAvatarBroken] = useState(false); // 私有桶匿名不可直读，失败回退字母头像
  const shownAvatarKey = previewUrl || user?.avatar_url || "";

  useEffect(() => {
    setNickname(user?.nickname || "");
  }, [user?.nickname]);
  // 头像来源变化后重置失败标记。
  useEffect(() => {
    setAvatarBroken(false);
  }, [shownAvatarKey]);

  const profileQuery = useQuery({ queryKey: ["me"], queryFn: authApi.me });

  // 服务端资料与本地 store 不一致时以服务端为准（如他处改过资料）。
  useEffect(() => {
    if (profileQuery.data && user && profileQuery.data.updated_at !== user.updated_at) {
      setUser(profileQuery.data);
    }
  }, [profileQuery.data, user, setUser]);

  const saveMutation = useMutation({
    mutationFn: async (request: UpdateProfileRequest) => {
      if (request.avatar_url === undefined && pendingAvatar) {
        const url = await uploadFile(pendingAvatar);
        return authApi.updateProfile({ ...request, avatar_url: url });
      }
      return authApi.updateProfile(request);
    },
    onSuccess: (updated) => {
      setUser(updated);
      queryClient.setQueryData(["me"], updated);
      setPendingAvatar(null);
      setPreviewUrl("");
      if (fileInputRef.current) fileInputRef.current.value = "";
      setNotice({ kind: "ok", text: "资料已更新。" });
    },
    onError: (error) => setNotice({ kind: "error", text: getApiErrorMessage(error) }),
  });

  function pickAvatar(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    if (!file) return;
    if (!["image/png", "image/jpeg", "image/webp", "image/gif"].includes(file.type)) {
      setNotice({ kind: "error", text: "头像仅支持 png / jpeg / webp / gif 图片。" });
      event.target.value = "";
      return;
    }
    if (file.size > maxAvatarBytes) {
      setNotice({ kind: "error", text: "头像图片不能超过 5MB。" });
      event.target.value = "";
      return;
    }
    setPendingAvatar(file);
    if (previewUrl) URL.revokeObjectURL(previewUrl); // 释放旧预览，避免 objectURL 泄漏
    setPreviewUrl(URL.createObjectURL(file));
    setNotice(null);
  }

  function clearAvatar() {
    if (previewUrl) URL.revokeObjectURL(previewUrl);
    setPendingAvatar(null);
    setPreviewUrl("");
    if (fileInputRef.current) fileInputRef.current.value = "";
  }

  function submitProfile(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const nicknameChanged = nickname.trim() !== (user?.nickname || "");
    if (!pendingAvatar && !nicknameChanged) {
      setNotice({ kind: "error", text: "没有要保存的修改。" });
      return;
    }
    const request: UpdateProfileRequest = {};
    if (nicknameChanged) request.nickname = nickname.trim(); // 头像（若有）由 mutation 上传后一并 PATCH
    setNotice(null);
    saveMutation.mutate(request);
  }

  if (profileQuery.isPending && !user) {
    return <div className="checkout-section"><LoadingBlock label="正在读取资料" /></div>;
  }
  if (profileQuery.isError) {
    return <div className="checkout-section"><ErrorState message={getApiErrorMessage(profileQuery.error)} onRetry={() => profileQuery.refetch()} /></div>;
  }

  const current = user;
  const shownAvatar = shownAvatarKey;
  const displayName = current?.nickname || current?.username || "";
  const showAvatarImage = Boolean(shownAvatar) && !avatarBroken;

  return (
    <section className="checkout-section">
      <div className="checkout-section-heading">
        <div><p className="eyebrow text-smoke">01 / 个人资料</p><h2 className="mt-2 font-nantes text-3xl">怎么称呼你？</h2></div>
      </div>

      {notice && <div className={`notice ${notice.kind === "ok" ? "notice-success" : "notice-error"} mt-6`}>{notice.text}</div>}

      <form className="mt-6 space-y-6" onSubmit={submitProfile}>
        <div className="flex items-center gap-5">
          {showAvatarImage ? (
            <img
              src={shownAvatar}
              alt="头像"
              className="friend-avatar"
              style={{ width: 64, height: 64, borderRadius: "var(--radius-md)", objectFit: "cover" }}
              onError={() => setAvatarBroken(true)}
            />
          ) : (
            <div className="friend-avatar" aria-hidden="true" style={{ width: 64, height: 64, fontSize: 24 }}>{displayName.slice(0, 1).toUpperCase()}</div>
          )}
          <div className="space-y-2">
            <input ref={fileInputRef} type="file" accept="image/png,image/jpeg,image/webp,image/gif" className="hidden" onChange={pickAvatar} aria-label="选择头像图片" />
            <div className="flex flex-wrap gap-2">
              <Button type="button" variant="secondary" size="small" onClick={() => fileInputRef.current?.click()} disabled={saveMutation.isPending}>
                <Icon name="image" size={15} /> {pendingAvatar ? "换一张" : "上传头像"}
              </Button>
              {pendingAvatar && (
                <Button type="button" variant="ghost" size="small" onClick={clearAvatar} disabled={saveMutation.isPending}>
                  移除预览
                </Button>
              )}
            </div>
            <p className="text-xs leading-5 text-smoke">png / jpeg / webp / gif，不超过 5MB；保存资料时一并上传。</p>
          </div>
        </div>

        <label className="form-label">
          <span>昵称</span>
          <input
            className="form-control"
            value={nickname}
            maxLength={32}
            onChange={(event) => setNickname(event.target.value)}
            placeholder={current?.username || "展示用昵称"}
          />
        </label>
        <p className="text-sm text-smoke">用户名 <strong>{current?.username}</strong>（登录凭据，不可修改）；未设置昵称时展示用户名。</p>

        <Button type="submit" disabled={saveMutation.isPending}>
          {saveMutation.isPending ? <Spinner label="正在保存" /> : <>保存资料 <Icon name="check" size={16} /></>}
        </Button>
      </form>
    </section>
  );
}

// ---- 地址簿 ----

function AddressSection() {
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<{ id: number | null; draft: CreateAddressRequest } | null>(null);
  const [actionError, setActionError] = useState("");

  const addressesQuery = useQuery({ queryKey: ["addresses"], queryFn: getAddresses });

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["addresses"] });
    // 结算页缓存了地址选择，一并失效。
    queryClient.invalidateQueries({ queryKey: ["checkout"] });
  };

  const saveMutation = useMutation({
    mutationFn: async ({ id, draft }: { id: number | null; draft: CreateAddressRequest }) => {
      if (id) {
        await updateAddress(id, draft);
        return;
      }
      return createAddress(draft);
    },
    onSuccess: () => {
      invalidate();
      setEditing(null);
      setActionError("");
    },
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });
  const deleteMutation = useMutation({
    mutationFn: (id: number) => deleteAddress(id),
    onSuccess: invalidate,
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });
  const defaultMutation = useMutation({
    mutationFn: (id: number) => setDefaultAddress(id),
    onSuccess: () => {
      invalidate();
      setActionError("");
    },
    onError: (error) => setActionError(getApiErrorMessage(error)),
  });

  if (addressesQuery.isPending) {
    return <div className="checkout-section"><LoadingBlock label="正在读取地址簿" /></div>;
  }
  if (addressesQuery.isError) {
    return <div className="checkout-section"><ErrorState message={getApiErrorMessage(addressesQuery.error)} onRetry={() => addressesQuery.refetch()} /></div>;
  }

  // 后端空列表序列化为 {"items":null}，统一收敛为空数组。
  const addresses = addressesQuery.data || [];
  const busy = saveMutation.isPending || deleteMutation.isPending || defaultMutation.isPending;

  function updateDraft(field: keyof CreateAddressRequest, value: string | boolean) {
    setEditing((current) => current && { ...current, draft: { ...current.draft, [field]: value } });
  }

  function submitAddress(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!editing) return;
    setActionError("");
    saveMutation.mutate(editing);
  }

  return (
    <section className="checkout-section">
      <div className="checkout-section-heading">
        <div><p className="eyebrow text-smoke">02 / 地址簿</p><h2 className="mt-2 font-nantes text-3xl">寄到哪里？</h2></div>
        {!editing && (
          <Button variant="secondary" onClick={() => setEditing({ id: null, draft: emptyDraft })}>
            <Icon name="plus" size={16} /> 新增地址
          </Button>
        )}
      </div>

      {actionError && <div className="notice notice-error mt-6">{actionError}</div>}

      {editing ? (
        <form className="mt-6" onSubmit={submitAddress}>
          <div className="form-grid-2">
            <label className="form-label"><span>收货人</span><input className="form-control" required value={editing.draft.receiver} onChange={(event) => updateDraft("receiver", event.target.value)} placeholder="姓名" /></label>
            <label className="form-label"><span>手机号</span><input className="form-control" required value={editing.draft.phone} onChange={(event) => updateDraft("phone", event.target.value)} placeholder="11 位手机号" inputMode="tel" /></label>
            <label className="form-label"><span>省</span><input className="form-control" required value={editing.draft.province} onChange={(event) => updateDraft("province", event.target.value)} placeholder="例如 江苏省" /></label>
            <label className="form-label"><span>市</span><input className="form-control" required value={editing.draft.city} onChange={(event) => updateDraft("city", event.target.value)} placeholder="例如 南通市" /></label>
            <label className="form-label"><span>区 / 县</span><input className="form-control" required value={editing.draft.district} onChange={(event) => updateDraft("district", event.target.value)} placeholder="例如 崇川区" /></label>
            <label className="form-label"><span>详细地址</span><input className="form-control" required value={editing.draft.detail} onChange={(event) => updateDraft("detail", event.target.value)} placeholder="街道、门牌号" /></label>
          </div>
          {editing.id ? (
            <p className="mt-5 text-xs leading-5 text-smoke">默认地址由列表"设为默认"切换（编辑不改动默认指向）。</p>
          ) : (
            <label className="checkbox-label mt-5"><input type="checkbox" checked={editing.draft.is_default} onChange={(event) => updateDraft("is_default", event.target.checked)} />设为默认地址</label>
          )}
          <div className="mt-5 flex gap-2">
            <Button type="submit" disabled={saveMutation.isPending}>{saveMutation.isPending ? <Spinner label="正在保存" /> : editing.id ? "保存修改" : "保存地址"}</Button>
            <Button type="button" variant="ghost" onClick={() => { setEditing(null); setActionError(""); }} disabled={saveMutation.isPending}>取消</Button>
          </div>
        </form>
      ) : addresses.length ? (
        <div className="address-list mt-6">
          {addresses.map((address) => (
            <AddressRow
              key={address.id}
              address={address}
              busy={busy}
              onEdit={() => setEditing({ id: address.id, draft: { receiver: address.receiver, phone: address.phone, province: address.province, city: address.city, district: address.district, detail: address.detail, is_default: address.is_default } })}
              onDelete={() => deleteMutation.mutate(address.id)}
              onSetDefault={() => defaultMutation.mutate(address.id)}
            />
          ))}
        </div>
      ) : (
        <div className="mt-6">
          <EmptyState eyebrow="空空如也" title="地址簿还没有记录。" description="新增一条地址，结算时就能直接选用。" />
        </div>
      )}
    </section>
  );
}

function AddressRow({
  address,
  busy,
  onEdit,
  onDelete,
  onSetDefault,
}: {
  address: Address;
  busy: boolean;
  onEdit: () => void;
  onDelete: () => void;
  onSetDefault: () => void;
}) {
  return (
    <div className="address-option">
      <span className="min-w-0 flex-1 text-left">
        <strong>{address.receiver}</strong>
        <span className="ml-3 text-sm text-smoke">{address.phone}</span>
        <small>{formatAddress(address)}</small>
      </span>
      {address.is_default && <span className="tag">默认</span>}
      <div className="flex flex-none gap-1">
        {!address.is_default && (
          <Button variant="ghost" size="small" disabled={busy} onClick={onSetDefault}>设为默认</Button>
        )}
        <Button variant="ghost" size="small" disabled={busy} onClick={onEdit}>编辑</Button>
        <Button variant="ghost" size="small" className="button-danger" disabled={busy} onClick={onDelete}>
          <Icon name="trash" size={15} /> 删除
        </Button>
      </div>
    </div>
  );
}
