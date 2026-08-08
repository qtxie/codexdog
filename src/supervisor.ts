import { spawn, type ChildProcess } from "node:child_process";
import { createServer } from "node:net";
import { delay, jitteredDelay } from "./backoff.js";
import { startControlServer, type ControlServerHandle } from "./control-server.js";
import { nextCyberPolicyRecoveryAction } from "./cyber-policy-recovery.js";
import {
  classifyFailure,
  formatFailure,
  type ClassifiedFailure,
} from "./failure-classifier.js";
import { JsonRpcClient } from "./json-rpc-client.js";
import { Logger, sanitizeText } from "./logger.js";
import {
  isRecord,
  readBoolean,
  readString,
  readTurn,
  readTurnError,
  type JsonRpcMessage,
  type TurnError,
} from "./protocol.js";
import { ProviderProbe } from "./provider-probe.js";
import { type StateStore, type SupervisorState } from "./state-store.js";
import { TuiProxy } from "./tui-proxy.js";

export interface SupervisorOptions {
  cwd: string;
  codexPath: string;
  codexConfig: string[];
  tuiArgs: string[];
  healthUrl?: string;
  probeModel?: string;
  probeTimeoutMs: number;
  probeSuccesses: number;
  backoffMs: number[];
  maxAutoResumes: number;
}

interface RecoveryContext {
  threadId: string;
  failedTurnId: string;
  failure: ClassifiedFailure;
}

const CONTINUATION_PROMPT =
  "The previous turn ended because the model provider was unavailable. Continue the unfinished task from the existing thread and workspace state. Inspect existing work first, verify what already completed, and do not repeat completed steps.";

export class Supervisor {
  private state: SupervisorState;
  private readonly logger: Logger;
  private appServer?: ChildProcess;
  private tui?: ChildProcess;
  private proxy?: TuiProxy;
  private rpc?: JsonRpcClient;
  private probe?: ProviderProbe;
  private control?: ControlServerHandle;
  private shuttingDown = false;
  private recoveryAbort: AbortController | undefined;
  private pendingRecovery: RecoveryContext | undefined;
  private submittingResume = false;
  private readonly turnErrors = new Map<string, TurnError>();
  private readonly handledTurns = new Set<string>();
  private readonly cyberPolicyAttempts = new Map<string, number>();

  constructor(
    private readonly options: SupervisorOptions,
    private readonly store: StateStore,
  ) {
    this.logger = new Logger(store.logPath);
    this.state = {
      version: 1,
      pid: process.pid,
      cwd: options.cwd,
      phase: "starting",
      automaticResumeCount: 0,
      probeAttempt: 0,
      consecutiveProbeSuccesses: 0,
      updatedAt: new Date().toISOString(),
    };
  }

  async run(): Promise<number> {
    await this.store.initialize();
    await this.logger.initialize();
    await this.persist();
    this.logger.log(`Starting supervisor in ${this.options.cwd}`);

    try {
      const appServerPort = await getFreePort();
      const appServerUrl = `ws://127.0.0.1:${appServerPort}`;
      this.appServer = this.spawnAppServer(appServerUrl);
      await waitForReady(appServerPort, this.appServer);

      this.proxy = new TuiProxy(appServerUrl);
      const proxyPort = await this.proxy.start();
      this.proxy.onServerMessage((message) => void this.handleServerMessage(message));
      this.proxy.onClientMessage((message) => void this.handleClientMessage(message));

      this.rpc = new JsonRpcClient(appServerUrl, Math.max(30_000, this.options.probeTimeoutMs));
      await this.rpc.connect();
      await this.rpc.initialize();
      this.probe = new ProviderProbe(this.rpc, {
        cwd: this.options.cwd,
        timeoutMs: this.options.probeTimeoutMs,
        ...(this.options.healthUrl ? { healthUrl: this.options.healthUrl } : {}),
        ...(this.options.probeModel ? { model: this.options.probeModel } : {}),
      });

      this.control = await startControlServer(this.store, () => this.state, () => {
        void this.shutdown("stop requested");
      });

      this.state.appServerPort = appServerPort;
      this.state.proxyPort = proxyPort;
      this.state.controlPort = this.control.port;
      this.state.controlToken = this.control.token;
      this.state.phase = "idle";
      await this.persist();

      const proxyUrl = `ws://127.0.0.1:${proxyPort}`;
      process.stdout.write(`Codex supervisor is active for ${this.options.cwd}\n`);
      process.stdout.write(`State: ${this.store.path}\n`);
      this.tui = this.spawnTui(proxyUrl);
      this.installSignalHandlers();

      const exitCode = await new Promise<number>((resolve, reject) => {
        this.tui?.once("error", reject);
        this.tui?.once("exit", (code) => resolve(code ?? 0));
      });
      await this.shutdown(`Codex TUI exited with code ${exitCode}`);
      return exitCode;
    } catch (error) {
      this.logger.log(`Startup failure: ${error instanceof Error ? error.message : String(error)}`);
      await this.shutdown("startup failure");
      throw error;
    }
  }

