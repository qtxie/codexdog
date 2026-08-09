import { describe, expect, it } from "vitest";
import type { JsonRpcMessage } from "../src/protocol.js";
import { StallWatchdog } from "../src/stall-watchdog.js";

const options = {
  stallTimeoutMs: 100,
  stallConfirmMs: 50,
  toolStallTimeoutMs: 0,
};

function event(method: string, params: Record<string, unknown> = {}): JsonRpcMessage {
  return {
    method,
    params: { threadId: "thread-1", turnId: "turn-1", ...params },
  };
}

describe("StallWatchdog", () => {
  it("requires both the idle timeout and confirmation window", () => {
    const watchdog = new StallWatchdog(options);
    watchdog.startTurn("thread-1", "turn-1", 0);

    expect(watchdog.evaluate(99)).toBeUndefined();
    expect(watchdog.evaluate(100)).toMatchObject({
      kind: "suspected",
      confirmAt: 150,
    });
    expect(watchdog.evaluate(149)).toBeUndefined();
    expect(watchdog.evaluate(150)).toMatchObject({ kind: "confirmed" });
  });

  it("cancels a suspicion when matching turn activity arrives", () => {
    const watchdog = new StallWatchdog(options);
    watchdog.startTurn("thread-1", "turn-1", 0);
    expect(watchdog.evaluate(100)?.kind).toBe("suspected");

    expect(
      watchdog.observeServerMessage(event("item/reasoning/summaryTextDelta"), 120),
    ).toEqual({ activity: true, suspicionCleared: true });
    expect(watchdog.evaluate(219)).toBeUndefined();
    expect(watchdog.evaluate(220)?.kind).toBe("suspected");
  });

  it("pauses while waiting for a user or while an active tool is quiet", () => {
    const watchdog = new StallWatchdog(options);
    watchdog.startTurn("thread-1", "turn-1", 0);
    watchdog.observeServerMessage(
      event("thread/status/changed", {
        status: { type: "active", activeFlags: ["waitingOnApproval"] },
      }),
      10,
    );
    expect(watchdog.evaluate(1_000)).toBeUndefined();
    expect(watchdog.snapshot().pauseReason).toBe("waitingOnApproval");

    watchdog.observeServerMessage(
      event("thread/status/changed", {
        status: { type: "active", activeFlags: [] },
      }),
      1_000,
    );
    watchdog.observeServerMessage(
      event("item/started", {
        item: { id: "command-1", type: "commandExecution" },
      }),
      1_010,
    );
    expect(watchdog.evaluate(10_000)).toBeUndefined();
    expect(watchdog.snapshot().pauseReason).toBe("activeTool:commandExecution");

    watchdog.observeServerMessage(
      event("item/completed", {
        item: { id: "command-1", type: "commandExecution" },
      }),
      10_000,
    );
    expect(watchdog.evaluate(10_099)).toBeUndefined();
    expect(watchdog.evaluate(10_100)?.kind).toBe("suspected");
  });

  it("invalidates a confirmed stall when activity wins the confirmation race", () => {
    const watchdog = new StallWatchdog(options);
    watchdog.startTurn("thread-1", "turn-1", 0);
    watchdog.evaluate(100);
    const decision = watchdog.evaluate(150);
    expect(decision?.kind).toBe("confirmed");
    if (!decision || decision.kind !== "confirmed") {
      throw new Error("Expected a confirmed stall");
    }

    watchdog.observeServerMessage(event("item/agentMessage/delta"), 151);
    expect(watchdog.isCurrent(decision.context)).toBe(false);
  });

  it("ignores activity belonging to another turn", () => {
    const watchdog = new StallWatchdog(options);
    watchdog.startTurn("thread-1", "turn-1", 0);
    expect(
      watchdog.observeServerMessage(
        {
          method: "item/agentMessage/delta",
          params: { threadId: "thread-1", turnId: "turn-2" },
        },
        90,
      ).activity,
    ).toBe(false);
    expect(watchdog.evaluate(100)?.kind).toBe("suspected");
  });
});
