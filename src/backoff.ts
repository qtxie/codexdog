export function jitteredDelay(baseMs: number, random: () => number = Math.random): number {
  const factor = 0.8 + random() * 0.4;
  return Math.max(0, Math.round(baseMs * factor));
}

export function delay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason ?? new Error("Aborted"));
      return;
    }

    const timer = setTimeout(resolve, ms);
    const abort = () => {
      clearTimeout(timer);
      reject(signal?.reason ?? new Error("Aborted"));
    };
    signal?.addEventListener("abort", abort, { once: true });
  });
}
