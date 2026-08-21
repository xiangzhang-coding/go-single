import { describe, expect, test } from "bun:test";

import type { SendMessageRequest } from "../src/api/types";
import { StableRequestIDs } from "../src/lib/idempotency";
import {
  createMediaSendOperation,
  createTextSendOperation,
  executeMessageSend,
  type MessageSendOperation,
} from "../src/lib/message-send";

describe("stable client request ids", () => {
  test("retries reuse an operation id until it completes", () => {
    let sequence = 0;
    const ids = new StableRequestIDs(() => `request-${++sequence}`);

    expect(ids.forOperation("flashsale:7")).toBe("request-1");
    expect(ids.forOperation("flashsale:7")).toBe("request-1");

    ids.complete("flashsale:7");
    expect(ids.forOperation("flashsale:7")).toBe("request-2");
  });

  test("a failed media send keeps its id and uploaded reference for retry", async () => {
    const file = new File(["image"], "photo.png", { type: "image/png" });
    const initial = createMediaSendOperation("image", file, () => "message-1");
    let pending: MessageSendOperation = initial;
    let uploads = 0;
    const requests: SendMessageRequest[] = [];

    const upload = async () => {
      uploads += 1;
      return { url: "/files/reference-1" };
    };
    const failSend = async (request: SendMessageRequest) => {
      requests.push(request);
      throw new Error("unknown network result");
    };

    await expect(executeMessageSend(pending, 2, upload, failSend, (prepared) => {
      pending = prepared;
    })).rejects.toThrow("unknown network result");
    await expect(executeMessageSend(pending, 2, upload, failSend, (prepared) => {
      pending = prepared;
    })).rejects.toThrow("unknown network result");

    expect(uploads).toBe(1);
    expect(requests).toEqual([
      { to_user_id: 2, type: "image", url: "/files/reference-1", client_request_id: "message-1" },
      { to_user_id: 2, type: "image", url: "/files/reference-1", client_request_id: "message-1" },
    ]);
  });

  test("an upload failure retries the same media operation id", async () => {
    const file = new File(["image"], "photo.png", { type: "image/png" });
    const pending = createMediaSendOperation("image", file, () => "message-1");
    let uploads = 0;
    const upload = async () => {
      uploads += 1;
      if (uploads === 1) throw new Error("upload failed");
      return { url: "/files/reference-1" };
    };
    const requests: SendMessageRequest[] = [];

    await expect(executeMessageSend(pending, 2, upload, async (request) => {
      requests.push(request);
      return "sent";
    }, () => undefined)).rejects.toThrow("upload failed");
    await executeMessageSend(pending, 2, upload, async (request) => {
      requests.push(request);
      return "sent";
    }, () => undefined);

    expect(requests).toEqual([
      { to_user_id: 2, type: "image", url: "/files/reference-1", client_request_id: "message-1" },
    ]);
  });

  test("a successful send allows the next operation to get a new id", () => {
    let sequence = 0;
    const makeID = () => `message-${++sequence}`;

    expect(createTextSendOperation("first", makeID).clientRequestId).toBe("message-1");
    expect(createTextSendOperation("second", makeID).clientRequestId).toBe("message-2");
  });
});
