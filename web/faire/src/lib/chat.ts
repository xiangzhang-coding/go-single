import type { ConversationView, Message } from "../api/types";

export function latestMessageID(cached: readonly Message[], pulled: readonly Message[]): number | undefined {
  let latest = 0;
  for (const item of cached) latest = Math.max(latest, item.id);
  for (const item of pulled) latest = Math.max(latest, item.id);
  return latest || undefined;
}

export function conversationBeforeID(conversations: readonly ConversationView[]): number | undefined {
  for (let index = conversations.length - 1; index >= 0; index -= 1) {
    const id = conversations[index]?.last_message?.id;
    if (id) return id;
  }
  return undefined;
}
