import { isRecord, readNumber, type TurnError } from "./protocol.js";

export type FailureDisposition = "transient" | "permanent";

export interface ClassifiedFailure {
  disposition: FailureDisposition;
  code: string;
  httpStatus?: number;
  message: string;
}

const TRANSIENT_STRING_CODES = new Set([
  "serverOverloaded",
  "internalServerError",
]);

const PERMANENT_STRING_CODES = new Set([
  "activeTurnNotSteerable",
  "badRequest",
  "contextWindowExceeded",
  "cyberPolicy",
  "other",
  "sandboxError",
  "sessionBudgetExceeded",
  "threadRollbackFailed",
  "unauthorized",
  "usageLimitExceeded",
]);

const TRANSIENT_OBJECT_CODES = new Set([
  "httpConnectionFailed",
  "responseStreamConnectionFailed",
  "responseStreamDisconnected",
  "responseTooManyFailedAttempts",
]);

const PERMANENT_MESSAGE =
  /\b(unauthori[sz]ed|forbidden|invalid api key|authentication|bad request|context window|usage limit|quota exhausted|sandbox)\b/i;
const TRANSIENT_MESSAGE =
  /\b(timed?\s*out|timeout|connection (?:closed|failed|refused|reset)|network error|dns|tls|socket|stream disconnected|temporarily unavailable|server overloaded|too many requests|http\s*(?:408|425|429|5\d\d))\b/i;

function isTransientStatus(status: number | undefined): boolean {
  return status === undefined || status === 408 || status === 425 || status === 429 || status >= 500;
}

function objectCode(info: Record<string, unknown>): {
  code: string;
  httpStatus?: number;
} | undefined {
  for (const [code, value] of Object.entries(info)) {
    if (!TRANSIENT_OBJECT_CODES.has(code) && !PERMANENT_STRING_CODES.has(code)) {
      continue;
    }

    const status = isRecord(value) ? readNumber(value.httpStatusCode) : undefined;
    return { code, ...(status !== undefined ? { httpStatus: status } : {}) };
  }
  return undefined;
}

export function classifyFailure(error: TurnError): ClassifiedFailure {
  const message = error.message || "Codex turn failed";
  const info = error.codexErrorInfo;

  if (typeof info === "string") {
    if (TRANSIENT_STRING_CODES.has(info)) {
      return { disposition: "transient", code: info, message };
    }
    if (PERMANENT_STRING_CODES.has(info)) {
      return { disposition: "permanent", code: info, message };
    }
  }

  if (isRecord(info)) {
    const parsed = objectCode(info);
    if (parsed) {
      const transient = TRANSIENT_OBJECT_CODES.has(parsed.code) && isTransientStatus(parsed.httpStatus);
      return {
        disposition: transient ? "transient" : "permanent",
        code: parsed.code,
        ...(parsed.httpStatus !== undefined ? { httpStatus: parsed.httpStatus } : {}),
        message,
      };
    }
  }

  if (PERMANENT_MESSAGE.test(message)) {
    return { disposition: "permanent", code: "messageMatch", message };
  }
  if (TRANSIENT_MESSAGE.test(message)) {
    return { disposition: "transient", code: "messageMatch", message };
  }

  return { disposition: "permanent", code: "unclassified", message };
}

export function formatFailure(failure: ClassifiedFailure): string {
  const status = failure.httpStatus === undefined ? "" : ` HTTP ${failure.httpStatus}`;
  return `${failure.code}${status}: ${failure.message}`;
}