  private spawnAppServer(url: string): ChildProcess {
    const configArgs = this.options.codexConfig.flatMap((value) => ["-c", value]);
    const child = spawn(
      this.options.codexPath,
      ["app-server", ...configArgs, "--listen", url],
      {
        cwd: this.options.cwd,
        stdio: ["ignore", "ignore", "pipe"],
        windowsHide: true,
      },
    );
    child.stderr?.resume();
    child.once("exit", (code) => {
      if (!this.shuttingDown) {
        this.logger.log(`Codex app-server exited unexpectedly with code ${String(code)}`);
        void this.setAttention("Codex app-server exited unexpectedly");
      }
    });
    return child;
  }

  private spawnTui(proxyUrl: string): ChildProcess {
    const configArgs = this.options.codexConfig.flatMap((value) => ["-c", value]);
    return spawn(
      this.options.codexPath,
      [...configArgs, "--remote", proxyUrl, "-C", this.options.cwd, ...this.options.tuiArgs],
      {
        cwd: this.options.cwd,
        stdio: "inherit",
        windowsHide: false,
      },
    );
  }

  private handleClientMessage(message: JsonRpcMessage): void {
    if (message.method !== "turn/start") {
      return;
    }
    const threadId = message.params ? readString(message.params.threadId) : undefined;
    if (threadId) {
      this.state.currentThreadId = threadId;
    }

    if (this.recoveryAbort && !this.submittingResume) {
      this.logger.log("User started a turn while recovery was active; automatic recovery cancelled");
      this.cancelRecovery();
      this.state.automaticResumeCount = 0;
    }
    if (!this.submittingResume && threadId) {
      this.cyberPolicyAttempts.delete(threadId);
    }
    this.state.phase = "running";
    this.state.probeAttempt = 0;
    this.state.consecutiveProbeSuccesses = 0;
    delete this.state.nextProbeAt;
    void this.persist();
  }

