let controller = new AbortController();

export function currentSessionRequestSignal(): AbortSignal {
  return controller.signal;
}

export function beginSessionRequestGeneration() {
  controller.abort();
  controller = new AbortController();
}
