import { describe, expect, test } from "bun:test";

import { recordPollAttempt, resetPollBudget } from "../web/faire/src/lib/flashsale-poll";

describe("flash-sale lifecycle polling budget", () => {
  test("keeps attempts for the same pre-deduction", () => {
    expect(recordPollAttempt({ lifecycleID: "41", attempts: 4 }, "41")).toEqual({
      lifecycleID: "41",
      attempts: 5,
    });
  });

  test("resets attempts for a new pre-deduction", () => {
    expect(resetPollBudget({ lifecycleID: "41", attempts: 30 }, "42")).toEqual({
      lifecycleID: "42",
      attempts: 0,
    });
    expect(recordPollAttempt({ lifecycleID: "41", attempts: 30 }, "42")).toEqual({
      lifecycleID: "42",
      attempts: 1,
    });
  });
});
