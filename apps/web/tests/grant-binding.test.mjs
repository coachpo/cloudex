import assert from "node:assert/strict";
import test from "node:test";
import { changeGrantBinding } from "../lib/api.ts";

test("grant binding updates carry the observed revision on bind and unbind", async context => {
  const requests = [];
  context.mock.method(globalThis, "fetch", async (path, init) => {
    requests.push({ path, method: init.method, headers: init.headers, body: init.body });
    return Response.json({ data: { binding_revision: 11 } });
  });

  assert.equal((await changeGrantBinding("workspace/test", "grant-b", 9)).binding_revision, 11);
  assert.equal((await changeGrantBinding("workspace/test", "", 10)).binding_revision, 11);
  assert.equal(requests[0].path, "/api/v1/workspaces/workspace%2Ftest/grant-binding");
  assert.equal(requests[0].method, "PUT");
  assert.equal(requests[0].headers.get("If-Match"), '"9"');
  assert.deepEqual(JSON.parse(requests[0].body), { grant_id: "grant-b" });
  assert.equal(requests[1].method, "DELETE");
  assert.equal(requests[1].headers.get("If-Match"), '"10"');
  assert.equal(requests[1].body, undefined);
});
