import { appendFile, rename, stat, unlink } from "node:fs/promises";

const MAX_LOG_BYTES = 2 * 1024 * 1024;
const ROTATIONS = 3;

export function sanitizeText(value: string, maxLength = 1_000): string {
  return value
    .replace(/(authorization\s*[:=]\s*bearer\s+)[^\s,;]+/gi, "$1[REDACTED]")
    .replace(/((?:api[-_ ]?key|token)\s*[:=]\s*)[^\s,;]+/gi, "$1[REDACTED]")
    .slice(0, maxLength);
}

export class Logger {
  private writeChain = Promise.resolve();

  constructor(private readonly path: string) {}

  async initialize(): Promise<void> {
    try {
      const info = await stat(this.path);
      if (info.size < MAX_LOG_BYTES) {
        return;
      }
    } catch {
      return;
    }

    await unlink(`${this.path}.${ROTATIONS}`).catch(() => undefined);
    for (let index = ROTATIONS - 1; index >= 1; index -= 1) {
      await rename(`${this.path}.${index}`, `${this.path}.${index + 1}`).catch(() => undefined);
    }
    await rename(this.path, `${this.path}.1`).catch(() => undefined);
  }

  log(message: string): void {
    const line = `${new Date().toISOString()} ${sanitizeText(message)}\n`;
    this.writeChain = this.writeChain
      .then(() => appendFile(this.path, line, "utf8"))
      .catch(() => undefined);
  }
}
