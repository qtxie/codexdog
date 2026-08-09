#!/usr/bin/env node

import { resolve } from "node:path";
import { homedir } from "node:os";
import { queryControl, requestStop } from "./control-server.js";
import { StateStore, publicState } from "./state-store.js";
import { Supervisor, type SupervisorOptions } from "./supervisor.js";

interface ParsedArguments extends SupervisorOptions {
  command: "start" | "status" | "stop" | "help";
  stateRoot: string;
  json: boolean;
}

const DEFAULT_BACKOFF = [2_000, 5_000, 10_000, 20_000, 30_000, 60_000];

async function main(): Promise<number> {
  const args = parseArguments(process.argv.slice(2));
  if (args.command === "help") {
    printHelp();
    return 0;
  }

  const store = new StateStore(args.stateRoot, args.cwd);
  if (args.command === "status") {
    const state = await store.read();
    if (!state) {
      process.stdout.write(`No supervisor state for ${args.cwd}\n`);
      return 1;
    }
    const live = await queryControl(state);
    const visible = publicState(state, live);
    if (args.json) {
      process.stdout.write(`${JSON.stringify(visible, null, 2)}\n`);
    } else {
      process.stdout.write(
        [
          `Workspace: ${visible.cwd}`,
          `Live: ${visible.live ? "yes" : "no"}`,
          `Phase: ${visible.phase}`,
          `Thread: ${visible.currentThreadId ?? "-"}`,
          `Turn: ${visible.activeTurnId ?? "-"}`,
          `Automatic resumes: ${visible.automaticResumeCount}`,
          `Stall resumes: ${visible.stallRecoveryCount ?? 0}`,
          `Last turn activity: ${visible.lastTurnActivityAt ?? "-"}`,
          `Stall suspected: ${visible.stallSuspectedAt ?? "-"}`,
          `Watchdog pause: ${visible.stallPausedReason ?? "-"}`,
          `Last error: ${visible.lastError ?? "-"}`,
          `Updated: ${visible.updatedAt}`,
        ].join("\n") + "\n",
      );
    }
    return live ? 0 : 1;
  }

  if (args.command === "stop") {
    const state = await store.read();
    if (!state || !(await requestStop(state))) {
      process.stderr.write(`No live supervisor for ${args.cwd}\n`);
      return 1;
    }
    process.stdout.write(`Stop requested for ${args.cwd}\n`);
    return 0;
  }

  const existing = await store.read();
  if (existing && (await queryControl(existing))) {
    throw new Error(`A supervisor is already running for ${args.cwd}`);
  }
  if (!process.stdin.isTTY) {
    throw new Error("start requires an interactive terminal");
  }

  const supervisor = new Supervisor(args, store);
  return supervisor.run();
}

