import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AxiosResponse, InternalAxiosRequestConfig } from "axios";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../src/api/client";
import type { Address, CartItemView, User } from "../src/api/types";
import { App } from "../src/app";
import { clearCheckoutOperationByOrderNo } from "../src/lib/pending-operations";
import { CheckoutPage } from "../src/pages/CheckoutPage";
import { OrderDetailPage } from "../src/pages/OrderDetailPage";
import { useAuthStore } from "../src/store/auth";

const originalAdapter = api.defaults.adapter;
const orderNo = "ORDER-2026-0001";
const user: User = {
  id: 73,
  username: "checkout_user",
  nickname: "Checkout User",
  avatar_url: "",
  role: "user",
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};
const address: Address = {
  id: 31,
  user_id: user.id,
  receiver: "Lin",
  phone: "13800000000",
  province: "江苏省",
  city: "南通市",
  district: "崇川区",
  detail: "青年中路 1 号",
  is_default: true,
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};
const cartItem: CartItemView = {
  id: 11,
  user_id: user.id,
  sku_id: 19,
  quantity: 2,
  product_id: 7,
  title: "手工陶杯",
  specs: { color: "sand" },
  price: 6800,
  stock: 8,
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};

beforeEach(() => {
  api.defaults.adapter = originalAdapter;
  useAuthStore.setState({ token: null, user: null });
});

afterEach(() => {
  clearCheckoutOperationByOrderNo(user.id, orderNo);
  cleanup();
  api.defaults.adapter = originalAdapter;
  useAuthStore.setState({ token: null, user: null });
});

describe("frontend user-flow tracers", () => {
  test("redirects an unauthenticated protected route to login with returnTo", async () => {
    window.history.replaceState(null, "", "/orders/ORDER-42?tab=payment");
    const queryClient = createTestQueryClient();

    render(
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>,
    );

    const heading = await screen.findByRole("heading", { name: "登录 FAIRE" });
    expect(heading.textContent).toBe("登录 FAIRE");
    expect(`${window.location.pathname}${window.location.search}`).toBe(
      "/login?returnTo=%2Forders%2FORDER-42%3Ftab%3Dpayment",
    );

    queryClient.clear();
  });

  test("recovers a processing checkout without creating the order twice", async () => {
    useAuthStore.setState({ token: "checkout-token", user });
    let createOrderRequests = 0;
    api.defaults.adapter = async (config) => {
      if (config.method === "get" && config.url === "/cart") {
        return response(config, { items: [cartItem] });
      }
      if (config.method === "get" && config.url === "/addresses") {
        return response(config, { items: [address] });
      }
      if (config.method === "get" && config.url === "/coupons/mine") {
        return response(config, { items: [], total: 0 });
      }
      if (config.method === "post" && config.url === "/orders") {
        createOrderRequests += 1;
        return response(config, { state: "processing", order_no: orderNo });
      }
      if (config.method === "get" && config.url === `/orders/${orderNo}`) {
        return pendingResponse(config);
      }
      throw new Error(`Unexpected API request: ${config.method} ${config.url}`);
    };

    const checkoutClient = createTestQueryClient();
    const checkoutView = renderCheckoutRoutes(checkoutClient, ["/checkout"]);
    expect((await screen.findByRole("heading", { name: "把选择寄出去。" })).textContent).toBe("把选择寄出去。");
    expect(screen.getByText("手工陶杯").textContent).toBe("手工陶杯");

    const submit = screen.getByRole("button", { name: /提交订单/ }) as HTMLButtonElement;
    await waitFor(() => expect(submit.disabled).toBe(false));
    await userEvent.setup().click(submit);

    expect((await screen.findByRole("heading", { name: "订单正在生成。" })).textContent).toBe("订单正在生成。");
    expect(createOrderRequests).toBe(1);

    checkoutView.unmount();
    checkoutClient.clear();
    cleanup();

    const recoveryClient = createTestQueryClient();
    const recoveryView = renderCheckoutRoutes(recoveryClient, [`/orders/${orderNo}`]);

    expect((await screen.findByRole("heading", { name: "订单正在生成。" })).textContent).toBe("订单正在生成。");
    expect(screen.getByText("安全的重复提交仍在落库，页面会继续查询结果。").textContent).toContain("重复提交");
    expect(createOrderRequests).toBe(1);

    recoveryView.unmount();
    recoveryClient.clear();
  });
});

function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  });
}

function renderCheckoutRoutes(queryClient: QueryClient, initialEntries: string[]) {
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={initialEntries}>
        <Routes>
          <Route path="/checkout" element={<CheckoutPage />} />
          <Route path="/orders/:orderNo" element={<OrderDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function response<T>(config: InternalAxiosRequestConfig, data: T): AxiosResponse<T> {
  return {
    data,
    status: 200,
    statusText: "OK",
    headers: {},
    config,
  };
}

function pendingResponse(config: InternalAxiosRequestConfig): Promise<AxiosResponse<never>> {
  return new Promise((_resolve, reject) => {
    const signal = config.signal as AbortSignal | undefined;
    signal?.addEventListener("abort", () => reject(signal.reason));
  });
}
