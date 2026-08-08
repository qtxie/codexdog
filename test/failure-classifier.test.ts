import { describe, expect, it } from "vitest";
import { classifyFailure } from "../src/failure-classifier.js";

describe("classifyFailure", () => {
  it("recovers upstream 5xx responses", () => {
    expect(
      classifyFailure({
        message: "upstream unavailable",
        codexErrorInfo: { httpConnectionFailed: { httpStatusCode: 503 } },
      }),
    ).toMatchObject({ disposition: "transient", code: "httpConnectionFailed", httpStatus: 503 });
  });

  it("recovers stream disconnects without an HTTP status", () => {
    expect(
      classifyFailure({
        message: "stream ended",
        codexErrorInfo: { responseStreamDisconnected: {} },
      }).disposition,
    ).toBe("transient");
  });

  it("does not retry provider authentication failures", () => {
    expect(
      classifyFailure({
        message: "unauthorized",
        codexErrorInfo: { httpConnectionFailed: { httpStatusCode: 401 } },
      }).disposition,
    ).toBe("permanent");
    expect(
      classifyFailure({ message: "login expired", codexErrorInfo: "unauthorized" }).disposition,
    ).toBe("permanent");
  });

  it("treats overload and timeout messages as transient", () => {
    expect(
      classifyFailure({ message: "overloaded", codexErrorInfo: "serverOverloaded" }).disposition,
    ).toBe("transient");
    expect(classifyFailure({ message: "request timed out" }).disposition).toBe("transient");
  });

  it("fails closed for unknown errors", () => {
    expect(classifyFailure({ message: "unexpected model response" }).disposition).toBe("permanent");
  });
});
