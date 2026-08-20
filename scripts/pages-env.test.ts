import { describe, expect, test } from "bun:test";

import { validatePagesEnvironment } from "./validate-pages-env";

describe("Cloudflare Pages endpoint validation", () => {
  test("accepts the documented endpoints", () => {
    expect(() => validatePagesEnvironment(
      "https://api.example.com/api",
      "wss://api.example.com/ws",
    )).not.toThrow();
  });

  test.each([
    ["https:// /api", "wss://api.example.com/ws"],
    ["https://api.example.com\\evil/api", "wss://api.example.com/ws"],
    ["https://api.example.com\t/api", "wss://api.example.com/ws"],
    ["https://api.example.com/api?debug=1", "wss://api.example.com/ws"],
    ["https://api.example.com/api#fragment", "wss://api.example.com/ws"],
    ["https://api.example.com", "wss://api.example.com/ws"],
    ["https://api.example.com/api", "wss:// /ws"],
    ["https://api.example.com/api", "wss://api.example.com/other"],
    ["https://api.example.com/api", "wss://api.example.com/ws?token=x"],
  ])("rejects malformed endpoints", (apiBase, wsBase) => {
    expect(() => validatePagesEnvironment(apiBase, wsBase)).toThrow();
  });
});
