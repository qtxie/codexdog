export type JsonRpcId = number | string;

export interface JsonRpcMessage {
  id?: JsonRpcId;
  method?: string;
  params?: Record<string, unknown>;
  result?: unknown;
  error?: unknown;
}

export interface TurnError {
  message: string;
  codexErrorInfo?: unknown;
  additionalDetails?: string | null;
}

export interface Turn {
  id: string;
  status: "completed" | "failed" | "inProgress" | "interrupted";
  error?: TurnError | null;
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function parseJsonRpc(value: string): JsonRpcMessage | undefined {
  try {
    const parsed: unknown = JSON.parse(value);
    return isRecord(parsed) ? (parsed as JsonRpcMessage) : undefined;
  } catch {
    return undefined;
  }
}

export function readString(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

export function readBoolean(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

export function readNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

export function readTurn(value: unknown): Turn | undefined {
  if (!isRecord(value)) {
    return undefined;
  }

  const id = readString(value.id);
  const status = readString(value.status);
  if (
    !id ||
    (status !== "completed" &&
      status !== "failed" &&
      status !== "inProgress" &&
      status !== "interrupted")
  ) {
    return undefined;
  }

  const errorValue = value.error;
  let error: TurnError | null | undefined;
  if (errorValue === null) {
    error = null;
  } else if (isRecord(errorValue) && typeof errorValue.message === "string") {
    error = {
      message: errorValue.message,
      ...(errorValue.codexErrorInfo !== undefined
        ? { codexErrorInfo: errorValue.codexErrorInfo }
        : {}),
      ...(typeof errorValue.additionalDetails === "string" || errorValue.additionalDetails === null
        ? { additionalDetails: errorValue.additionalDetails }
        : {}),
    };
  }

  return { id, status, ...(error !== undefined ? { error } : {}) };
}

export function readTurnError(value: unknown): TurnError | undefined {
  if (!isRecord(value) || typeof value.message !== "string") {
    return undefined;
  }

  return {
    message: value.message,
    ...(value.codexErrorInfo !== undefined ? { codexErrorInfo: value.codexErrorInfo } : {}),
    ...(typeof value.additionalDetails === "string" || value.additionalDetails === null
      ? { additionalDetails: value.additionalDetails }
      : {}),
  };
}
