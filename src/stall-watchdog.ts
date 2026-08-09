import { isRecord, readBoolean, readString, type JsonRpcMessage } from "./protocol.js";

export interface StallWatchdogOptions {
  stallTimeoutMs: number;
  stallConfirmMs: number;
  toolStallTimeoutMs: number;
}

export interface StallContext {
  threadId: string;
  turnId: string;
  lastActivityAt: number;
  idleMs: number;
  activitySequence: number;
  blockingItemTypes: string[];
}

export type StallDecision =
  | { kind: "suspected"; context: StallContext; confirmAt: number }
  | { kind: "confirmed"; context: StallContext };

export interface StallSnapshot {
  threadId?: string;
  turnId?: string;
  lastActivityAt?: number;
  suspectedAt?: number;
  pauseReason?: string;
  recovering: boolean;
}

export interface StallObservation {
  activity: boolean;
  suspicionCleared: boolean;
}

interface ActiveTurnState {
  threadId: string;
  turnId: string;
  lastActivityAt: number;
  activitySequence: number;
  blockingItems: Map<string, string>;
  waitingFlags: Set<string>;
  safetyBuffering: boolean;
  verificationRequired: boolean;
  suspectedAt: number | undefined;
  recovering: boolean;
}

const BLOCKING_ITEM_TYPES = new Set([
  "collabToolCall",
  "commandExecution",
  "contextCompaction",
  "dynamicToolCall",
  "fileChange",
  "imageView",
  "mcpToolCall",
  "webSearch",
]);

const WAITING_FLAGS = new Set(["waitingOnApproval", "waitingOnUserInput"]);

export class StallWatchdog {
  private active: ActiveTurnState | undefined;

  constructor(private readonly options: StallWatchdogOptions) {}

  get enabled(): boolean {
    return this.options.stallTimeoutMs > 0;
  }

  startTurn(threadId: string, turnId: string, now = Date.now()): void {
    if (!this.enabled) {
      return;
    }
    this.active = {
      threadId,
      turnId,
      lastActivityAt: now,
      activitySequence: 0,
      blockingItems: new Map(),
      waitingFlags: new Set(),
      safetyBuffering: false,
      verificationRequired: false,
      suspectedAt: undefined,
      recovering: false,
    };
  }

  completeTurn(turnId: string): void {
    if (this.active?.turnId === turnId) {
      this.active = undefined;
    }
  }

  observeServerMessage(message: JsonRpcMessage, now = Date.now()): StallObservation {
    const active = this.active;
    const params = message.params;
    if (!active || !message.method || !params || !this.matchesActiveTurn(params, active)) {
      return { activity: false, suspicionCleared: false };
    }

    const suspicionCleared = active.suspectedAt !== undefined;
    const method = message.method;

    if (method === "thread/status/changed") {
      const status = params.status;
      if (isRecord(status)) {
        const flags = Array.isArray(status.activeFlags) ? status.activeFlags : [];
        this.assignWaitingFlags(active, flags);
      }
    } else if (method === "model/safetyBuffering/updated") {
      active.safetyBuffering = readBoolean(params.showBufferingUi) ?? true;
    } else if (method === "model/verification") {
      active.verificationRequired = true;
    } else if (method === "item/started") {
      this.trackStartedItem(params, active);
    } else if (method === "item/completed") {
      this.trackCompletedItem(params, active);
    } else if (method === "hook/started") {
      active.blockingItems.set(hookKey(params), "hook");
    } else if (method === "hook/completed") {
      active.blockingItems.delete(hookKey(params));
    }

    this.recordActivity(active, now);
    return { activity: true, suspicionCleared };
  }

  evaluate(now = Date.now()): StallDecision | undefined {
    const active = this.active;
    if (!active || active.recovering || this.pauseReason(active)) {
      return undefined;
    }

    const hasBlockingItems = active.blockingItems.size > 0;
    const timeoutMs = hasBlockingItems
      ? this.options.toolStallTimeoutMs
      : this.options.stallTimeoutMs;
    if (timeoutMs <= 0) {
      return undefined;
    }

    const idleMs = Math.max(0, now - active.lastActivityAt);
    if (idleMs < timeoutMs) {
      active.suspectedAt = undefined;
      return undefined;
    }

    const context = this.context(active, idleMs);
    if (active.suspectedAt === undefined) {
      active.suspectedAt = now;
      return {
        kind: "suspected",
        context,
        confirmAt: now + this.options.stallConfirmMs,
      };
    }

    if (now - active.suspectedAt < this.options.stallConfirmMs) {
      return undefined;
    }

    active.recovering = true;
    return { kind: "confirmed", context };
  }

