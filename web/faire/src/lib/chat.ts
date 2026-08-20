import type { Message } from "../api/types";

export function latestMessageID(cached: readonly Message[], pulled: readonly Message[]): number | undefined {
  let latest = 0;
  for (const item of cached) latest = Math.max(latest, item.id);
  for (const item of pulled) latest = Math.max(latest, item.id);
  return latest || undefined;
}
