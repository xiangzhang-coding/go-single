# Faire Theme

The Faire demo is a React + TypeScript + Vite + Tailwind v4 frontend for the product, cart, checkout, payment and order lifecycle APIs.

## Run locally

```bash
bun install
bun run dev
```

Start the Go server on port `8080` first. The Vite dev proxy forwards `/api` and `/ws` to that server. `VITE_API_BASE` can be set to an absolute API URL when deploying the SPA separately.

## Main flow

1. Browse the public catalog and filter by category.
2. Open a product, choose a SKU and add it to the cart.
3. Sign in, add/select an address, optionally select an eligible coupon, and submit the cart.
4. Pay from the order detail page through the mock payment endpoint.
5. The admin seed account can simulate shipping; the recipient can confirm receipt.
