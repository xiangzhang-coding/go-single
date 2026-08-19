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
  CreateCouponTemplateRequest,
  CreateFlashSaleRequest,
  CreateOrderRequest,
  CreateProductRequest,
  CreateSKURequest,
  FlashSaleActivity,
  FlashSaleAdminListResponse,
  FlashSaleListResponse,
  FlashSalePurchase,
  FlashSalePurchaseResponse,
  FriendRequest,
  FriendRequestListResponse,
  FriendView,
  LoginResponse,
  Message,
  MessageListResponse,
  OrderListResponse,
  OrderView,
  Payment,
  Post,
  PostListResponse,
  Product,
  ProductDetail,
  ProductListResponse,
  SendMessageRequest,
  SharePostRequest,
  SKU,
  User,
  UpdateProfileRequest,
  UploadedMedia,
  MediaKind,
  UserCoupon,
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

  // 个人资料：PATCH 部分更新（未提交字段不动、空串清空）。
  async updateProfile(request: UpdateProfileRequest) {
    const { data } = await api.patch<User>("/users/me", request);
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

// 编辑地址（不触碰默认指向；后端返回 204 无 body）。
export async function updateAddress(id: number, request: CreateAddressRequest) {
  await api.put(`/addresses/${id}`, request);
}

export async function deleteAddress(id: number) {
  await api.delete(`/addresses/${id}`);
}

export async function setDefaultAddress(id: number) {
  await api.put(`/addresses/${id}/default`);
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
  const { data } = await api.post<UserCoupon>(`/coupons/${templateId}/claim`);
  return data;
}

export async function getFlashSales() {
  const { data } = await api.get<FlashSaleListResponse>("/flashsales");
  return data;
}

export async function purchaseFlashSale(activityId: number, clientRequestId: string) {
	const { data } = await api.post<FlashSalePurchaseResponse>(
		`/flashsales/${activityId}/purchase`,
		{ client_request_id: clientRequestId },
	);
  return data;
}

export async function getFlashSalePurchase(id: string) {
  const { data } = await api.get<FlashSalePurchase>(`/flashsales/purchases/${id}`);
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

export async function getFriendRequests(params: { scope: "incoming" | "outgoing"; status?: string; page: number; pageSize?: number }) {
  const { data } = await api.get<FriendRequestListResponse>("/friend-requests", {
    params: {
      scope: params.scope,
      status: params.status || undefined,
      page: params.page,
      page_size: params.pageSize || 20,
    },
  });
  return data;
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

export async function uploadFile(file: File, kind: MediaKind = "image") {
  const form = new FormData();
  form.append("file", file);
  form.append("kind", kind);
  const { data } = await api.post<UploadedMedia>("/files", form, {
    headers: { "Content-Type": "multipart/form-data" },
  });
  return data;
}

export async function getMedia(reference: string) {
  if (!reference.startsWith("/files/")) {
    throw new Error("无效的媒体引用");
  }
  const response = await api.get<Blob>(reference, { responseType: "blob" });
  return {
    blob: response.data,
    filename: filenameFromDisposition(response.headers["content-disposition"]),
  };
}

function filenameFromDisposition(value?: string): string {
  if (!value) return "download";
  const encoded = value.match(/filename\*=utf-8''([^;]+)/i)?.[1];
  if (encoded) {
    try {
      return decodeURIComponent(encoded);
    } catch {
      return "download";
    }
  }
  return value.match(/filename="?([^";]+)"?/i)?.[1] || "download";
}

// ---- 后台管理（T25，admin 角色；后端 RequireAdmin 兜底）----

export const adminApi = {
  // ---- 商品 ----
  async getCategories() {
    const { data } = await api.get<{ items: Category[] }>("/categories");
    return data.items;
  },
  async createCategory(name: string) {
    const { data } = await api.post<Category>("/admin/categories", { name });
    return data;
  },
  async updateCategory(id: number, name: string) {
    await api.put(`/admin/categories/${id}`, { name });
  },
  async deleteCategory(id: number) {
    await api.delete(`/admin/categories/${id}`);
  },
  async getProducts(params: { status?: string; page: number; pageSize?: number }) {
    const { data } = await api.get<ProductListResponse>("/admin/products", {
      params: {
        status: params.status || undefined,
        page: params.page,
        page_size: params.pageSize || 20,
      },
    });
    return data;
  },
  async createProduct(request: CreateProductRequest) {
    const { data } = await api.post<Product>("/admin/products", request);
    return data;
  },
  async updateProduct(id: number, request: CreateProductRequest) {
    await api.put(`/admin/products/${id}`, request);
  },
  async publishProduct(id: number) {
    await api.post(`/admin/products/${id}/publish`);
  },
  async unpublishProduct(id: number) {
    await api.post(`/admin/products/${id}/unpublish`);
  },
  async getProductDetail(productId: number) {
    const { data } = await api.get<ProductDetail>(`/products/${productId}`);
    return data;
  },
  async createSKU(productId: number, request: CreateSKURequest) {
    const { data } = await api.post<SKU>(`/admin/products/${productId}/skus`, request);
    return data;
  },
  async updateSKU(id: number, request: CreateSKURequest) {
    await api.put(`/admin/skus/${id}`, request);
  },
  async deleteSKU(id: number) {
    await api.delete(`/admin/skus/${id}`);
  },

  // ---- 订单 ----
  async getOrders(params: { status?: string; page: number; pageSize?: number }) {
    const { data } = await api.get<OrderListResponse>("/admin/orders", {
      params: {
        status: params.status || undefined,
        page: params.page,
        page_size: params.pageSize || 20,
      },
    });
    return data;
  },
  async shipOrder(orderNo: string) {
    await api.post(`/admin/orders/${orderNo}/ship`);
  },

  // ---- 秒杀活动 ----
  async getFlashSales() {
    const { data } = await api.get<FlashSaleAdminListResponse>("/admin/flashsales");
    return data.items;
  },
  async createFlashSale(request: CreateFlashSaleRequest) {
    const { data } = await api.post<FlashSaleActivity>("/admin/flashsales", request);
    return data;
  },
  async updateFlashSale(id: number, request: CreateFlashSaleRequest) {
    await api.put(`/admin/flashsales/${id}`, request);
  },
  async publishFlashSale(id: number) {
    await api.post(`/admin/flashsales/${id}/publish`);
  },
  async unpublishFlashSale(id: number) {
    await api.post(`/admin/flashsales/${id}/unpublish`);
  },

  // ---- 券模板 ----
  async getCouponTemplates() {
    const { data } = await api.get<CouponTemplateListResponse>("/admin/coupons");
    return data.items;
  },
  async createCouponTemplate(request: CreateCouponTemplateRequest) {
    const { data } = await api.post<CouponTemplateView>("/admin/coupons", request);
    return data;
  },
  async updateCouponTemplate(id: number, request: CreateCouponTemplateRequest) {
    await api.put(`/admin/coupons/${id}`, request);
  },
};
