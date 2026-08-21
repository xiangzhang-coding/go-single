import { useEffect, type ReactNode } from "react";
import {
  BrowserRouter,
  Navigate,
  Route,
  Routes,
  useLocation,
} from "react-router-dom";

import { AppShell } from "./components/AppShell";
import { AuthPage } from "./pages/AuthPage";
import { CartPage } from "./pages/CartPage";
import { ChatPage } from "./pages/ChatPage";
import { CheckoutPage } from "./pages/CheckoutPage";
import { CouponsPage } from "./pages/CouponsPage";
import { FeedPage } from "./pages/FeedPage";
import { FlashSalePage } from "./pages/FlashSalePage";
import { FriendsPage } from "./pages/FriendsPage";
import { HomePage } from "./pages/HomePage";
import { NotFoundPage } from "./pages/NotFoundPage";
import { OrderDetailPage } from "./pages/OrderDetailPage";
import { OrdersPage } from "./pages/OrdersPage";
import { ProductDetailPage } from "./pages/ProductDetailPage";
import { ProfilePage } from "./pages/ProfilePage";
import { AdminCouponsPage } from "./pages/admin/AdminCouponsPage";
import { AdminFlashSalesPage } from "./pages/admin/AdminFlashSalesPage";
import { AdminLayout } from "./pages/admin/AdminLayout";
import { AdminOrdersPage } from "./pages/admin/AdminOrdersPage";
import { AdminProductsPage } from "./pages/admin/AdminProductsPage";
import { useChatRealtime } from "./lib/chat-hooks";
import { AUTH_SESSION_KEY, parseStoredSession } from "./lib/auth-storage";
import { endSession, syncSessionFromStorage } from "./lib/session";
import { useAuthStore } from "./store/auth";

function ProtectedRoute({ children }: { children: ReactNode }) {
  const { token, user } = useAuthStore();
  const location = useLocation();

  if (!token || !user) {
    const returnTo = `${location.pathname}${location.search}`;
    return <Navigate to={`/login?returnTo=${encodeURIComponent(returnTo)}`} replace />;
  }
  return children;
}

// AdminRoute 后台路由守卫（T25）：已登录 + admin 角色才放行；
// 普通用户重定向回首页（前端隐藏，后端 RequireAdmin 另有 403 兜底）。
function AdminRoute({ children }: { children: ReactNode }) {
  const { token, user } = useAuthStore();
  const location = useLocation();

  if (!token || !user) {
    const returnTo = `${location.pathname}${location.search}`;
    return <Navigate to={`/login?returnTo=${encodeURIComponent(returnTo)}`} replace />;
  }
  if (user.role !== "admin") {
    return <Navigate to="/" replace />;
  }
  return children;
}

function GuestRoute({ children }: { children: ReactNode }) {
  const { token, user } = useAuthStore();
  if (token && user) {
    return <Navigate to="/" replace />;
  }
  return children;
}

function SessionEvents() {
  useEffect(() => {
    const handleSessionExpired = () => endSession();
    const handleStorage = (event: StorageEvent) => {
      if (event.key === AUTH_SESSION_KEY) {
        syncSessionFromStorage(parseStoredSession(event.newValue));
      }
    };
    window.addEventListener("faire:session-expired", handleSessionExpired);
    window.addEventListener("storage", handleStorage);
    return () => {
      window.removeEventListener("faire:session-expired", handleSessionExpired);
      window.removeEventListener("storage", handleStorage);
    };
  }, []);

  return null;
}

export function App() {
  useChatRealtime();

  return (
    <BrowserRouter>
      <SessionEvents />
      <Routes>
        <Route
          path="/login"
          element={
            <GuestRoute>
              <AuthPage mode="login" />
            </GuestRoute>
          }
        />
        <Route
          path="/register"
          element={
            <GuestRoute>
              <AuthPage mode="register" />
            </GuestRoute>
          }
        />
        <Route element={<AppShell />}>
          <Route index element={<HomePage />} />
          <Route path="products/:id" element={<ProductDetailPage />} />
          <Route
            path="cart"
            element={
              <ProtectedRoute>
                <CartPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="checkout"
            element={
              <ProtectedRoute>
                <CheckoutPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="orders"
            element={
              <ProtectedRoute>
                <OrdersPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="orders/:orderNo"
            element={
              <ProtectedRoute>
                <OrderDetailPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="flash-sale"
            element={
              <ProtectedRoute>
                <FlashSalePage />
              </ProtectedRoute>
            }
          />
          <Route
            path="coupons"
            element={
              <ProtectedRoute>
                <CouponsPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="friends"
            element={
              <ProtectedRoute>
                <FriendsPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="feed"
            element={
              <ProtectedRoute>
                <FeedPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="chat"
            element={
              <ProtectedRoute>
                <ChatPage />
              </ProtectedRoute>
            }
          />
          <Route
            path="profile"
            element={
              <ProtectedRoute>
                <ProfilePage />
              </ProtectedRoute>
            }
          />
          <Route
            path="admin"
            element={
              <AdminRoute>
                <AdminLayout />
              </AdminRoute>
            }
          >
            <Route index element={<AdminProductsPage />} />
            <Route path="orders" element={<AdminOrdersPage />} />
            <Route path="flashsales" element={<AdminFlashSalesPage />} />
            <Route path="coupons" element={<AdminCouponsPage />} />
          </Route>
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
