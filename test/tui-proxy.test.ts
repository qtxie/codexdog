import WebSocket, { WebSocketServer } from "ws";
import { afterEach, describe, expect, it } from "vitest";
import { TuiProxy } from "../src/tui-proxy.js";
import type { JsonRpcMessage } from "../src/protocol.js";

const closeTasks: Array<() => Promise<void>> = [];

afterEach(async () => {
  while (closeTasks.length > 0) {
    await closeTasks.pop()?.();
  }
});

describe("TuiProxy", () => {
  it("forwards frames and observes app-server messages", async () => {
    const upstream = new WebSocketServer({ host: "127.0.0.1", port: 0 });
    await new Promise<void>((resolve) => upstream.once("listening", resolve));
    const address = upstream.address();
    if (!address || typeof address === "string") {
      throw new Error("No upstream port");
    }
    upstream.on("connection", (socket) => {
      socket.on("message", (data) => {
        const request = JSON.parse(data.toString()) as JsonRpcMessage;
        socket.send(JSON.stringify({ id: request.id, result: { ok: true } }));
        socket.send(
          JSON.stringify({
            method: "turn/started",
            params: { threadId: "thread-1", turn: { id: "turn-1", items: [], status: "inProgress" } },
          }),
        );
      });
    });
    closeTasks.push(
      () => new Promise<void>((resolve) => upstream.close(() => resolve())),
    );

    const proxy = new TuiProxy(`ws://127.0.0.1:${address.port}`);
    const proxyPort = await proxy.start();
    closeTasks.push(() => proxy.close());
    const observed: JsonRpcMessage[] = [];
    proxy.onServerMessage((message) => observed.push(message));

    const client = new WebSocket(`ws://127.0.0.1:${proxyPort}`);
    await new Promise<void>((resolve, reject) => {
      client.once("open", resolve);
      client.once("error", reject);
    });
    closeTasks.push(
      () =>
        new Promise<void>((resolve) => {
          if (client.readyState === WebSocket.CLOSED) {
            resolve();
          } else {
            client.once("close", () => resolve());
            client.close();
          }
        }),
    );

    const turnStarted = new Promise<void>((resolve) => {
      proxy.onServerMessage((message) => {
        if (message.method === "turn/started") {
          resolve();
        }
      });
    });
    client.send(JSON.stringify({ id: 1, method: "initialize", params: {} }));
    await turnStarted;

    expect(observed.some((message) => message.method === "turn/started")).toBe(true);
    await expect(
      proxy.request("turn/start", {
        threadId: "thread-1",
        input: [{ type: "text", text: "continue" }],
      }),
    ).resolves.toEqual({ ok: true });
  });
});
