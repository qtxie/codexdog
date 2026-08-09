import { createHash } from "node:crypto";
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";

export type RuntimePhase =
  | "starting"
  | "idle"
  | "running"
  | "waiting-for-user"
  | "suspected-stall"
  | "interrupting-stall"
  | "provider-down"
  | "probing"
  | "resuming"
  | "needs-attention"
  | "stopped";

export interface SupervisorState {
  version: 1;
  pid: number;
  cwd: string;
  phase: RuntimePhase;
  appServerPort?: number;
  proxyPort?: number;
  controlPort?: number;
  controlToken?: string;
  currentThreadId?: string;
  activeTurnId?: string;
  lastFailedTurnId?: string;
  resumeRequestedForTurnId?: string;
  automaticResumeCount: number;
  stallRecoveryCount: number;
  probeAttempt: number;
  consecutiveProbeSuccesses: number;
  lastTurnActivityAt?: string;
  stallSuspectedAt?: string;
  stallPausedReason?: string;
  nextProbeAt?: string;
  lastError?: string;
  updatedAt: string;
  stoppedReason?: string;
}

export type PublicSupervisorState = Omit<SupervisorState, "controlToken"> & {
  live: boolean;
};

function workspaceKey(cwd: string): string {
  return createHash("sha256").update(resolve(cwd).toLowerCase()).digest("hex").slice(0, 12);
}

export class StateStore {
  readonly path: string;
  readonly logPath: string;
  private writeChain = Promise.resolve();

  constructor(root: string, cwd: string) {
    const key = workspaceKey(cwd);
    this.path = join(root, `state-${key}.json`);
    this.logPath = join(root, `supervisor-${key}.log`);
  }

  async initialize(): Promise<void> {
    await mkdir(dirname(this.path), { recursive: true });
  }

  write(state: SupervisorState): Promise<void> {
    const serialized = `${JSON.stringify(state, null, 2)}\n`;
    this.writeChain = this.writeChain.catch(() => undefined).then(async () => {
      await this.initialize();
      const temporary = `${this.path}.${process.pid}.tmp`;
      await writeFile(temporary, serialized, {
        encoding: "utf8",
        mode: 0o600,
      });
      await rename(temporary, this.path);
    });
    return this.writeChain;
  }

  async read(): Promise<SupervisorState | undefined> {
    try {
      return JSON.parse(await readFile(this.path, "utf8")) as SupervisorState;
    } catch {
      return undefined;
    }
  }
}

export function publicState(state: SupervisorState, live: boolean): PublicSupervisorState {
  const { controlToken: _controlToken, ...visible } = state;
  return { ...visible, live };
}
