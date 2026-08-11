export function formatMoney(cents: number) {
  return new Intl.NumberFormat("zh-CN", {
    style: "currency",
    currency: "CNY",
    minimumFractionDigits: 2,
  }).format(cents / 100);
}

export function formatDate(value?: string) {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

export function formatAddress(address: {
  province: string;
  city: string;
  district: string;
  detail: string;
}) {
  return `${address.province}${address.city}${address.district}${address.detail}`;
}

export function parseSpecs(value: unknown): Array<[string, string]> {
  let parsed = value;
  if (typeof value === "string") {
    try {
      parsed = JSON.parse(value);
    } catch {
      return value ? [["规格", value]] : [];
    }
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return parsed ? [["规格", String(parsed)]] : [];
  }
  return Object.entries(parsed).map(([key, item]) => [key, String(item)]);
}

export function formatSpecs(value: unknown) {
  return parseSpecs(value).map(([key, item]) => `${key}: ${item}`).join(" · ") || "标准规格";
}

export function makeClientRequestID() {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

export function isCouponUsable(coupon: {
  status: string;
  min_amount: number;
  valid_from: string;
  valid_until: string;
}, subtotal: number) {
  const now = Date.now();
  return (
    coupon.status === "unused" &&
    subtotal >= coupon.min_amount &&
    new Date(coupon.valid_from).getTime() <= now &&
    new Date(coupon.valid_until).getTime() >= now
  );
}
