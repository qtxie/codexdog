import { describe, expect, it } from "vitest";
import { jitteredDelay } from "../src/backoff.js";

describe("jitteredDelay", () => {
  it("applies bounded twenty-percent jitter", () => {
    expect(jitteredDelay(1_000, () => 0)).toBe(800);
    expect(jitteredDelay(1_000, () => 0.5)).toBe(1_000);
    expect(jitteredDelay(1_000, () => 1)).toBe(1_200);
  });
});
