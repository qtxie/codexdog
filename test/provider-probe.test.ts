import { describe, expect, it } from "vitest";
import {
  ProviderProbe,
  type RpcClientLike,
} from "../src/provider-probe.js";
import type { JsonRpcMessage } from "../src/protocol.js";

class MockRpc implements RpcClientLike {
  private handler?: (message: JsonRpcMessage) => void;
  status: "completed" | "failed" = "completed";
  turnStarts = 0;

  onNotification(handler: (message: JsonRpcMessage) => void): () => void {
    this.handler = handler;
    return () => {
      this.handler = undefined;
    };
  }

  async request<T = unknown>(method: string, params: Record<string, unknown>): Promise<T> {
    if (method === "thread/start") {
      return { thread: { id: "health-thread" } } as T;
    }
    if (method === "turn/start") {
      this.turnStarts += 1;
      const turnId = `health-turn-${this.turnStarts}`;
      queueMicrotask(() => {
        this.handler?.({
          method: "turn/completed",
          params: {
            threadId: "health-thread",
            turn: {
              id: turnId,
              items: [],
              status: this.status,
              ...(this.status === "failed"
                ? {
                    error: {
                      message: "provider unavailable",
                      codexErrorInfo: {
                        httpConnectionFailed: { httpStatusCode: 503 },
                      },
                    },
                  }
                : {}),
            },
          },
        });
      });
      return { turn: { id: turnId, items: [], status: "inProgress" } } as T;
    }
    return {} as T;
  }
}

describe("ProviderProbe", () => {
  it("uses an ephemeral thread and recognizes a completed canary", async () => {
    const rpc = new MockRpc();
    const probe = new ProviderProbe(rpc, { cwd: process.cwd(), timeoutMs: 1_000 });

    await expect(probe.check()).resolves.toEqual({ healthy: true });
    probe.dispose();
  });

  it("returns a classified transient failure", async () => {
    const rpc = new MockRpc();
    rpc.status = "failed";
    const probe = new ProviderProbe(rpc, { cwd: process.cwd(), timeoutMs: 1_000 });

    const result = await probe.check();
    expect(result.healthy).toBe(false);
    expect(result.failure).toMatchObject({ disposition: "transient", httpStatus: 503 });
    probe.dispose();
  });

  it("reuses its ephemeral health thread", async () => {
    const rpc = new MockRpc();
    let threadStarts = 0;
    const request = rpc.request.bind(rpc);
    rpc.request = async <T>(method: string, params: Record<string, unknown>): Promise<T> => {
      if (method === "thread/start") {
        threadStarts += 1;
      }
      return request<T>(method, params);
    };
    const probe = new ProviderProbe(rpc, { cwd: process.cwd(), timeoutMs: 1_000 });

    await probe.check();
    await probe.check();
    expect(threadStarts).toBe(1);
    probe.dispose();
  });
});
