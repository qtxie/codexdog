import WebSocket, { type RawData } from "ws";
import { parseJsonRpc, type JsonRpcId, type JsonRpcMessage } from "./protocol.js";

interface PendingRequest {
  resolve: (result: unknown) => void;
  reject: (error: Error) => void;
  timer: NodeJS.Timeout;
}

export type NotificationHandler = (message: JsonRpcMessage) => void;

export class JsonRpcClient {
  private socket?: WebSocket;
  private nextId = 1;
  private readonly pending = new Map<JsonRpcId, PendingRequest>();
  private readonly handlers = new Set<NotificationHandler>();

  constructor(
    private readonly url: string,
    private readonly requestTimeoutMs = 15_000,
  ) {}

  async connect(): Promise<void> {
    if (this.socket?.readyState === WebSocket.OPEN) {
      return;
    }

    const socket = new WebSocket(this.url, { perMessageDeflate: false });
    this.socket = socket;

    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`Timed out connecting to ${this.url}`)), 10_000);
      socket.once("open", () => {
        clearTimeout(timer);
        resolve();
      });
      socket.once("error", (error) => {
        clearTimeout(timer);
        reject(error);
      });
    });

    socket.on("message", (data) => this.handleMessage(data));
    socket.on("close", () => this.rejectPending(new Error("Codex app-server connection closed")));
    socket.on("error", (error) => this.rejectPending(error));
  }

  async initialize(): Promise<void> {
    await this.request("initialize", {
      clientInfo: {
        name: "codex_provider_supervisor",
        title: "Codex Provider Supervisor",
        version: "0.1.0",
      },
      capabilities: { experimentalApi: false },
    });
    this.notify("initialized", {});
  }

  request<T = unknown>(method: string, params: Record<string, unknown>): Promise<T> {
    const socket = this.requireOpenSocket();
    const id = `supervisor-${this.nextId++}`;

    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`JSON-RPC request timed out: ${method}`));
      }, this.requestTimeoutMs);

      this.pending.set(id, {
        resolve: (value) => resolve(value as T),
        reject,
        timer,
      });
      socket.send(JSON.stringify({ id, method, params }));
    });
  }

  notify(method: string, params: Record<string, unknown>): void {
    this.requireOpenSocket().send(JSON.stringify({ method, params }));
  }

  onNotification(handler: NotificationHandler): () => void {
    this.handlers.add(handler);
    return () => this.handlers.delete(handler);
  }

  close(): void {
    this.socket?.close();
    this.rejectPending(new Error("JSON-RPC client closed"));
  }

  private handleMessage(data: RawData): void {
    const message = parseJsonRpc(data.toString());
    if (!message) {
      return;
    }

    if (message.id !== undefined && this.pending.has(message.id)) {
      const pending = this.pending.get(message.id);
      this.pending.delete(message.id);
      if (!pending) {
        return;
      }
      clearTimeout(pending.timer);
      if (message.error !== undefined) {
        pending.reject(new Error(`JSON-RPC error: ${JSON.stringify(message.error)}`));
      } else {
        pending.resolve(message.result);
      }
      return;
    }

    if (message.method) {
      for (const handler of this.handlers) {
        handler(message);
      }
    }
  }

  private requireOpenSocket(): WebSocket {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) {
      throw new Error("JSON-RPC client is not connected");
    }
    return this.socket;
  }

  private rejectPending(error: Error): void {
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(error);
    }
    this.pending.clear();
  }
}
