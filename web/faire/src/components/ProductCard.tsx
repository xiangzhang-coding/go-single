import { Link } from "react-router-dom";

import type { ProductListItem } from "../api/types";
import { ProductVisual } from "./ui";
import { formatMoney } from "../lib/format";

export function ProductCard({ product }: { product: ProductListItem }) {
  return (
    <Link to={`/products/${product.id}`} className="product-card group">
      <ProductVisual seed={product.id} title={product.title} />
      <div className="product-card-copy">
        <p className="eyebrow text-smoke">商品 / {String(product.id).padStart(2, "0")}</p>
        <h2 className="mt-2 line-clamp-2 text-base text-ink-black">{product.title}</h2>
        <div className="product-card-meta mt-3">
          <span>{product.min_price != null ? `${formatMoney(product.min_price)} 起` : "尚未配置价格"}</span>
          <span className="transition-transform group-hover:translate-x-0.5" aria-hidden="true">查看 ↗</span>
        </div>
      </div>
    </Link>
  );
}
