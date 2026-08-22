import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { ProductListItem } from "../src/api/types";
import { ProductCard } from "../src/components/ProductCard";
import { ProductVisual } from "../src/components/ui";

afterEach(cleanup);

const product: ProductListItem = {
  id: 101,
  category_id: 1,
  title: "手工釉面马克杯",
  description: "米白釉面的日常杯具",
  status: "on_sale",
  min_price: 6800,
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};

describe("product display", () => {
  test("shows editorial artwork and the starting price on catalog cards", () => {
    const view = render(
      <MemoryRouter>
        <ProductCard product={product} />
      </MemoryRouter>,
    );

    expect(screen.getByRole("link", { name: /手工釉面马克杯/ }).getAttribute("href")).toBe("/products/101");
    expect(screen.getByText(/68\.00 起/).textContent).toContain("68.00 起");
    const image = view.container.querySelector<HTMLImageElement>(".product-visual-image");
    expect(image?.getAttribute("src")).toBe("/products/faire-vessel.svg");
    expect(view.container.querySelector("[data-artwork='vessel']")).not.toBeNull();
  });

  test("keeps the monogram fallback and prioritizes the detail artwork", () => {
    const view = render(<ProductVisual seed={101} title={product.title} priority />);
    expect(view.container.querySelector(".product-visual")?.getAttribute("data-fallback")).toBe("手");
    const image = view.container.querySelector<HTMLImageElement>(".product-visual-image");
    expect(image?.loading).toBe("eager");
    expect(image?.getAttribute("fetchpriority")).toBe("high");
    fireEvent.error(image!);
    expect(view.container.querySelector(".product-visual-image")).toBeNull();
    expect(view.container.querySelector(".product-visual")?.classList.contains("is-fallback")).toBe(true);
  });

  test("makes a published product without SKU pricing explicit", () => {
    render(
      <MemoryRouter>
        <ProductCard product={{ ...product, min_price: undefined }} />
      </MemoryRouter>,
    );
    expect(screen.getByText("尚未配置价格")).not.toBeNull();
  });
});
