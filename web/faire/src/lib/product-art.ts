export type ProductArtworkKey = "vessel" | "textile" | "tray" | "desk" | "carry" | "glass";

export interface ProductArtwork {
  key: ProductArtworkKey;
  src: string;
  caption: string;
}

const artwork: ProductArtwork[] = [
  { key: "vessel", src: "/products/faire-vessel.svg", caption: "Vessel study" },
  { key: "textile", src: "/products/faire-textile.svg", caption: "Textile study" },
  { key: "tray", src: "/products/faire-tray.svg", caption: "Timber study" },
  { key: "desk", src: "/products/faire-desk.svg", caption: "Brass study" },
  { key: "carry", src: "/products/faire-carry.svg", caption: "Carry study" },
  { key: "glass", src: "/products/faire-glass.svg", caption: "Glass study" },
];

const titleRules: Array<{ pattern: RegExp; key: ProductArtworkKey }> = [
  { pattern: /杯|马克|茶具|咖啡|mug|cup|vessel/i, key: "vessel" },
  { pattern: /袋|包|篮|bag|tote|basket/i, key: "carry" },
  { pattern: /布|毯|巾|亚麻|linen|textile|fabric/i, key: "textile" },
  { pattern: /托盘|木盘|餐盘|tray|plate|board/i, key: "tray" },
  { pattern: /黄铜|夹|笔|剪|brass|clip|stationery/i, key: "desk" },
  { pattern: /花瓶|玻璃|香薰|瓶|vase|glass|bottle/i, key: "glass" },
];

export function resolveProductArtwork(productID: number, title = ""): ProductArtwork {
  const matched = titleRules.find(({ pattern }) => pattern.test(title));
  if (matched) {
    return artwork.find(({ key }) => key === matched.key)!;
  }
  const index = Math.abs(Math.trunc(productID || 0)) % artwork.length;
  return artwork[index]!;
}
