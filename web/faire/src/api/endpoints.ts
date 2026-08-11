import { api } from "./client";
import type {
  Address,
  Category,
  CartItem,
  CartItemView,
  CouponListResponse,
  CreateAddressRequest,
  CreateOrderRequest,
  LoginResponse,
  OrderListResponse,
  OrderView,
  Payment,
  ProductDetail,
  ProductListResponse,
  User,
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

export async function getMyCoupons() {
  const { data } = await api.get<CouponListResponse>("/coupons/mine", {
    params: { status: "unused", page: 1, page_size: 50 },
  });
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
