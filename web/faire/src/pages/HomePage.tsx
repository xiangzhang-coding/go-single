import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { getCategories, getProducts } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import { ProductCard } from "../components/ProductCard";
import { Button, ErrorState, Icon, LoadingBlock } from "../components/ui";

export function HomePage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const page = Math.max(1, Number(searchParams.get("page")) || 1);
  const categoryValue = searchParams.get("category");
  const parsedCategoryId = categoryValue ? Number(categoryValue) : NaN;
  const categoryId = Number.isSafeInteger(parsedCategoryId) && parsedCategoryId > 0 ? parsedCategoryId : undefined;
  const categoriesQuery = useQuery({
    queryKey: ["categories"],
    queryFn: getCategories,
    staleTime: 5 * 60_000,
  });
  const productsQuery = useQuery({
    queryKey: ["products", categoryId, page],
    queryFn: () => getProducts({ categoryId, page, pageSize: 12 }),
  });

  const products = productsQuery.data?.items || [];
  const total = productsQuery.data?.total || 0;
  const totalPages = Math.max(1, Math.ceil(total / 12));

  function chooseCategory(nextCategory?: number) {
    const next = new URLSearchParams(searchParams);
    if (nextCategory) {
      next.set("category", String(nextCategory));
    } else {
      next.delete("category");
    }
    next.delete("page");
    setSearchParams(next);
  }

  function changePage(nextPage: number) {
    const safePage = Math.min(totalPages, Math.max(1, nextPage));
    const next = new URLSearchParams(searchParams);
    if (safePage === 1) {
      next.delete("page");
    } else {
      next.set("page", String(safePage));
    }
    setSearchParams(next);
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  return (
    <>
      <section className="site-container page-section pt-8 sm:pt-14">
        <div className="hero-panel">
          <div className="hero-copy">
            <p className="eyebrow text-smoke">FAIRE / 日常商品目录</p>
            <h1 className="mt-5 font-nantes text-5xl leading-[1.08] sm:text-6xl">
              日常用品，<br /><span className="hero-underlined">值得慢慢挑。</span>
            </h1>
            <p className="mt-6 max-w-lg text-base leading-7 text-charcoal">
              从一件好用的商品开始，把生活里真正需要的东西收进同一张清单。
            </p>
            <Link to="#catalog" className="button button-primary mt-8 inline-flex">
              浏览商品 <Icon name="arrow-down" size={17} />
            </Link>
          </div>
          <div className="hero-collage" role="img" aria-label="商品目录预览">
            <div className="hero-collage-note">编辑精选<br /><span>03 / 目录页</span></div>
            <div className="hero-art-main"><span>OBJECTS<br />FOR<br />EVERYDAY</span></div>
            <div className="hero-art-side"><span>small<br />things<br />matter</span></div>
            <div className="hero-art-stamp">FAIRE<br /><small>est. 2026</small></div>
          </div>
        </div>
      </section>

      <section id="catalog" className="site-container page-section pt-16 sm:pt-24">
        <div className="section-heading-row">
          <div>
            <p className="eyebrow text-smoke">目录 / {total || 0} 件上架商品</p>
            <h2 className="mt-3 font-nantes text-4xl">今天，从这里开始。</h2>
          </div>
          <div className="section-index" aria-hidden="true">01 <span>/</span> catalog</div>
        </div>

        <div className="category-pills mt-8" role="group" aria-label="商品类目">
          <button type="button" className={!categoryId ? "active" : ""} aria-pressed={!categoryId} onClick={() => chooseCategory()}>
            全部
          </button>
          {categoriesQuery.data?.map((category) => (
            <button
              key={category.id}
              type="button"
              className={category.id === categoryId ? "active" : ""}
              aria-pressed={category.id === categoryId}
              onClick={() => chooseCategory(category.id)}
            >
              {category.name}
            </button>
          ))}
        </div>

        {productsQuery.isPending ? (
          <LoadingBlock label="正在读取商品目录" />
        ) : productsQuery.isError ? (
          <ErrorState message={getApiErrorMessage(productsQuery.error)} onRetry={() => productsQuery.refetch()} />
        ) : products.length === 0 ? (
          <div className="empty-state border border-ash bg-white mt-8">
            <p className="eyebrow text-smoke">目录暂空</p>
            <h2 className="mt-3 font-nantes text-3xl">还没有匹配的商品。</h2>
            <p className="mt-2 text-sm text-smoke">换一个类目，或回到全部商品看看。</p>
            <Button variant="secondary" className="mt-6" onClick={() => chooseCategory()}>
              回到全部商品 <Icon name="arrow-right" size={16} />
            </Button>
          </div>
        ) : (
          <div className="product-grid mt-8">
            {products.map((product) => <ProductCard key={product.id} product={product} />)}
          </div>
        )}

        {totalPages > 1 && (
          <div className="pagination mt-10">
            <button type="button" disabled={page <= 1} onClick={() => changePage(page - 1)} aria-label="上一页">
              <Icon name="arrow-left" size={17} />
            </button>
            <span><strong>{page}</strong> / {totalPages}</span>
            <button type="button" disabled={page >= totalPages} onClick={() => changePage(page + 1)} aria-label="下一页">
              <Icon name="arrow-right" size={17} />
            </button>
          </div>
        )}
      </section>

      <section className="inverted-section mt-20 sm:mt-28">
        <div className="site-container inverted-section-inner">
          <p className="eyebrow text-white/70">FAIRE / 选择的理由</p>
          <h2 className="mt-5 max-w-3xl font-nantes text-4xl leading-tight text-white sm:text-5xl">
            不急着填满购物车，<br />先留下真正合适的东西。
          </h2>
          <p className="mt-6 max-w-xl text-sm leading-7 text-white/70">每件商品都从公开目录进入，库存和价格在详情页以服务端数据为准。</p>
        </div>
      </section>
    </>
  );
}
