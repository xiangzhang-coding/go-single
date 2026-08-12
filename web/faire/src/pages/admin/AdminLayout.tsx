import { Link, NavLink, Outlet } from "react-router-dom";

const adminTabs = [
  { to: "/admin", label: "商品管理", end: true },
  { to: "/admin/orders", label: "订单管理" },
  { to: "/admin/flashsales", label: "秒杀活动" },
  { to: "/admin/coupons", label: "券模板" },
];

export function AdminLayout() {
  return (
    <section className="site-container page-section pt-8 sm:pt-12">
      <div className="section-heading-row">
        <div>
          <p className="eyebrow text-smoke">后台管理 / 运营工作台</p>
          <h1 className="mt-3 font-nantes text-5xl">把商品、订单与活动，都打理清楚。</h1>
        </div>
        <div className="section-index" aria-hidden="true">10 <span>/</span> admin</div>
      </div>

      <div className="admin-tabs mt-10" aria-label="后台管理分区">
        {adminTabs.map((tab) => (
          <NavLink
            key={tab.to}
            to={tab.to}
            end={tab.end}
            className={({ isActive }) => (isActive ? "active" : "")}
          >
            {tab.label}
          </NavLink>
        ))}
        <Link to="/" className="admin-back-link">
          回到前台 ↗
        </Link>
      </div>

      <Outlet />
    </section>
  );
}
