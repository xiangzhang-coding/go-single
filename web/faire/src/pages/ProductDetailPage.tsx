import { useEffect, useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { addCartItem, getProductDetail } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import { formatMoney, parseSpecs } from "../lib/format";
import { useAuthStore } from "../store/auth";
import { Button, ErrorState, Icon, LoadingBlock, ProductVisual, QuantityStepper } from "../components/ui";

export function ProductDetailPage() {
  const { id } = useParams();
  const productId = Number(id);
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const { token } = useAuthStore();
  const [selectedSkuId, setSelectedSkuId] = useState<number | null>(null);
  const [quantity, setQuantity] = useState(1);
  const [feedback, setFeedback] = useState("");
  const detailQuery = useQuery({
    queryKey: ["product", productId],
    queryFn: () => getProductDetail(productId),
    enabled: Number.isSafeInteger(productId) && productId > 0,
  });

  useEffect(() => {
    const firstSku = detailQuery.data?.skus[0];
    setSelectedSkuId(firstSku?.id ?? null);
    setQuantity(1);
  }, [detailQuery.data]);

  const selectedSku = detailQuery.data?.skus.find((sku) => sku.id === selectedSkuId);
  const addMutation = useMutation({
    mutationFn: () => addCartItem(selectedSkuId!, quantity),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["cart"] });
      setFeedback("已加入购物车");
    },
    onError: (error) => setFeedback(getApiErrorMessage(error)),
  });

  if (!Number.isSafeInteger(productId) || productId <= 0) {
    return <div className="site-container page-section"><ErrorState message="商品编号无效。" /></div>;
  }
  if (detailQuery.isPending) {
    return <div className="site-container page-section"><LoadingBlock label="正在读取商品详情" /></div>;
  }
  if (detailQuery.isError || !detailQuery.data) {
    return <div className="site-container page-section"><ErrorState message={getApiErrorMessage(detailQuery.error)} onRetry={() => detailQuery.refetch()} /></div>;
  }

  const product = detailQuery.data;
  const canAdd = Boolean(selectedSku && selectedSku.stock > 0);

  function addToCart() {
    if (!token) {
      navigate(`/login?returnTo=${encodeURIComponent(`${location.pathname}${location.search}`)}`);
      return;
    }
    setFeedback("");
    addMutation.mutate();
  }

  function buyNow() {
    if (!token) {
      navigate(`/login?returnTo=${encodeURIComponent(`${location.pathname}${location.search}`)}`);
      return;
    }
    if (!selectedSku) return;
    const params = new URLSearchParams({
      product_id: String(productId),
      sku_id: String(selectedSku.id),
      quantity: String(quantity),
    });
    navigate(`/checkout?${params.toString()}`);
  }

  return (
    <section className="site-container page-section pt-8 sm:pt-14">
      <Link to="/" className="back-link"><Icon name="arrow-left" size={16} /> 返回目录</Link>
      <div className="product-detail-grid mt-8 sm:mt-12">
        <div>
          <ProductVisual seed={product.id} title={product.title} />
          <div className="product-detail-caption mt-4">
            <span>FAIRE / OBJECT {String(product.id).padStart(3, "0")}</span>
            <span>公开目录</span>
          </div>
        </div>

        <div className="product-detail-copy">
          <p className="eyebrow text-smoke">商品详情 / {product.status === "on_sale" ? "在售" : "暂不可售"}</p>
          <h1 className="mt-4 font-nantes text-5xl leading-[1.08]">{product.title}</h1>
          <p className="mt-6 max-w-xl text-base leading-7 text-charcoal">{product.description || "一件适合被认真使用的日常商品。"}</p>

          <div className="detail-divider my-8" />

          <div>
            <div className="flex items-baseline justify-between gap-4">
              <h2 className="text-sm uppercase tracking-[0.12em] text-smoke">选择规格</h2>
              {selectedSku && <span className="text-sm text-smoke">库存 {selectedSku.stock}</span>}
            </div>
            <div className="sku-list mt-4">
              {product.skus.map((sku) => (
                <button
                  type="button"
                  key={sku.id}
                  disabled={sku.stock < 1}
                  className={`sku-option ${sku.id === selectedSkuId ? "selected" : ""}`}
                  onClick={() => { setSelectedSkuId(sku.id); setQuantity(1); setFeedback(""); }}
                >
                  <span>
                    {parseSpecs(sku.specs).map(([key, value]) => <span key={key} className="block">{key} / {value}</span>)}
                    {parseSpecs(sku.specs).length === 0 && <span>标准规格</span>}
                  </span>
                  <strong>{formatMoney(sku.price)}</strong>
                </button>
              ))}
            </div>
          </div>

          <div className="detail-buy-row mt-8">
            <QuantityStepper value={quantity} max={Math.min(99, selectedSku?.stock || 1)} onChange={setQuantity} disabled={!canAdd} />
            <div className="detail-buy-actions">
              <Button variant="secondary" className="flex-1 justify-center" onClick={addToCart} disabled={!canAdd || addMutation.isPending}>
                {addMutation.isPending ? "正在加入" : "加入购物车"}
                {!addMutation.isPending && <Icon name="bag" size={17} />}
              </Button>
              <Button className="flex-1 justify-center" onClick={buyNow} disabled={!canAdd}>
                立即购买 <Icon name="arrow-right" size={17} />
              </Button>
            </div>
          </div>
          {feedback && <div className={`notice mt-4 ${feedback === "已加入购物车" ? "notice-success" : "notice-error"}`}>{feedback}</div>}

          <dl className="detail-facts mt-10">
            <div><dt>商品编号</dt><dd>{product.id}</dd></div>
            <div><dt>SKU 数量</dt><dd>{product.skus.length} 个规格</dd></div>
            <div><dt>配送说明</dt><dd>下单后以订单地址快照为准</dd></div>
          </dl>
        </div>
      </div>
    </section>
  );
}