  private async handleServerMessage(message: JsonRpcMessage): Promise<void> {
    const params = message.params;
    if (!params || !message.method) {
      return;
    }

    if (message.method === "thread/started") {
      const threadId = isRecord(params.thread) ? readString(params.thread.id) : undefined;
      if (threadId) {
        this.state.currentThreadId = threadId;
        await this.persist();
      }
      return;
    }

    if (message.method === "thread/status/changed") {
      this.handleThreadStatus(params);
      return;
    }

    if (message.method === "error") {
      const turnId = readString(params.turnId);
      const error = readTurnError(params.error);
      if (turnId && error) {
        this.turnErrors.set(turnId, error);
        const willRetry = readBoolean(params.willRetry) ?? false;
        this.logger.log(`Turn ${turnId} error (willRetry=${willRetry}): ${error.message}`);
      }
      return;
    }

    if (message.method === "turn/started") {
      const threadId = readString(params.threadId);
      const turn = readTurn(params.turn);
      if (threadId) {
        this.state.currentThreadId = threadId;
      }
      if (turn) {
        this.state.activeTurnId = turn.id;
      }
      this.state.phase = "running";
      await this.persist();
      return;
    }

    if (message.method !== "turn/completed") {
      return;
    }

    const threadId = readString(params.threadId);
    const turn = readTurn(params.turn);
    if (!threadId || !turn || this.handledTurns.has(turn.id)) {
      return;
    }
    this.handledTurns.add(turn.id);
    this.state.currentThreadId = threadId;
    delete this.state.activeTurnId;

    if (turn.status === "completed") {
      this.turnErrors.delete(turn.id);
      this.cyberPolicyAttempts.delete(threadId);
      this.cancelRecovery();
      this.state.phase = "idle";
      this.state.automaticResumeCount = 0;
      this.state.probeAttempt = 0;
      this.state.consecutiveProbeSuccesses = 0;
      delete this.state.lastError;
      delete this.state.nextProbeAt;
      await this.persist();
      this.logger.log(`Turn ${turn.id} completed`);
      return;
    }

    if (turn.status === "interrupted") {
      this.turnErrors.delete(turn.id);
      this.cyberPolicyAttempts.delete(threadId);
      this.cancelRecovery();
      this.state.phase = "idle";
      this.state.automaticResumeCount = 0;
      delete this.state.nextProbeAt;
      await this.persist();
      this.logger.log(`Turn ${turn.id} was interrupted; no recovery scheduled`);
      return;
    }

    const error = turn.error ?? this.turnErrors.get(turn.id) ?? { message: "Codex turn failed" };
    this.turnErrors.delete(turn.id);
    const failure = classifyFailure(error);
    this.state.lastFailedTurnId = turn.id;
    this.state.lastError = sanitizeText(formatFailure(failure));

    if (failure.code === "cyberPolicy") {
      await this.recoverCyberPolicy({ threadId, failedTurnId: turn.id, failure });
      return;
    }

    if (failure.disposition === "permanent") {
      await this.setAttention(`Non-recoverable turn failure: ${formatFailure(failure)}`);
      return;
    }

    this.startRecovery({ threadId, failedTurnId: turn.id, failure });
  }

  private handleThreadStatus(params: Record<string, unknown>): void {
    const status = params.status;
    if (!isRecord(status)) {
      return;
    }
    const type = readString(status.type);
    const activeFlags = Array.isArray(status.activeFlags) ? status.activeFlags : [];
    const waiting = activeFlags.some(
      (flag) => flag === "waitingOnApproval" || flag === "waitingOnUserInput",
    );
    if (type === "active" && waiting) {
      this.state.phase = "waiting-for-user";
      void this.persist();
    }
  }

  private async recoverCyberPolicy(context: RecoveryContext): Promise<void> {
    if (!this.proxy) {
      await this.setAttention("Cyber policy recovery is unavailable before the TUI proxy starts");
      return;
    }
    if (this.state.automaticResumeCount >= this.options.maxAutoResumes) {
      await this.setAttention(
        `Automatic resume limit (${this.options.maxAutoResumes}) reached for ${context.threadId}`,
      );
      return;
    }
    if (this.state.resumeRequestedForTurnId === context.failedTurnId) {
      await this.setAttention(`Resume was already requested for failed turn ${context.failedTurnId}`);
      return;
    }

    const attemptsSubmitted = this.cyberPolicyAttempts.get(context.threadId) ?? 0;
    const action = nextCyberPolicyRecoveryAction(attemptsSubmitted);
    if (!action) {
      await this.setAttention(
        `Cyber policy recovery exhausted after ${attemptsSubmitted} attempts for ${context.threadId}`,
      );
      return;
    }

    this.submittingResume = true;
    this.state.phase = "resuming";
    this.state.resumeRequestedForTurnId = context.failedTurnId;
    this.state.automaticResumeCount += 1;
    delete this.state.nextProbeAt;
    await this.persist();

    let targetThreadId = context.threadId;
    this.cyberPolicyAttempts.set(context.threadId, attemptsSubmitted + 1);

    try {
      if (action.kind === "fork-thread") {
        const result = await this.proxy.request<{ thread?: unknown }>("thread/fork", {
          threadId: context.threadId,
          lastTurnId: context.failedTurnId,
        });
        const forkedThreadId = isRecord(result.thread)
          ? readString(result.thread.id)
          : undefined;
        if (!forkedThreadId) {
          throw new Error("Codex did not return a forked thread");
        }
        targetThreadId = forkedThreadId;
        this.cyberPolicyAttempts.delete(context.threadId);
        this.cyberPolicyAttempts.set(targetThreadId, attemptsSubmitted + 1);
      } else {
        await this.proxy.request("thread/resume", {
          threadId: context.threadId,
          cwd: this.options.cwd,
        });
      }

      const result = await this.proxy.request<{ turn?: unknown }>("turn/start", {
        threadId: targetThreadId,
        input: [{ type: "text", text: action.prompt }],
        cwd: this.options.cwd,
        clientUserMessageId: `codex-supervisor:cyber-policy:${attemptsSubmitted + 1}:${context.failedTurnId}`,
      });
      const turn = readTurn(result.turn);
      if (!turn) {
        throw new Error("Codex did not return a cyber policy recovery turn");
      }

      this.state.phase = "running";
      this.state.activeTurnId = turn.id;
      this.state.currentThreadId = targetThreadId;
      this.state.probeAttempt = 0;
      this.state.consecutiveProbeSuccesses = 0;
      await this.persist();
      this.logger.log(
        `Cyber policy recovery ${attemptsSubmitted + 1}/3 started turn ${turn.id} on ${targetThreadId}`,
      );
    } catch (error) {
      await this.setAttention(
        `Cyber policy recovery failed: ${error instanceof Error ? error.message : String(error)}`,
      );
    } finally {
      this.submittingResume = false;
    }
  }

