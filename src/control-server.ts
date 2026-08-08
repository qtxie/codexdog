import { randomBytes, timingSafeEqual } from "node:crypto";
import { createServer, type Server } from "node:http";
import type { StateStore, SupervisorState } from "./state-store.js";

export interface ControlServerHandle {
  port: number;
  token: string;
  close: () => Promise<void>;
}

function authorized(header: string | undefined, token: string): boolean {
  const supplied = header?.startsWith("Bearer ") ? header.slice(7) : "";
  const left = Buffer.from(supplied);
  const right = Buffer.from(token);
  return left.length === right.length && timingSafeEqual(left, right);
}

export async function startControlServer(
  store: StateStore,
  state: () => SupervisorState,
  stop: () => void,
): Promise<ControlServerHandle> {
  const token = randomBytes(32).toString("base64url");
  let server: Server;
  server = createServer((request, response) => {
    if (!authorized(request.headers.authorization, token)) {
      response.writeHead(401).end();
      return;
    }

    if (request.method === "GET" && request.url === "/status") {
      response.writeHead(200, { "content-type": "application/json" });
      response.end(JSON.stringify(state()));
      return;
    }

    if (request.method === "POST" && request.url === "/stop") {
      response.writeHead(202).end();
      setImmediate(stop);
      return;
    }

    response.writeHead(404).end();
  });

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  if (!address || typeof address === "string") {
    throw new Error("Could not determine control server port");
  }

  await store.initialize();
  return {
    port: address.port,
    token,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

export async function queryControl(state: SupervisorState): Promise<boolean> {
  if (!state.controlPort || !state.controlToken) {
    return false;
  }
  try {
    const response = await fetch(`http://127.0.0.1:${state.controlPort}/status`, {
      headers: { authorization: `Bearer ${state.controlToken}` },
      signal: AbortSignal.timeout(1_000),
    });
    return response.ok;
  } catch {
    return false;
  }
}

export async function requestStop(state: SupervisorState): Promise<boolean> {
  if (!state.controlPort || !state.controlToken) {
    return false;
  }
  try {
    const response = await fetch(`http://127.0.0.1:${state.controlPort}/stop`, {
      method: "POST",
      headers: { authorization: `Bearer ${state.controlToken}` },
      signal: AbortSignal.timeout(2_000),
    });
    return response.ok;
  } catch {
    return false;
  }
}
