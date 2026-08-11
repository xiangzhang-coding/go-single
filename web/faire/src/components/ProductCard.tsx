import { Link } from "react-router-dom";

import type { Product } from "../api/types";
import { ProductVisual } from "./ui";

export function ProductCard({ product }: { product: Product }) {
  return (
    <Link to={`/products/${product.id}`} className="product-card group">
      <ProductVisual seed={product.id} title={product.title} />
      <div className="product-card-copy">
        <p className="eyebrow text-smoke">商品 / {String(product.id).padStart(2, "0")}</p>
        <h2 className="mt-2 line-clamp-2 text-base text-ink-black">{product.title}</h2>
        <p className="mt-3 text-sm text-smoke transition-colors group-hover:text-ink-black">
          查看规格与库存 <span aria-hidden="true">↗</span>
        </p>
      </div>
    </Link>
  );
}