  private startRecovery(context: RecoveryContext): void {
    if (this.state.automaticResumeCount >= this.options.maxAutoResumes) {
      void this.setAttention(
        `Automatic resume limit (${this.options.maxAutoResumes}) reached for ${context.threadId}`,
      );
      return;
    }

    if (this.recoveryAbort) {
      this.pendingRecovery = context;
      return;
    }

    const controller = new AbortController();
    this.recoveryAbort = controller;
    this.state.phase = "provider-down";
    this.state.probeAttempt = 0;
    this.state.consecutiveProbeSuccesses = 0;
    void this.persist();
    this.logger.log(`Provider recovery started after ${formatFailure(context.failure)}`);

    void this.runRecovery(context, controller.signal)
      .catch((error) => {
        if (!controller.signal.aborted) {
          void this.setAttention(
            `Recovery failed: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
      })
      .finally(() => {
        if (this.recoveryAbort === controller) {
          this.recoveryAbort = undefined;
        }
        const pending = this.pendingRecovery;
        this.pendingRecovery = undefined;
        if (pending && !this.shuttingDown) {
          this.startRecovery(pending);
        }
      });
  }

  private async runRecovery(context: RecoveryContext, signal: AbortSignal): Promise<void> {
    if (!this.probe || !this.rpc) {
      throw new Error("Recovery services are not initialized");
    }

    let attempt = 0;
    let successes = 0;
    let retryAfterMs: number | undefined;
    while (!signal.aborted) {
      const configured = this.options.backoffMs[Math.min(attempt, this.options.backoffMs.length - 1)];
      if (configured === undefined) {
        throw new Error("No provider probe backoff is configured");
      }
      const waitMs = retryAfterMs ?? (successes > 0 ? 1_000 : jitteredDelay(configured));
      this.state.phase = "provider-down";
      this.state.probeAttempt = attempt + 1;
      this.state.consecutiveProbeSuccesses = successes;
      this.state.nextProbeAt = new Date(Date.now() + waitMs).toISOString();
      await this.persist();
      await delay(waitMs, signal);

      this.state.phase = "probing";
      delete this.state.nextProbeAt;
      await this.persist();
      const result = await this.probe.check();
      if (signal.aborted) {
        return;
      }

      if (result.healthy) {
        successes += 1;
        this.state.consecutiveProbeSuccesses = successes;
        await this.persist();
        this.logger.log(
          `Provider probe succeeded (${successes}/${this.options.probeSuccesses})`,
        );
        if (successes >= this.options.probeSuccesses) {
          await this.resumeThread(context, signal);
          return;
        }
      } else {
        successes = 0;
        const failure = result.failure;
        this.state.consecutiveProbeSuccesses = 0;
        this.state.lastError = sanitizeText(
          failure ? formatFailure(failure) : "Provider probe failed",
        );
        await this.persist();
        this.logger.log(`Provider probe failed: ${this.state.lastError}`);
        if (failure?.disposition === "permanent") {
          await this.setAttention(`Provider probe requires attention: ${formatFailure(failure)}`);
          return;
        }
      }

      retryAfterMs = result.retryAfterMs;
      attempt += 1;
    }
  }

  private async resumeThread(context: RecoveryContext, signal: AbortSignal): Promise<void> {
    if (!this.proxy || signal.aborted) {
      return;
    }
    if (this.state.resumeRequestedForTurnId === context.failedTurnId) {
      await this.setAttention(`Resume was already requested for failed turn ${context.failedTurnId}`);
      return;
    }

    this.submittingResume = true;
    this.state.phase = "resuming";
    this.state.resumeRequestedForTurnId = context.failedTurnId;
    this.state.automaticResumeCount += 1;
    delete this.state.nextProbeAt;
    await this.persist();

    try {
      await this.proxy.request("thread/resume", {
        threadId: context.threadId,
        cwd: this.options.cwd,
      });
      const result = await this.proxy.request<{ turn?: unknown }>("turn/start", {
        threadId: context.threadId,
        input: [{ type: "text", text: CONTINUATION_PROMPT }],
        cwd: this.options.cwd,
        clientUserMessageId: `codex-supervisor:${context.failedTurnId}`,
      });
      const turn = readTurn(result.turn);
      if (!turn) {
        throw new Error("Codex did not return a resumed turn");
      }
      this.state.phase = "running";
      this.state.activeTurnId = turn.id;
      this.state.currentThreadId = context.threadId;
      this.state.probeAttempt = 0;
      this.state.consecutiveProbeSuccesses = 0;
      await this.persist();
      this.logger.log(
        `Resumed thread ${context.threadId} as turn ${turn.id} after failure ${context.failedTurnId}`,
      );
    } finally {
      this.submittingResume = false;
    }
  }

  private cancelRecovery(): void {
    this.pendingRecovery = undefined;
    this.recoveryAbort?.abort(new Error("Recovery cancelled"));
    this.recoveryAbort = undefined;
  }

  private async setAttention(message: string): Promise<void> {
    this.cancelRecovery();
    this.state.phase = "needs-attention";
    this.state.lastError = sanitizeText(message);
    delete this.state.nextProbeAt;
    await this.persist();
    this.logger.log(message);
  }

  private async persist(): Promise<void> {
    this.state.updatedAt = new Date().toISOString();
    await this.store.write(this.state);
  }

  private installSignalHandlers(): void {
    process.on("SIGINT", () => {
      // The foreground TUI owns Ctrl+C so it can interrupt an active turn.
      this.logger.log("SIGINT received by supervisor; leaving handling to the Codex TUI");
    });
    process.once("SIGTERM", () => void this.shutdown("SIGTERM"));
    process.once("SIGHUP", () => void this.shutdown("SIGHUP"));
  }

  private async shutdown(reason: string): Promise<void> {
    if (this.shuttingDown) {
      return;
    }
    this.shuttingDown = true;
    this.cancelRecovery();
    this.logger.log(`Stopping supervisor: ${reason}`);

    this.state.phase = "stopped";
    this.state.stoppedReason = reason;
    delete this.state.controlToken;
    delete this.state.nextProbeAt;
    await this.persist().catch(() => undefined);

    this.probe?.dispose();
    this.rpc?.close();
    await this.proxy?.close().catch(() => undefined);
    this.tui?.kill();
    this.appServer?.kill();
    await this.control?.close().catch(() => undefined);
  }
}

async function getFreePort(): Promise<number> {
  const server = createServer();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (!address || typeof address === "string") {
    throw new Error("Could not allocate a localhost port");
  }
  const port = address.port;
  await new Promise<void>((resolve) => server.close(() => resolve()));
  return port;
}

async function waitForReady(port: number, processHandle: ChildProcess): Promise<void> {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    if (processHandle.exitCode !== null) {
      throw new Error(`Codex app-server exited with code ${processHandle.exitCode}`);
    }
    try {
      const response = await fetch(`http://127.0.0.1:${port}/readyz`, {
        signal: AbortSignal.timeout(1_000),
      });
      if (response.ok) {
        return;
      }
    } catch {
      // The listener may not be bound yet.
    }
    await delay(100);
  }
  throw new Error("Timed out waiting for Codex app-server readiness");
}
