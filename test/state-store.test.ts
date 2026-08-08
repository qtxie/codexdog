import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { publicState, StateStore, type SupervisorState } from "../src/state-store.js";

const temporaryDirectories: string[] = [];

afterEach(async () => {
  await Promise.all(
    temporaryDirectories.splice(0).map((path) => rm(path, { recursive: true, force: true })),
  );
});

describe("StateStore", () => {
  it("atomically replaces state and hides the control token", async () => {
    const root = await mkdtemp(join(tmpdir(), "codex-supervisor-test-"));
    temporaryDirectories.push(root);
    const store = new StateStore(root, process.cwd());
    const state: SupervisorState = {
      version: 1,
      pid: 42,
      cwd: process.cwd(),
      phase: "starting",
      automaticResumeCount: 0,
      probeAttempt: 0,
      consecutiveProbeSuccesses: 0,
      controlToken: "secret",
      updatedAt: new Date().toISOString(),
    };

    await store.write(state);
    state.phase = "idle";
    await store.write(state);

    expect((await store.read())?.phase).toBe("idle");
    expect(publicState(state, true)).not.toHaveProperty("controlToken");
  });
});
