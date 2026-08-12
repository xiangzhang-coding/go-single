import type { ButtonHTMLAttributes, ReactNode } from "react";

import type { OrderStatus } from "../api/types";

export type IconName =
  | "arrow-left"
  | "arrow-down"
  | "arrow-right"
  | "bag"
  | "check"
  | "chevron-down"
  | "clock"
  | "close"
  | "image"
  | "login"
  | "logout"
  | "message"
  | "minus"
  | "pin"
  | "plus"
  | "refresh"
  | "search"
  | "send"
  | "trash"
  | "user-plus";

export function Icon({ name, size = 18 }: { name: IconName; size?: number }) {
  const common = {
    width: size,
    height: size,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.6,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true,
  };

  switch (name) {
    case "arrow-left":
      return <svg {...common}><path d="m15 18-6-6 6-6" /></svg>;
    case "arrow-down":
      return <svg {...common}><path d="M12 5v14M6 13l6 6 6-6" /></svg>;
    case "arrow-right":
      return <svg {...common}><path d="m9 18 6-6-6-6" /></svg>;
    case "bag":
      return <svg {...common}><path d="M6 8h12l1 12H5L6 8Z" /><path d="M9 9V6a3 3 0 0 1 6 0v3" /></svg>;
    case "check":
      return <svg {...common}><path d="m5 12 4 4L19 6" /></svg>;
    case "chevron-down":
      return <svg {...common}><path d="m6 9 6 6 6-6" /></svg>;
    case "clock":
      return <svg {...common}><circle cx="12" cy="12" r="8.5" /><path d="M12 7v5l3 2" /></svg>;
    case "close":
      return <svg {...common}><path d="m6 6 12 12M18 6 6 18" /></svg>;
    case "image":
      return <svg {...common}><rect x="3.5" y="5" width="17" height="14" rx="1" /><circle cx="9" cy="10" r="1.6" /><path d="m5 18 5-5 3 3 3-3 3 3" /></svg>;
    case "message":
      return <svg {...common}><path d="M4 5.5A1.5 1.5 0 0 1 5.5 4h13A1.5 1.5 0 0 1 20 5.5v10a1.5 1.5 0 0 1-1.5 1.5H10l-4.5 3.8V17H5.5A1.5 1.5 0 0 1 4 15.5v-10Z" /></svg>;
    case "send":
      return <svg {...common}><path d="M4 11.5 20 4l-6.5 16-3-6.5L4 11.5Z" /><path d="M10.5 13.5 20 4" /></svg>;
    case "user-plus":
      return <svg {...common}><circle cx="9.5" cy="8" r="3.2" /><path d="M3.5 20c.6-3.2 3-5 6-5s5.4 1.8 6 5" /><path d="M18 9v5M15.5 11.5h5" /></svg>;
    case "login":
      return <svg {...common}><path d="M10 5H6a1 1 0 0 0-1 1v12a1 1 0 0 0 1 1h4" /><path d="m14 8 4 4-4 4M9 12h9" /></svg>;
    case "logout":
      return <svg {...common}><path d="M14 5h4a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1h-4" /><path d="m10 8-4 4 4 4M6 12h9" /></svg>;
    case "minus":
      return <svg {...common}><path d="M5 12h14" /></svg>;
    case "pin":
      return <svg {...common}><path d="M19 10c0 5-7 10-7 10S5 15 5 10a7 7 0 1 1 14 0Z" /><circle cx="12" cy="10" r="2" /></svg>;
    case "plus":
      return <svg {...common}><path d="M12 5v14M5 12h14" /></svg>;
    case "refresh":
      return <svg {...common}><path d="M20 11a8 8 0 0 0-14.7-3.8L4 9" /><path d="M4 4v5h5M4 13a8 8 0 0 0 14.7 3.8L20 15" /><path d="M20 20v-5h-5" /></svg>;
    case "search":
      return <svg {...common}><circle cx="10.8" cy="10.8" r="6.3" /><path d="m16 16 4 4" /></svg>;
    case "trash":
      return <svg {...common}><path d="M5 7h14M10 11v5M14 11v5M9 7V4h6v3m-9 0 1 13h10l1-13" /></svg>;
  }
}

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";

export function Button({
  children,
  variant = "primary",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
  children: ReactNode;
}) {
  const variants: Record<ButtonVariant, string> = {
    primary: "button button-primary",
    secondary: "button button-secondary",
    ghost: "button button-ghost",
    danger: "button button-danger",
  };

  return (
    <button className={`${variants[variant]} ${className}`} {...props}>
      {children}
    </button>
  );
}

export function Spinner({ label = "加载中" }: { label?: string }) {
  return (
    <span className="inline-flex items-center gap-2 text-sm text-smoke">
      <span className="spinner" aria-hidden="true" />
      {label}
    </span>
  );
}

export function LoadingBlock({ label = "正在读取目录" }: { label?: string }) {
  return (
    <div className="loading-block">
      <Spinner label={label} />
    </div>
  );
}

export function ErrorState({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div className="empty-state border border-ash bg-white">
      <p className="eyebrow text-smoke">暂时无法继续</p>
      <h2 className="mt-3 font-nantes text-2xl">这页没有正常抵达。</h2>
      <p className="mt-2 text-sm text-smoke">{message}</p>
      {onRetry && (
        <Button variant="secondary" className="mt-6" onClick={onRetry}>
          <Icon name="refresh" size={16} />
          再试一次
        </Button>
      )}
    </div>
  );
}

export function EmptyState({
  eyebrow = "空空如也",
  title,
  description,
  action,
}: {
  eyebrow?: string;
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return (
    <div className="empty-state border border-ash bg-white">
      <p className="eyebrow text-smoke">{eyebrow}</p>
      <h2 className="mt-3 font-nantes text-2xl">{title}</h2>
      {description && <p className="mt-2 text-sm text-smoke">{description}</p>}
      {action && <div className="mt-6">{action}</div>}
    </div>
  );
}

export function ProductVisual({ seed = 0, title = "FAIRE" }: { seed?: number; title?: string }) {
  const variant = Math.abs(seed) % 6;
  return (
    <div className={`product-visual product-visual-${variant}`} aria-hidden="true">
      <span className="product-visual-label">{title.slice(0, 1).toUpperCase()}</span>
      <span className="product-visual-mark">FAIRE</span>
      <span className="product-visual-dot" />
    </div>
  );
}

export function QuantityStepper({
  value,
  onChange,
  min = 1,
  max = 99,
  disabled = false,
}: {
  value: number;
  onChange: (next: number) => void;
  min?: number;
  max?: number;
  disabled?: boolean;
}) {
  return (
    <div className="quantity-stepper" aria-label="数量">
      <button
        type="button"
        aria-label="减少数量"
        disabled={disabled || value <= min}
        onClick={() => onChange(Math.max(min, value - 1))}
      >
        <Icon name="minus" size={15} />
      </button>
      <span aria-live="polite">{value}</span>
      <button
        type="button"
        aria-label="增加数量"
        disabled={disabled || value >= max}
        onClick={() => onChange(Math.min(max, value + 1))}
      >
        <Icon name="plus" size={15} />
      </button>
    </div>
  );
}

const statusLabels: Record<OrderStatus, string> = {
  pending_payment: "待支付",
  paid: "已支付",
  shipped: "已发货",
  completed: "已完成",
  cancelled: "已取消",
};

export function StatusBadge({ status }: { status: OrderStatus | "" }) {
  if (!status) {
    return <span className="status-badge status-processing">处理中</span>;
  }
  return <span className={`status-badge status-${status}`}>{statusLabels[status]}</span>;
}
