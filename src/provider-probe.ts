import { classifyFailure, type ClassifiedFailure } from "./failure-classifier.js";
import { isRecord, readString, readTurn, readTurnError, type JsonRpcMessage } from "./protocol.js";

export interface ProbeResult {
  healthy: boolean;
  failure?: ClassifiedFailure;
  retryAfterMs?: number;
}

interface TurnCompletion {
  status: "completed" | "failed" | "interrupted";
  error?: ReturnType<typeof readTurnError>;
}

interface ProviderProbeOptions {
  cwd: string;
  timeoutMs: number;
  healthUrl?: string;
  model?: string;
}

export interface RpcClientLike {
  request<T = unknown>(method: string, params: Record<string, unknown>): Promise<T>;
  onNotification(handler: (message: JsonRpcMessage) => void): () => void;
}

export class ProviderProbe {
  private healthThreadId?: string;
  private readonly completions = new Map<string, TurnCompletion>();
  private readonly waiters = new Map<string, (completion: TurnCompletion) => void>();
  private readonly unsubscribe: () => void;

  constructor(
    private readonly rpc: RpcClientLike,
    private readonly options: ProviderProbeOptions,
  ) {
    this.unsubscribe = rpc.onNotification((message) => this.handleNotification(message));
  }

  async check(): Promise<ProbeResult> {
    if (this.options.healthUrl) {
      const endpointResult = await this.checkHealthEndpoint();
      if (!endpointResult.healthy) {
        return endpointResult;
      }
    }

    let turnId: string | undefined;
    try {
      const threadId = await this.ensureHealthThread();
      const response = await this.rpc.request<{ turn?: unknown }>("turn/start", {
        threadId,
        input: [{ type: "text", text: "Reply with exactly CODEX_PROVIDER_OK. Do not use tools." }],
        cwd: this.options.cwd,
        approvalPolicy: "never",
        sandboxPolicy: { type: "readOnly" },
        ...(this.options.model ? { model: this.options.model } : {}),
      });
      const turn = readTurn(response.turn);
      if (!turn) {
        return {
          healthy: false,
          failure: classifyFailure({ message: "Health probe did not return a turn" }),
        };
      }
      turnId = turn.id;
      const completion = await this.waitForCompletion(turn.id);
      if (completion.status === "completed") {
        return { healthy: true };
      }

      return {
        healthy: false,
        failure: classifyFailure(
          completion.error ?? { message: `Health probe turn ${completion.status}` },
        ),
      };
    } catch (error) {
      if (turnId && this.healthThreadId) {
        await this.rpc
          .request("turn/interrupt", { threadId: this.healthThreadId, turnId })
          .catch(() => undefined);
      }
      return {
        healthy: false,
        failure: classifyFailure({
          message: error instanceof Error ? error.message : String(error),
        }),
      };
    }
  }

  dispose(): void {
    this.unsubscribe();
  }

  private async ensureHealthThread(): Promise<string> {
    if (this.healthThreadId) {
      return this.healthThreadId;
    }

    const response = await this.rpc.request<{ thread?: unknown }>("thread/start", {
      cwd: this.options.cwd,
      ephemeral: true,
      sandbox: "read-only",
      approvalPolicy: "never",
      developerInstructions:
        "This is a provider health check. Never call tools. Reply only with the requested marker.",
      ...(this.options.model ? { model: this.options.model } : {}),
    });
    const threadId = isRecord(response.thread) ? readString(response.thread.id) : undefined;
    if (!threadId) {
      throw new Error("Health probe did not return a thread id");
    }
    this.healthThreadId = threadId;
    return threadId;
  }

  private handleNotification(message: JsonRpcMessage): void {
    if (message.method !== "turn/completed" || !message.params) {
      return;
    }
    if (readString(message.params.threadId) !== this.healthThreadId) {
      return;
    }

    const turn = readTurn(message.params.turn);
    if (!turn || turn.status === "inProgress") {
      return;
    }
    const completion: TurnCompletion = {
      status: turn.status,
      ...(turn.error ? { error: turn.error } : {}),
    };
    this.completions.set(turn.id, completion);
    const waiter = this.waiters.get(turn.id);
    if (waiter) {
      this.waiters.delete(turn.id);
      waiter(completion);
    }
  }

  private waitForCompletion(turnId: string): Promise<TurnCompletion> {
    const existing = this.completions.get(turnId);
    if (existing) {
      this.completions.delete(turnId);
      return Promise.resolve(existing);
    }

    return new Promise<TurnCompletion>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.waiters.delete(turnId);
        reject(new Error(`Provider health probe timed out after ${this.options.timeoutMs}ms`));
      }, this.options.timeoutMs);
      this.waiters.set(turnId, (completion) => {
        clearTimeout(timer);
        this.completions.delete(turnId);
        resolve(completion);
      });
    });
  }

  private async checkHealthEndpoint(): Promise<ProbeResult> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), Math.min(this.options.timeoutMs, 15_000));
    try {
      const response = await fetch(this.options.healthUrl!, {
        method: "GET",
        signal: controller.signal,
      });
      if (response.ok) {
        return { healthy: true };
      }
      const retryAfter = response.headers.get("retry-after");
      const retrySeconds = retryAfter ? Number(retryAfter) : Number.NaN;
      return {
        healthy: false,
        failure: classifyFailure({
          message: `Provider health endpoint returned HTTP ${response.status}`,
          codexErrorInfo: { httpConnectionFailed: { httpStatusCode: response.status } },
        }),
        ...(Number.isFinite(retrySeconds) ? { retryAfterMs: retrySeconds * 1_000 } : {}),
      };
    } catch (error) {
      return {
        healthy: false,
        failure: classifyFailure({
          message: error instanceof Error ? error.message : String(error),
          codexErrorInfo: { httpConnectionFailed: {} },
        }),
      };
    } finally {
      clearTimeout(timer);
    }
  }
}
