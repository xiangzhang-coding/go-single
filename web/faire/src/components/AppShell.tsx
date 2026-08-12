import { Link, NavLink, Outlet, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { getCart } from "../api/endpoints";
import { Icon } from "./ui";
import { useAuthStore } from "../store/auth";

export function AppShell() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { token, user, logout } = useAuthStore();
  const cartQuery = useQuery({
    queryKey: ["cart"],
    queryFn: getCart,
    enabled: Boolean(token),
    staleTime: 15_000,
  });
  const cartCount = cartQuery.data?.items.reduce((sum, item) => sum + item.quantity, 0) || 0;

  function signOut() {
    logout();
    queryClient.clear();
    navigate("/");
  }

  return (
    <div className="app-shell">
      <div className="promo-bar">
        <div className="site-container flex items-center justify-between gap-4">
          <span>把日常用品，挑成自己的目录。</span>
          <Link to={user ? "/orders" : "/register"} className="promo-link">
            {user ? "查看订单" : "注册后开始购买"} <span aria-hidden="true">↗</span>
          </Link>
        </div>
      </div>

      <header className="site-header">
        <div className="site-container site-header-main">
          <Link to="/" className="brand-mark" aria-label="FAIRE 首页">
            FAIRE<span>/</span>
          </Link>

          <div className="header-actions">
            {user ? (
              <>
                <span className="header-user" title={user.role === "admin" ? "管理员" : "已登录"}>
                  {user.username}
                </span>
                <button type="button" className="text-link" onClick={signOut}>
                  <Icon name="logout" size={15} />
                  退出
                </button>
              </>
            ) : (
              <>
                <Link to="/login" className="text-link">
                  <Icon name="login" size={15} />
                  登录
                </Link>
                <Link to="/register" className="button button-primary button-small">
                  注册购买
                </Link>
              </>
            )}
          </div>
        </div>

        <div className="site-container site-nav-row">
          <nav className="site-nav" aria-label="主导航">
            <NavLink to="/" end className={({ isActive }) => (isActive ? "active" : "")}>
              目录
            </NavLink>
            <NavLink to="/flash-sale" className={({ isActive }) => (isActive ? "active" : "")}>
              秒杀
            </NavLink>
            <NavLink to="/coupons" className={({ isActive }) => (isActive ? "active" : "")}>
              优惠券
            </NavLink>
            <NavLink to="/orders" className={({ isActive }) => (isActive ? "active" : "")}>
              我的订单
            </NavLink>
            <NavLink to="/cart" className={({ isActive }) => (isActive ? "active" : "")}>
              购物车{cartCount > 0 && <span className="nav-count">{cartCount}</span>}
            </NavLink>
          </nav>
          <span className="nav-note">安静浏览，认真下单</span>
        </div>
      </header>

      <main>
        <Outlet />
      </main>

      <footer className="site-footer">
        <div className="site-container flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
          <div>
            <p className="brand-mark brand-mark-footer">FAIRE<span>/</span></p>
            <p className="mt-2 max-w-sm text-sm leading-6 text-smoke">一套连接商品、购物车和订单的演示目录。每一笔金额都按分计算。</p>
          </div>
          <p className="text-xs uppercase tracking-[0.16em] text-smoke">Go Single / Faire Theme</p>
        </div>
      </footer>
    </div>
  );
}
