import { api } from "./client";
import type {
  Address,
  Category,
  CartItem,
  CartItemView,
  ConversationListResponse,
  ConversationView,
  CouponListResponse,
  CouponTemplateListResponse,
  CouponTemplateView,
  CreateAddressRequest,
  CreateOrderRequest,
  FlashSaleListResponse,
  FriendRequest,
  FriendRequestView,
  FriendView,
  LoginResponse,
  Message,
  MessageListResponse,
  OrderListResponse,
  OrderView,
  Payment,
  Post,
  PostListResponse,
  ProductDetail,
  ProductListResponse,
  SendMessageRequest,
  SharePostRequest,
  User,
  UserCouponView,
  UserSearchResult,
} from "./types";

export const authApi = {
  async register(username: string, password: string) {
    const { data } = await api.post<User>("/auth/register", { username, password });
    return data;
  },

  async login(username: string, password: string) {
    const { data } = await api.post<LoginResponse>("/auth/login", { username, password });
    return data;
  },

  async me() {
    const { data } = await api.get<User>("/users/me");
    return data;
  },
};

export async function getCategories() {
  const { data } = await api.get<{ items: Category[] }>("/categories");
  return data.items;
}

export async function getProducts(params: { categoryId?: number; page: number; pageSize?: number }) {
  const { data } = await api.get<ProductListResponse>("/products", {
    params: {
      category_id: params.categoryId,
      page: params.page,
      page_size: params.pageSize || 20,
    },
  });
  return data;
}

export async function getProductDetail(productId: number) {
  const { data } = await api.get<ProductDetail>(`/products/${productId}`);
  return data;
}

export async function getCart() {
  const { data } = await api.get<{ items: CartItemView[] }>("/cart");
  return data;
}

export async function addCartItem(skuId: number, quantity: number) {
  const { data } = await api.post<CartItem>("/cart", {
    sku_id: skuId,
    quantity,
  });
  return data;
}

export async function updateCartItem(itemId: number, quantity: number) {
  await api.put(`/cart/items/${itemId}`, { quantity });
}

export async function deleteCartItem(itemId: number) {
  await api.delete(`/cart/items/${itemId}`);
}

export async function getAddresses() {
  const { data } = await api.get<{ items: Address[] }>("/addresses");
  return data.items;
}

export async function createAddress(request: CreateAddressRequest) {
  const { data } = await api.post<Address>("/addresses", request);
  return data;
}

export async function getMyCoupons(status?: string) {
  const { data } = await api.get<CouponListResponse>("/coupons/mine", {
    params: { status: status || "", page: 1, page_size: 50 },
  });
  return data;
}

export async function getClaimableCoupons() {
  const { data } = await api.get<CouponTemplateListResponse>("/coupons");
  return data.items;
}

export async function claimCoupon(templateId: number) {
  const { data } = await api.post<UserCouponView>(`/coupons/${templateId}/claim`);
  return data;
}

export async function getFlashSales() {
  const { data } = await api.get<FlashSaleListResponse>("/flashsales");
  return data;
}

export async function purchaseFlashSale(activityId: number) {
  const { data } = await api.post<{ status: "queued"; order_no: string; message: string }>(
    `/flashsales/${activityId}/purchase`,
  );
  return data;
}

export async function createOrder(request: CreateOrderRequest) {
  const { data } = await api.post<OrderView>("/orders", request);
  return data;
}

export async function getOrders(params: { status?: string; page: number; pageSize?: number }) {
  const { data } = await api.get<OrderListResponse>("/orders", {
    params: {
      status: params.status || undefined,
      page: params.page,
      page_size: params.pageSize || 10,
    },
  });
  return data;
}

export async function getOrder(orderNo: string) {
  const { data } = await api.get<OrderView>(`/orders/${orderNo}`);
  return data;
}

export async function cancelOrder(orderNo: string) {
  await api.post(`/orders/${orderNo}/cancel`);
}

export async function confirmOrder(orderNo: string) {
  await api.post(`/orders/${orderNo}/confirm`);
}

export async function mockPay(orderNo: string, amount: number, result: "success" | "fail") {
  const { data } = await api.post<Payment>("/payments/mock", {
    order_id: orderNo,
    payment_id: `${orderNo}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
    amount,
    result,
  });
  return data;
}

// ---- 好友 ----

export async function searchUsers(username: string) {
  const { data } = await api.get<{ items: UserSearchResult[] }>("/users", {
    params: { username, limit: 10 },
  });
  return data.items;
}

export async function sendFriendRequest(toUserId: number) {
  const { data } = await api.post<FriendRequest>("/friend-requests", { to_user_id: toUserId });
  return data;
}

export async function getFriendRequests(params: { scope: "incoming" | "outgoing"; status?: string }) {
  const { data } = await api.get<{ items: FriendRequestView[] }>("/friend-requests", {
    params: { scope: params.scope, status: params.status || undefined },
  });
  return data.items;
}

export async function acceptFriendRequest(requestId: number) {
  await api.post(`/friend-requests/${requestId}/accept`);
}

export async function rejectFriendRequest(requestId: number) {
  await api.post(`/friend-requests/${requestId}/reject`);
}

export async function getFriends() {
  const { data } = await api.get<{ items: FriendView[] }>("/friends");
  return data.items;
}

// ---- 好友圈 ----

export async function getFeed(params: { page: number; pageSize?: number }) {
  const { data } = await api.get<PostListResponse>("/posts/feed", {
    params: { page: params.page, page_size: params.pageSize || 20 },
  });
  return data;
}

export async function getMyPosts(params: { page: number; pageSize?: number }) {
  const { data } = await api.get<PostListResponse>("/posts/mine", {
    params: { page: params.page, page_size: params.pageSize || 20 },
  });
  return data;
}

export async function sharePost(request: SharePostRequest) {
  const { data } = await api.post<Post>("/posts", request);
  return data;
}

export async function deletePost(postId: number) {
  await api.delete(`/posts/${postId}`);
}

// ---- 聊天 ----

export async function getConversations(params: { beforeId?: number; limit?: number }) {
  const { data } = await api.get<ConversationListResponse>("/conversations", {
    params: { before_id: params.beforeId || undefined, limit: params.limit || 20 },
  });
  return data;
}

export async function getMessages(
  conversationKey: string,
  params: { afterId?: number; beforeId?: number; limit?: number },
) {
  const { data } = await api.get<MessageListResponse>(`/conversations/${conversationKey}/messages`, {
    params: {
      after_id: params.afterId || undefined,
      before_id: params.beforeId || undefined,
      limit: params.limit || 30,
    },
  });
  return data;
}

export async function sendMessage(request: SendMessageRequest) {
  const { data } = await api.post<Message>("/messages", request);
  return data;
}

export async function markConversationRead(conversationKey: string, lastMessageId: number) {
  await api.post(`/conversations/${conversationKey}/read`, { last_message_id: lastMessageId });
}

// ---- 文件上传（图片消息 / 动态配图）----

export async function uploadFile(file: File) {
  const form = new FormData();
  form.append("file", file);
  const { data } = await api.post<{ url: string }>("/files", form, {
    headers: { "Content-Type": "multipart/form-data" },
  });
  return data.url;
}
