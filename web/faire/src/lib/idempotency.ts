import { makeClientRequestID } from "./format";

export class StableRequestIDs {
  private readonly active = new Map<string | number, string>();

  constructor(private readonly generate: () => string = makeClientRequestID) {}

  forOperation(key: string | number): string {
    const current = this.active.get(key);
    if (current) return current;
    const next = this.generate();
    this.active.set(key, next);
    return next;
  }

  complete(key: string | number) {
    this.active.delete(key);
  }
}
