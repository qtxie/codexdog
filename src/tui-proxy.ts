import { randomUUID } from "node:crypto";
import WebSocket, { WebSocketServer, type RawData } from "ws";
import { parseJsonRpc, type JsonRpcId, type JsonRpcMessage } from "./protocol.js";

export type ProxyObserver = (message: JsonRpcMessage, connectionId: string) => void;

interface QueuedFrame {
  data: RawData;
  binary: boolean;
}

interface Bridge {
  downstream: WebSocket;
  upstream: WebSocket;
}

interface InjectedRequest {
  connectionId: string;
  resolve: (result: unknown) => void;
  reject: (error: Error) => void;
  timer: NodeJS.Timeout;
}

export class TuiProxy {
  private server?: WebSocketServer;
  private readonly downstreamSockets = new Set<WebSocket>();
  private readonly bridges = new Map<string, Bridge>();
  private readonly injectedRequests = new Map<JsonRpcId, InjectedRequest>();
  private readonly serverObservers = new Set<ProxyObserver>();
  private readonly clientObservers = new Set<ProxyObserver>();
  private nextInjectedId = 1;
  private lastActiveConnectionId: string | undefined;
  port?: number;

  constructor(private readonly upstreamUrl: string) {}

  async start(): Promise<number> {
    const server = new WebSocketServer({
      host: "127.0.0.1",
      port: 0,
      perMessageDeflate: false,
    });
    this.server = server;

    server.on("connection", (downstream) => this.bridge(downstream));
    await new Promise<void>((resolve, reject) => {
      server.once("listening", resolve);
      server.once("error", reject);
    });

    const address = server.address();
    if (!address || typeof address === "string") {
      throw new Error("Could not determine TUI proxy port");
    }
    this.port = address.port;
    return address.port;
  }

  onServerMessage(observer: ProxyObserver): () => void {
    this.serverObservers.add(observer);
    return () => this.serverObservers.delete(observer);
  }

  onClientMessage(observer: ProxyObserver): () => void {
    this.clientObservers.add(observer);
    return () => this.clientObservers.delete(observer);
  }

  request<T = unknown>(
    method: string,
    params: Record<string, unknown>,
    timeoutMs = 30_000,
  ): Promise<T> {
    const bridge = this.activeBridge();
    if (!bridge) {
      return Promise.reject(new Error("No active Codex TUI connection is available"));
    }
    const id = `codex-supervisor-proxy-${this.nextInjectedId++}`;
    const connectionId = this.lastActiveConnectionId!;

    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.injectedRequests.delete(id);
        reject(new Error(`TUI JSON-RPC request timed out: ${method}`));
      }, timeoutMs);
      this.injectedRequests.set(id, {
        connectionId,
        resolve: (result) => resolve(result as T),
        reject,
        timer,
      });
      bridge.upstream.send(JSON.stringify({ id, method, params }));
    });
  }

  async close(): Promise<void> {
    this.rejectInjectedRequests(new Error("TUI proxy closed"));
    for (const socket of this.downstreamSockets) {
      socket.close();
    }
    if (!this.server) {
      return;
    }
    await new Promise<void>((resolve) => this.server?.close(() => resolve()));
  }

  private bridge(downstream: WebSocket): void {
    const connectionId = randomUUID();
    const upstream = new WebSocket(this.upstreamUrl, { perMessageDeflate: false });
    const queue: QueuedFrame[] = [];
    this.downstreamSockets.add(downstream);
    this.bridges.set(connectionId, { downstream, upstream });
    this.lastActiveConnectionId = connectionId;

    upstream.on("open", () => {
      for (const frame of queue) {
        upstream.send(frame.data, { binary: frame.binary });
      }
      queue.length = 0;
    });

    downstream.on("message", (data, binary) => {
      this.lastActiveConnectionId = connectionId;
      this.observe(data, connectionId, this.clientObservers);
      if (upstream.readyState === WebSocket.OPEN) {
        upstream.send(data, { binary });
      } else if (upstream.readyState === WebSocket.CONNECTING) {
        queue.push({ data, binary });
      }
    });

    upstream.on("message", (data, binary) => {
      const message = parseJsonRpc(data.toString());
      if (message?.id !== undefined && this.injectedRequests.has(message.id)) {
        const pending = this.injectedRequests.get(message.id);
        this.injectedRequests.delete(message.id);
        if (pending) {
          clearTimeout(pending.timer);
          if (message.error !== undefined) {
            pending.reject(new Error(`JSON-RPC error: ${JSON.stringify(message.error)}`));
          } else {
            pending.resolve(message.result);
          }
        }
        return;
      }
      this.observe(data, connectionId, this.serverObservers);
      if (downstream.readyState === WebSocket.OPEN) {
        downstream.send(data, { binary });
      }
    });

    downstream.on("close", (code, reason) => {
      this.downstreamSockets.delete(downstream);
      this.bridges.delete(connectionId);
      this.rejectInjectedRequests(
        new Error("Codex TUI connection closed"),
        connectionId,
      );
      if (this.lastActiveConnectionId === connectionId) {
        this.lastActiveConnectionId = this.bridges.keys().next().value as string | undefined;
      }
      if (upstream.readyState === WebSocket.OPEN || upstream.readyState === WebSocket.CONNECTING) {
        closeSocket(upstream, code, reason);
      }
    });
    upstream.on("close", (code, reason) => {
      if (downstream.readyState === WebSocket.OPEN || downstream.readyState === WebSocket.CONNECTING) {
        closeSocket(downstream, code, reason);
      }
    });
    downstream.on("error", () => upstream.close());
    upstream.on("error", () => downstream.close());
  }

  private observe(data: RawData, connectionId: string, observers: Set<ProxyObserver>): void {
    const message = parseJsonRpc(data.toString());
    if (!message) {
      return;
    }
    for (const observer of observers) {
      observer(message, connectionId);
    }
  }

  private activeBridge(): Bridge | undefined {
    if (this.lastActiveConnectionId) {
      const active = this.bridges.get(this.lastActiveConnectionId);
      if (active?.upstream.readyState === WebSocket.OPEN) {
        return active;
      }
    }
    for (const [connectionId, bridge] of this.bridges) {
      if (bridge.upstream.readyState === WebSocket.OPEN) {
        this.lastActiveConnectionId = connectionId;
        return bridge;
      }
    }
    return undefined;
  }

  private rejectInjectedRequests(error: Error, connectionId?: string): void {
    for (const [id, pending] of this.injectedRequests) {
      if (connectionId && pending.connectionId !== connectionId) {
        continue;
      }
      clearTimeout(pending.timer);
      pending.reject(error);
      this.injectedRequests.delete(id);
    }
  }
}

function closeSocket(socket: WebSocket, code: number, reason: Buffer): void {
  if (code >= 1_000 && code <= 4_999 && code !== 1_004 && code !== 1_005 && code !== 1_006) {
    socket.close(code, reason);
  } else {
    socket.close();
  }
}
