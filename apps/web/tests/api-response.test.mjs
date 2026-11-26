import assert from "node:assert/strict";
import test from "node:test";
import { api, APIError, noResponseMessage } from "../lib/api.ts";

test("a non-envelope success response reports a control-plane error", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json("unexpected response"));
  await assert.rejects(api("/settings"), {
    name: "Error",
    message: noResponseMessage,
    status: 200,
  });
});

test("an API rejection preserves its HTTP status and problem code", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json({
    error: { message: "Sign in to continue.", code: "UNAUTHORIZED" },
  }, { status: 401 }));
  await assert.rejects(api("/auth/session"), {
    message: "Sign in to continue.",
    status: 401,
    code: "UNAUTHORIZED",
  });
  await assert.rejects(api("/auth/session"), APIError);
});
