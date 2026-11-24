import assert from "node:assert/strict";
import test from "node:test";
import { idempotencyKey } from "../lib/api.ts";

test("idempotency keys do not depend on crypto.randomUUID", (t) => {
  // Browsers omit randomUUID outside secure contexts, such as a console served over plain HTTP on a LAN address.
  t.mock.property(Object.getPrototypeOf(globalThis.crypto), "randomUUID", undefined);
  const first = idempotencyKey();
  const second = idempotencyKey();
  assert.match(first, /^[0-9a-f]{32}$/);
  assert.notEqual(first, second);
});
