export interface ErrorResponse {
  error: string;
}

export interface User {
  id: number;
  username: string;
  nickname: string;
  avatar_url: string;
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

export type CouponType = "direct" | "threshold";
export type UserCouponStatus = "unused" | "used" | "expired";
export type UserCouponListStatus = "" | UserCouponStatus;

export interface UserCouponView {
  id: number;
  template_id: number;
  name: string;
  type: CouponType;
  value: number;
  min_amount: number;
  status: UserCouponStatus;
  valid_from: string;
  valid_until: string;
  used_at?: string;
  created_at: string;
}

export interface UserCoupon {
  id: number;
  user_id: number;
  template_id: number;
  status: Exclude<UserCouponStatus, "expired">;
  used_at?: string;
  created_at: string;
  updated_at: string;
}

export type CouponTemplateState = "claimable" | "not_started" | "ended" | "sold_out" | "limit_reached";

export interface CouponTemplateRecord {
  id: number;
  name: string;
  type: CouponType;
  value: number;
  min_amount: number;
  total: number;
  per_user_limit: number;
  valid_from: string;
  valid_until: string;
  created_at: string;
  updated_at: string;
}

export interface AdminCouponTemplateView extends CouponTemplateRecord {
  claimed_count: number;
  state: "";
}

export interface ClaimableCouponTemplateView extends CouponTemplateRecord {
  claimed_count: number;
  state: CouponTemplateState;
}

export type FlashSaleState = "not_started" | "in_progress" | "off_sale" | "ended";

export interface FlashSaleSKU {
  id: number;
  product_id: number;
  specs: Record<string, string> | string | unknown;
  price: number;
}

export interface FlashSaleActivityRecord {
  id: number;
  sku_id: number;
  title: string;
  price: number;
  stock: number;
  per_user_limit: number;
  status: string;
  start_at: string;
  end_at: string;
  created_at: string;
  updated_at: string;
}

export interface FlashSaleActivity extends FlashSaleActivityRecord {
  state: FlashSaleState;
  product_title: string;
  sku: FlashSaleSKU;
}

export interface FlashSaleListResponse {
  server_time: string;
  items: FlashSaleActivity[];
}

export type FlashSalePurchaseStatus =
  | "preparing"
  | "pending_publish"
  | "pending_order"
  | "ordered"
  | "pending_rollback"
  | "rolled_back";

export interface FlashSalePurchase {
  id: string;
  status: FlashSalePurchaseStatus;
  order_no?: string;
  created_at: string;
  updated_at: string;
  ordered_at?: string;
  rolled_back_at?: string;
}

export interface FlashSalePurchaseResponse {
  pre_deduction_id: string;
  order_no?: string | null;
  status: "queued" | "rolled_back";
  pre_deduction_status: FlashSalePurchaseStatus;
  message: string;
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
  status: OrderStatus;
  activity_id?: number;
  purchase_slot?: string;
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

export interface OrderProcessingResponse {
  state: "processing";
  order_no: string;
}

export type CreateOrderResponse = OrderView | OrderProcessingResponse;

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

export interface AdminCouponTemplateListResponse {
  items: AdminCouponTemplateView[];
}

export interface ClaimableCouponTemplateListResponse {
  items: ClaimableCouponTemplateView[];
}

export interface LoginResponse {
  token: string;
  user: User;
}

// ---- 后台管理（T25）----

export interface CreateProductRequest {
  category_id: number;
  title: string;
  description?: string;
}

export interface CreateSKURequest {
  specs: unknown;
  price: number;
  stock: number;
}

export interface UpdateSKURequest extends CreateSKURequest {
  expected_stock: number;
}

export interface CreateFlashSaleRequest {
  sku_id: number;
  title: string;
  price: number;
  stock: number;
  per_user_limit?: number;
  start_at: string;
  end_at: string;
}

export interface CreateCouponTemplateRequest {
  name: string;
  type: CouponType;
  value: number;
  min_amount: number;
  total: number;
  per_user_limit: number;
  valid_from: string;
  valid_until: string;
}

export interface FlashSaleAdminListResponse {
  items: FlashSaleActivity[];
}

interface CreateOrderRequestBase {
  client_request_id: string;
  address_id: number;
  coupon_id?: number;
}

interface CreateOrderItemRequest {
  sku_id: number;
  quantity: number;
}

export type CreateOrderRequest =
  | (CreateOrderRequestBase & { from_cart: true; items?: never })
  | (CreateOrderRequestBase & {
      from_cart: false;
      items: [CreateOrderItemRequest, ...CreateOrderItemRequest[]];
    });

export interface UpdateAddressRequest {
  receiver: string;
  phone: string;
  province: string;
  city: string;
  district: string;
  detail: string;
}

export interface CreateAddressRequest extends UpdateAddressRequest {
  is_default: boolean;
}

// 个人资料（PATCH 语义：undefined = 不改动，空串 = 清空）。
export interface UpdateProfileRequest {
  nickname?: string;
  avatar_url?: string;
}

export type MediaKind = "image" | "file";

export interface UploadedMedia {
  url: string;
  kind: MediaKind;
  filename: string;
  content_type: string;
  size: number;
}

// ---- 社交：好友 ----

export type FriendRequestStatus = "pending" | "accepted" | "rejected";

export interface FriendRequest {
  id: number;
  from_user_id: number;
  to_user_id: number;
  status: FriendRequestStatus;
  created_at: string;
  updated_at: string;
}

export interface FriendRequestView extends FriendRequest {
  peer_username: string;
}

export interface FriendRequestListResponse {
  items: FriendRequestView[];
  total: number;
}

export interface FriendView {
  user_id: number;
  username: string;
  since: string;
}

export interface UserSearchResult {
  id: number;
  username: string;
}

// ---- 社交：好友圈动态 ----

export interface Post {
  id: number;
  user_id: number;
  sku_id: number;
  content?: string;
  image_url?: string;
  created_at: string;
  updated_at: string;
}

export interface PostView extends Post {
  author_username: string;
}

export interface PostListResponse {
  items: PostView[];
  total: number;
}

export interface SharePostRequest {
  sku_id: number;
  content?: string;
  image_url?: string;
}

// ---- 聊天 ----

export type MessageType = "text" | "image" | "file";

export interface Message {
  id: number;
  conversation_key: string;
  sender_id: number;
  recipient_id: number;
  type: MessageType;
  content?: string;
  url?: string;
  created_at: string;
}

export interface ConversationView {
  conversation_key: string;
  peer_user_id: number;
  peer_username: string;
  last_message?: Message;
  unread_count: number;
}

export interface ConversationListResponse {
  items: ConversationView[];
  has_more: boolean;
}

export interface MessageListResponse {
  items: Message[];
  has_more: boolean;
}

export interface SendMessageRequest {
  to_user_id: number;
  type: MessageType;
  content?: string;
  url?: string;
  client_request_id: string;
}
