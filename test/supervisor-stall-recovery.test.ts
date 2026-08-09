import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { JsonRpcMessage } from "../src/protocol.js";
import { StateStore, type SupervisorState } from "../src/state-store.js";
import { Supervisor, type SupervisorOptions } from "../src/supervisor.js";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  vi.useRealTimers();
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
    request<T>(method: string, params: Record<string, unknown>, timeoutMs?: number): Promise<T>;
  };
  state: SupervisorState;
  handleClientMessage(message: JsonRpcMessage): void;
  handleServerMessage(message: JsonRpcMessage): Promise<void>;
  evaluateStall(now?: number): Promise<void>;
}

function options(): SupervisorOptions {
  return {
    cwd: process.cwd(),
    codexPath: "codex",
    codexConfig: [],
    tuiArgs: [],
    probeTimeoutMs: 30_000,
    probeSuccesses: 2,
    backoffMs: [1_000],
    maxAutoResumes: 5,
    stallTimeoutMs: 100,
    stallConfirmMs: 50,
    stallInterruptTimeoutMs: 1_000,
    maxStallResumes: 2,
    toolStallTimeoutMs: 0,
  };
}

function turnStarted(turnId: string): JsonRpcMessage {
  return {
    method: "turn/started",
    params: {
      threadId: "thread-1",
      turn: { id: turnId, status: "inProgress" },
    },
  };
}

describe("Supervisor stalled-turn recovery", () => {
  it("confirms, interrupts, and continues the same thread", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
    const root = await mkdtemp(join(tmpdir(), "codex-supervisor-stall-"));
    temporaryDirectories.push(root);
    const supervisor = new Supervisor(options(), new StateStore(root, process.cwd()));
    const harness = supervisor as unknown as SupervisorHarness;
    const requests: RecordedRequest[] = [];

    harness.proxy = {
      async request<T>(method: string, params: Record<string, unknown>): Promise<T> {
        requests.push({ method, params });
        if (method === "thread/read") {
          return {
            thread: { id: "thread-1", status: { type: "active", activeFlags: [] } },
          } as T;
        }
        if (method === "turn/interrupt") {
          await harness.handleServerMessage({
            method: "turn/completed",
            params: {
              threadId: "thread-1",
              turn: { id: "turn-1", status: "interrupted" },
            },
          });
          return {} as T;
        }
        if (method === "turn/start") {
          return { turn: { id: "turn-2", status: "inProgress" } } as T;
        }
        return {} as T;
      },
    };

    await harness.handleServerMessage(turnStarted("turn-1"));
    await harness.evaluateStall(100);
    expect(harness.state.phase).toBe("suspected-stall");
    await harness.evaluateStall(150);

    expect(requests.map((request) => request.method)).toEqual([
      "thread/read",
      "turn/interrupt",
      "thread/resume",
      "turn/start",
    ]);
    expect(requests[1]?.params).toEqual({ threadId: "thread-1", turnId: "turn-1" });
    expect(requests[3]?.params).toMatchObject({
      threadId: "thread-1",
      input: [{ type: "text", text: "continue" }],
    });
    expect(harness.state.phase).toBe("running");
    expect(harness.state.activeTurnId).toBe("turn-2");
    expect(harness.state.stallRecoveryCount).toBe(1);
  });

  it("does not resume a user-initiated interruption", async () => {
    const root = await mkdtemp(join(tmpdir(), "codex-supervisor-manual-interrupt-"));
    temporaryDirectories.push(root);
    const supervisor = new Supervisor(options(), new StateStore(root, process.cwd()));
    const harness = supervisor as unknown as SupervisorHarness;
    const requests: RecordedRequest[] = [];
    harness.proxy = {
      async request<T>(method: string, params: Record<string, unknown>): Promise<T> {
        requests.push({ method, params });
        return {} as T;
      },
    };

    await harness.handleServerMessage(turnStarted("turn-1"));
    await harness.handleServerMessage({
      method: "turn/completed",
      params: {
        threadId: "thread-1",
        turn: { id: "turn-1", status: "interrupted" },
      },
    });

    expect(requests).toHaveLength(0);
    expect(harness.state.phase).toBe("idle");
    expect(harness.state.stallRecoveryCount).toBe(0);
  });

  it("lets a user interrupt win during stall confirmation", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
    const root = await mkdtemp(join(tmpdir(), "codex-supervisor-stall-user-wins-"));
    temporaryDirectories.push(root);
    const supervisor = new Supervisor(options(), new StateStore(root, process.cwd()));
    const harness = supervisor as unknown as SupervisorHarness;
    const requests: RecordedRequest[] = [];
    harness.proxy = {
      async request<T>(method: string, params: Record<string, unknown>): Promise<T> {
        requests.push({ method, params });
        if (method === "thread/read") {
          harness.handleClientMessage({
            method: "turn/interrupt",
            params: { threadId: "thread-1", turnId: "turn-1" },
          });
          return {
            thread: { id: "thread-1", status: { type: "active", activeFlags: [] } },
          } as T;
        }
        return {} as T;
      },
    };

    await harness.handleServerMessage(turnStarted("turn-1"));
    await harness.evaluateStall(100);
    await harness.evaluateStall(150);
    await harness.handleServerMessage({
      method: "turn/completed",
      params: {
        threadId: "thread-1",
        turn: { id: "turn-1", status: "interrupted" },
      },
    });

    expect(requests.map((request) => request.method)).toEqual(["thread/read"]);
    expect(harness.state.phase).toBe("idle");
    expect(harness.state.stallRecoveryCount).toBe(0);
  });
});