  isCurrent(context: StallContext): boolean {
    const active = this.active;
    return Boolean(
      active &&
        active.recovering &&
        active.threadId === context.threadId &&
        active.turnId === context.turnId &&
        active.activitySequence === context.activitySequence,
    );
  }

  cancelRecovery(): void {
    if (!this.active) {
      return;
    }
    this.active.recovering = false;
    this.active.suspectedAt = undefined;
  }

  setWaitingFlags(threadId: string, flags: string[], now = Date.now()): void {
    if (!this.active || this.active.threadId !== threadId) {
      return;
    }
    this.assignWaitingFlags(this.active, flags);
    this.recordActivity(this.active, now);
    this.active.recovering = false;
  }

  defer(now = Date.now()): void {
    if (!this.active) {
      return;
    }
    this.recordActivity(this.active, now);
    this.active.recovering = false;
  }

  snapshot(): StallSnapshot {
    const active = this.active;
    if (!active) {
      return { recovering: false };
    }
    const pauseReason = this.pauseReason(active);
    return {
      threadId: active.threadId,
      turnId: active.turnId,
      lastActivityAt: active.lastActivityAt,
      ...(active.suspectedAt !== undefined ? { suspectedAt: active.suspectedAt } : {}),
      ...(pauseReason ? { pauseReason } : {}),
      recovering: active.recovering,
    };
  }

  private matchesActiveTurn(
    params: Record<string, unknown>,
    active: ActiveTurnState,
  ): boolean {
    const threadId = readString(params.threadId);
    const directTurnId = readString(params.turnId);
    const nestedTurnId = isRecord(params.turn) ? readString(params.turn.id) : undefined;
    const turnId = directTurnId ?? nestedTurnId;
    if (threadId && threadId !== active.threadId) {
      return false;
    }
    if (turnId && turnId !== active.turnId) {
      return false;
    }
    return Boolean(threadId || turnId);
  }

  private trackStartedItem(params: Record<string, unknown>, active: ActiveTurnState): void {
    const item = params.item;
    if (!isRecord(item)) {
      return;
    }
    const id = readString(item.id);
    const type = readString(item.type);
    if (id && type && BLOCKING_ITEM_TYPES.has(type)) {
      active.blockingItems.set(id, type);
    }
  }

  private assignWaitingFlags(active: ActiveTurnState, flags: unknown[]): void {
    active.waitingFlags = new Set(
      flags.filter((flag): flag is string => typeof flag === "string" && WAITING_FLAGS.has(flag)),
    );
  }

  private trackCompletedItem(params: Record<string, unknown>, active: ActiveTurnState): void {
    const item = params.item;
    if (!isRecord(item)) {
      return;
    }
    const id = readString(item.id);
    if (id) {
      active.blockingItems.delete(id);
    }
  }

  private recordActivity(active: ActiveTurnState, now: number): void {
    active.lastActivityAt = now;
    active.activitySequence += 1;
    active.suspectedAt = undefined;
  }

  private pauseReason(active: ActiveTurnState): string | undefined {
    const waiting = active.waitingFlags.values().next().value as string | undefined;
    if (waiting) {
      return waiting;
    }
    if (active.verificationRequired) {
      return "verificationRequired";
    }
    if (active.safetyBuffering) {
      return "safetyBuffering";
    }
    if (active.blockingItems.size > 0 && this.options.toolStallTimeoutMs <= 0) {
      const itemType = active.blockingItems.values().next().value as string | undefined;
      return `activeTool:${itemType ?? "unknown"}`;
    }
    return undefined;
  }

  private context(active: ActiveTurnState, idleMs: number): StallContext {
    return {
      threadId: active.threadId,
      turnId: active.turnId,
      lastActivityAt: active.lastActivityAt,
      idleMs,
      activitySequence: active.activitySequence,
      blockingItemTypes: [...new Set(active.blockingItems.values())].sort(),
    };
  }
}

function hookKey(params: Record<string, unknown>): string {
  const run = params.run;
  const id = isRecord(run) ? readString(run.id) : undefined;
  return `hook:${id ?? "active"}`;
}
