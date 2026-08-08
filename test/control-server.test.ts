import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  queryControl,
  requestStop,
  startControlServer,
} from "../src/control-server.js";
import { StateStore, type SupervisorState } from "../src/state-store.js";

describe("control server", () => {
  it("authenticates status and stop requests", async () => {
    const root = await mkdtemp(join(tmpdir(), "codex-supervisor-control-"));
    const store = new StateStore(root, process.cwd());
    const state: SupervisorState = {
      version: 1,
      pid: process.pid,
      cwd: process.cwd(),
      phase: "idle",
      automaticResumeCount: 0,
      probeAttempt: 0,
      consecutiveProbeSuccesses: 0,
      updatedAt: new Date().toISOString(),
    };
    let stopped = false;
    const handle = await startControlServer(store, () => state, () => {
      stopped = true;
    });
    state.controlPort = handle.port;
    state.controlToken = handle.token;

    try {
      await expect(queryControl(state)).resolves.toBe(true);
      await expect(requestStop(state)).resolves.toBe(true);
      await new Promise((resolve) => setImmediate(resolve));
      expect(stopped).toBe(true);
    } finally {
      await handle.close();
      await rm(root, { recursive: true, force: true });
    }
  });
});
