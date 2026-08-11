import { Link } from "react-router-dom";

import { Button, Icon } from "../components/ui";

export function NotFoundPage() {
  return (
    <section className="site-container page-section">
      <div className="empty-state border border-ash bg-white">
        <p className="eyebrow text-smoke">404 / 找不到页面</p>
        <h1 className="mt-3 font-nantes text-5xl">这页不在目录里。</h1>
        <p className="mt-3 text-sm text-smoke">回到首页，继续从商品目录开始。</p>
        <Link to="/" className="mt-7 inline-flex"><Button>返回目录 <Icon name="arrow-right" size={16} /></Button></Link>
      </div>
    </section>
  );
}
