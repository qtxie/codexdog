import { describe, expect, it } from "vitest";
import { nextCyberPolicyRecoveryAction } from "../src/cyber-policy-recovery.js";

describe("nextCyberPolicyRecoveryAction", () => {
  it("retries in English, retries in Chinese, then forks", () => {
    expect(nextCyberPolicyRecoveryAction(0)).toEqual({
      kind: "retry-thread",
      prompt: "continue",
    });
    expect(nextCyberPolicyRecoveryAction(1)).toEqual({
      kind: "retry-thread",
      prompt: "继续",
    });
    expect(nextCyberPolicyRecoveryAction(2)).toEqual({
      kind: "fork-thread",
      prompt: "continue",
    });
    expect(nextCyberPolicyRecoveryAction(3)).toBeUndefined();
  });
});
