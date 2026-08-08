import { spawn } from "node:child_process";
import { createServer } from "node:net";
import WebSocket from "ws";
import { JsonRpcClient } from "../dist/json-rpc-client.js";
import { ProviderProbe } from "../dist/provider-probe.js";
import { TuiProxy } from "../dist/tui-proxy.js";

const codexPath = process.env.CODEX_PATH ?? "codex";
const cwd = process.cwd();
let appServer;
let rpc;
let proxy;
let client;
let probe;

try {
  const port = await getFreePort();
  const appServerUrl = `ws://127.0.0.1:${port}`;
  appServer = spawn(codexPath, ["app-server", "--listen", appServerUrl], {
    cwd,
    stdio: "ignore",
    windowsHide: true,
  });
  await waitForReady(port, appServer);

  rpc = new JsonRpcClient(appServerUrl, 30_000);
  await rpc.connect();
  await rpc.initialize();
  await rpc.request("thread/list", { limit: 1 });

  proxy = new TuiProxy(appServerUrl);
  const proxyPort = await proxy.start();
  const observed = new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("Proxy did not observe initialize response")), 10_000);
    proxy.onServerMessage((message) => {
      if (message.id === "smoke-init") {
        clearTimeout(timer);
        resolve();
      }
    });
  });

  client = new WebSocket(`ws://127.0.0.1:${proxyPort}`, { perMessageDeflate: false });
  await new Promise((resolve, reject) => {
    client.once("open", resolve);
    client.once("error", reject);
  });
  client.send(
    JSON.stringify({
      id: "smoke-init",
      method: "initialize",
      params: {
        clientInfo: { name: "codex_supervisor_smoke", version: "0.1.0" },
      },
    }),
  );
  await observed;
  client.send(JSON.stringify({ method: "initialized", params: {} }));
  await proxy.request("thread/list", { limit: 1 });

  if (process.argv.includes("--canary")) {
    probe = new ProviderProbe(rpc, {
      cwd,
      timeoutMs: Number(process.env.CODEX_CANARY_TIMEOUT_MS ?? 120_000),
    });
    const result = await probe.check();
    if (!result.healthy) {
      throw new Error(
        `Provider canary failed: ${result.failure?.code ?? "unknown"}: ${result.failure?.message ?? "unknown error"}`,
      );
    }
    process.stdout.write("Configured provider canary passed.\n");
  }

  process.stdout.write("Codex app-server protocol smoke test passed.\n");
} finally {
  probe?.dispose();
  client?.close();
  rpc?.close();
  await proxy?.close().catch(() => undefined);
  appServer?.kill();
}

async function getFreePort() {
  const server = createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (!address || typeof address === "string") {
    throw new Error("Could not allocate a port");
  }
  await new Promise((resolve) => server.close(resolve));
  return address.port;
}

async function waitForReady(port, child) {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    if (child.exitCode !== null) {
      throw new Error(`Codex app-server exited with code ${child.exitCode}`);
    }
    try {
      const response = await fetch(`http://127.0.0.1:${port}/readyz`, {
        signal: AbortSignal.timeout(1_000),
      });
      if (response.ok) {
        return;
      }
    } catch {
      // The listener may still be starting.
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error("Timed out waiting for Codex app-server");
}
