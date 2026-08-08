import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import type { JsonRpcMessage } from "../src/protocol.js";
import { StateStore, type SupervisorState } from "../src/state-store.js";
import { Supervisor, type SupervisorOptions } from "../src/supervisor.js";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporaryDirectories.splice(0).map((path) => rm(path, { recursive: true, force: true })),
  );
});

interface RecordedRequest {
  method: string;
  params: Record<string, unknown>;
}

interface SupervisorHarness {
  proxy: {
    request<T>(method: string, params: Record<string, unknown>): Promise<T>;
  };
  state: SupervisorState;
  handleServerMessage(message: JsonRpcMessage): Promise<void>;
}

function cyberPolicyFailure(threadId: string, turnId: string): JsonRpcMessage {
  return {
    method: "turn/completed",
    params: {
      threadId,
      turn: {
        id: turnId,
        status: "failed",
        error: {
          message: "Request blocked by cyber safety policy",
          codexErrorInfo: "cyberPolicy",
        },
      },
    },
  };
}

describe("Supervisor cyber policy recovery", () => {
  it("retries twice, forks once, then requires attention", async () => {
    const root = await mkdtemp(join(tmpdir(), "codex-supervisor-cyber-policy-"));
    temporaryDirectories.push(root);
    const store = new StateStore(root, process.cwd());
    const options: SupervisorOptions = {
      cwd: process.cwd(),
      codexPath: "codex",
      codexConfig: [],
      tuiArgs: [],
      probeTimeoutMs: 30_000,
      probeSuccesses: 2,
      backoffMs: [1_000],
      maxAutoResumes: 5,
    };
    const supervisor = new Supervisor(options, store);
    const harness = supervisor as unknown as SupervisorHarness;
    const requests: RecordedRequest[] = [];
    let turnNumber = 0;
    harness.proxy = {
      async request<T>(method: string, params: Record<string, unknown>): Promise<T> {
        requests.push({ method, params });
        if (method === "thread/fork") {
          return { thread: { id: "fork-1" } } as T;
        }
        if (method === "turn/start") {
          turnNumber += 1;
          return {
            turn: { id: `recovery-${turnNumber}`, status: "inProgress" },
          } as T;
        }
        return {} as T;
      },
    };

    await harness.handleServerMessage(cyberPolicyFailure("thread-1", "original"));
    await harness.handleServerMessage(cyberPolicyFailure("thread-1", "recovery-1"));
    await harness.handleServerMessage(cyberPolicyFailure("thread-1", "recovery-2"));

    expect(requests.map((request) => request.method)).toEqual([
      "thread/resume",
      "turn/start",
      "thread/resume",
      "turn/start",
      "thread/fork",
      "turn/start",
    ]);
    expect(requests.filter((request) => request.method === "turn/start")).toMatchObject([
      { params: { threadId: "thread-1", input: [{ type: "text", text: "continue" }] } },
      { params: { threadId: "thread-1", input: [{ type: "text", text: "继续" }] } },
      { params: { threadId: "fork-1", input: [{ type: "text", text: "continue" }] } },
    ]);
    expect(requests[4]?.params).toMatchObject({
      threadId: "thread-1",
      lastTurnId: "recovery-2",
    });
    expect(harness.state.currentThreadId).toBe("fork-1");

    await harness.handleServerMessage(cyberPolicyFailure("fork-1", "recovery-3"));

    expect(requests).toHaveLength(6);
    expect(harness.state.phase).toBe("needs-attention");
    expect(harness.state.lastError).toContain("Cyber policy recovery exhausted after 3 attempts");
  });
});