function parseArguments(argv: string[]): ParsedArguments {
  let command: ParsedArguments["command"] = "start";
  let index = 0;
  const first = argv[0];
  if (first === "start" || first === "status" || first === "stop") {
    command = first;
    index = 1;
  } else if (first === "help" || first === "--help" || first === "-h") {
    command = "help";
    index = 1;
  }

  let cwd = process.cwd();
  let codexPath = "codex";
  let stateRoot =
    process.env.CODEX_SUPERVISOR_HOME ??
    resolve(process.env.LOCALAPPDATA ?? resolve(homedir(), ".local", "state"), "codex-supervisor");
  let healthUrl: string | undefined;
  let probeModel: string | undefined;
  let probeTimeoutMs = 120_000;
  let probeSuccesses = 2;
  let maxAutoResumes = 5;
  let stallTimeoutMs = 0;
  let stallConfirmMs = 30_000;
  let stallInterruptTimeoutMs = 15_000;
  let maxStallResumes = 2;
  let toolStallTimeoutMs = 0;
  let backoffMs = DEFAULT_BACKOFF;
  let json = false;
  const codexConfig: string[] = [];
  let tuiArgs: string[] = [];

  const valueAfter = (flag: string): string => {
    index += 1;
    const value = argv[index];
    if (!value) {
      throw new Error(`${flag} requires a value`);
    }
    return value;
  };

  for (; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--") {
      tuiArgs = argv.slice(index + 1);
      break;
    }
    switch (arg) {
      case "-C":
      case "--cwd":
        cwd = resolve(valueAfter(arg));
        break;
      case "--codex":
        codexPath = valueAfter(arg);
        break;
      case "--state-dir":
        stateRoot = resolve(valueAfter(arg));
        break;
      case "--health-url":
        healthUrl = valueAfter(arg);
        break;
      case "--probe-model":
        probeModel = valueAfter(arg);
        break;
      case "--probe-timeout-ms":
        probeTimeoutMs = positiveInteger(valueAfter(arg), arg);
        break;
      case "--probe-successes":
        probeSuccesses = positiveInteger(valueAfter(arg), arg);
        break;
      case "--max-auto-resumes":
        maxAutoResumes = positiveInteger(valueAfter(arg), arg);
        break;
      case "--stall-timeout-ms":
        stallTimeoutMs = nonNegativeInteger(valueAfter(arg), arg);
        break;
      case "--stall-confirm-ms":
        stallConfirmMs = positiveInteger(valueAfter(arg), arg);
        break;
      case "--stall-interrupt-timeout-ms":
        stallInterruptTimeoutMs = positiveInteger(valueAfter(arg), arg);
        break;
      case "--max-stall-resumes":
        maxStallResumes = positiveInteger(valueAfter(arg), arg);
        break;
      case "--tool-stall-timeout-ms":
        toolStallTimeoutMs = nonNegativeInteger(valueAfter(arg), arg);
        break;
      case "--backoff-ms":
        backoffMs = valueAfter(arg)
          .split(",")
          .map((value) => positiveInteger(value.trim(), arg));
        break;
      case "-c":
      case "--config":
        codexConfig.push(valueAfter(arg));
        break;
      case "--json":
        json = true;
        break;
      case "-h":
      case "--help":
        command = "help";
        break;
      default:
        throw new Error(`Unknown option ${String(arg)}. Put Codex TUI arguments after --.`);
    }
  }

  return {
    command,
    cwd: resolve(cwd),
    codexPath,
    stateRoot,
    json,
    codexConfig,
    tuiArgs,
    ...(healthUrl ? { healthUrl } : {}),
    ...(probeModel ? { probeModel } : {}),
    probeTimeoutMs,
    probeSuccesses,
    backoffMs,
    maxAutoResumes,
    stallTimeoutMs,
    stallConfirmMs,
    stallInterruptTimeoutMs,
    maxStallResumes,
    toolStallTimeoutMs,
  };
}

function positiveInteger(value: string, flag: string): number {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    throw new Error(`${flag} requires a positive integer`);
  }
  return parsed;
}

function nonNegativeInteger(value: string, flag: string): number {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 0) {
    throw new Error(`${flag} requires a non-negative integer`);
  }
  return parsed;
}

function printHelp(): void {
  process.stdout.write(`Codex Provider Supervisor

Usage:
  codex-supervisor start [options] [-- CODEX_TUI_ARGS...]
  codex-supervisor status [options]
  codex-supervisor stop [options]

Options:
  -C, --cwd DIR               Workspace to open (default: current directory)
  --codex PATH                Codex executable (default: codex)
  -c, --config KEY=VALUE      Codex config override; repeatable
  --health-url URL            Optional cheap health endpoint checked before canaries
  --probe-model MODEL         Optional model override for health canaries
  --probe-timeout-ms MS       Canary timeout (default: 120000)
  --probe-successes N         Successes required before resume (default: 2)
  --backoff-ms LIST           Comma-separated retry delays
  --max-auto-resumes N        Consecutive automatic resume limit (default: 5)
  --stall-timeout-ms MS       Silent-turn timeout; 0 disables it (default: 0)
  --stall-confirm-ms MS       Silence confirmation window (default: 30000)
  --stall-interrupt-timeout-ms MS
                               Interrupt/confirmation RPC timeout (default: 15000)
  --max-stall-resumes N       Consecutive stalled-turn resume limit (default: 2)
  --tool-stall-timeout-ms MS  Silent active-tool timeout; 0 disables it (default: 0)
  --state-dir DIR             State and log directory
  --json                      JSON output for status
  -h, --help                  Show help

Examples:
  codex-supervisor start -C D:\\work\\repo
  codex-supervisor start -C . --stall-timeout-ms 600000
  codex-supervisor start -C . -- --sandbox workspace-write
  codex-supervisor status -C . --json
`);
}

main()
  .then((code) => {
    process.exitCode = code;
  })
  .catch((error) => {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  });
