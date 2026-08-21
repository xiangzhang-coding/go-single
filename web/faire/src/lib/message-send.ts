import type { MessageType, SendMessageRequest } from "../api/types";
import { makeClientRequestID } from "./format";

interface MessageSendOperationBase {
  clientRequestId: string;
}

export interface TextSendOperation extends MessageSendOperationBase {
  type: "text";
  content: string;
}

export interface MediaSendOperation extends MessageSendOperationBase {
  type: Exclude<MessageType, "text">;
  file: File;
  uploadedReference?: string;
}

export type MessageSendOperation = TextSendOperation | MediaSendOperation;

export function createTextSendOperation(
  content: string,
  generate: () => string = makeClientRequestID,
): TextSendOperation {
  return { type: "text", content, clientRequestId: generate() };
}

export function createMediaSendOperation(
  type: MediaSendOperation["type"],
  file: File,
  generate: () => string = makeClientRequestID,
): MediaSendOperation {
  return { type, file, clientRequestId: generate() };
}

export async function executeMessageSend<T>(
  operation: MessageSendOperation,
  peerUserId: number,
  upload: (file: File, kind: MediaSendOperation["type"]) => Promise<{ url: string }>,
  send: (request: SendMessageRequest) => Promise<T>,
  onPrepared: (operation: MessageSendOperation) => void,
): Promise<{ result: T; operation: MessageSendOperation }> {
  let prepared = operation;
  if (operation.type !== "text" && !operation.uploadedReference) {
    const uploaded = await upload(operation.file, operation.type);
    prepared = { ...operation, uploadedReference: uploaded.url };
    onPrepared(prepared);
  }

  const request: SendMessageRequest = prepared.type === "text"
    ? {
        to_user_id: peerUserId,
        type: "text",
        content: prepared.content,
        client_request_id: prepared.clientRequestId,
      }
    : {
        to_user_id: peerUserId,
        type: prepared.type,
        url: prepared.uploadedReference,
        client_request_id: prepared.clientRequestId,
      };
  const result = await send(request);
  return { result, operation: prepared };
}
