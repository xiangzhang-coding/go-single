export interface User {
  id: number;
  username: string;
  role: "user" | "admin";
  created_at: string;
  updated_at: string;
}

export interface Category {
  id: number;
  name: string;
  created_at: string;
  updated_at: string;
}

export interface Product {
  id: number;
  category_id: number;
  title: string;
  description: string;
  status: string;
  created_at: string;
  updated_at: string;
}

export interface SKU {
  id: number;
  product_id: number;
  specs: Record<string, string> | string | unknown;
  price: number;
  stock: number;
  created_at: string;
  updated_at: string;
}

export interface ProductDetail extends Product {
  skus: SKU[];
}

export interface CartItem {
  id: number;
  user_id: number;
  sku_id: number;
  quantity: number;
  created_at: string;
  updated_at: string;
}

export interface CartItemView extends CartItem {
  product_id: number;
  title: string;
  specs: Record<string, string> | string | unknown;
  price: number;
  stock: number;
}

export interface Address {
  id: number;
  user_id: number;
  receiver: string;
  phone: string;
  province: string;
  city: string;
  district: string;
  detail: string;
  is_default: boolean;
  created_at: string;
  updated_at: string;
}

export interface UserCouponView {
  id: number;
  template_id: number;
  name: string;
  type: "direct" | "threshold" | string;
  value: number;
  min_amount: number;
  status: "unused" | "used" | "expired" | string;
  valid_from: string;
  valid_until: string;
  used_at?: string;
  created_at: string;
}

export type OrderStatus =
  | "pending_payment"
  | "paid"
  | "shipped"
  | "completed"
  | "cancelled";

export interface OrderItem {
  id: number;
  order_no: string;
  sku_id: number;
  product_id: number;
  title: string;
  specs: Record<string, string> | string | unknown;
  price: number;
  quantity: number;
  subtotal: number;
  created_at: string;
  updated_at: string;
}

export interface Order {
  order_no: string;
  user_id: number;
  order_type: "normal" | "seckill" | string;
  status: OrderStatus | "";
  activity_id?: number;
  total_amount: number;
  discount_amount: number;
  pay_amount: number;
  coupon_id?: number;
  receiver: string;
  phone: string;
  province: string;
  city: string;
  district: string;
  detail: string;
  paid_at?: string;
  shipped_at?: string;
  completed_at?: string;
  cancelled_at?: string;
  expire_at: string;
  created_at: string;
  updated_at: string;
}

export interface OrderView extends Order {
  items: OrderItem[];
}

export interface Payment {
  id: number;
  payment_id: string;
  order_no: string;
  user_id: number;
  amount: number;
  result: "success" | "fail" | string;
  created_at: string;
  updated_at: string;
}

export interface ProductListResponse {
  items: Product[];
  total: number;
}

export interface OrderListResponse {
  orders: OrderView[];
  total: number;
}

export interface CouponListResponse {
  items: UserCouponView[];
  total: number;
}

export interface LoginResponse {
  token: string;
  user: User;
}

export interface CreateOrderRequest {
  client_request_id: string;
  address_id: number;
  coupon_id?: number;
  from_cart: boolean;
  items?: Array<{ sku_id: number; quantity: number }>;
}

export interface CreateAddressRequest {
  receiver: string;
  phone: string;
  province: string;
  city: string;
  district: string;
  detail: string;
  is_default: boolean;
}
