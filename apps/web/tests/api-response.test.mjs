import assert from "node:assert/strict";
import test from "node:test";
import { api, APIError, noResponseMessage, readSettings, settingsQuery } from "../lib/api.ts";

test("a non-envelope success response reports a control-plane error", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json("unexpected response"));
  await assert.rejects(api("/settings"), {
    name: "Error",
    message: noResponseMessage,
    status: 200,
  });
});

test("a success envelope with a null or scalar payload reports a control-plane error", async (t) => {
  let data;
  t.mock.method(globalThis, "fetch", async () => Response.json({ data }));
  for (data of [null, "unexpected response", 7, false]) {
    await assert.rejects(api("/settings"), {
      message: noResponseMessage,
      status: 200,
    });
  }
});

test("management responses preserve objects, arrays, and explicit empty acknowledgements", async (t) => {
  const responses = [Response.json({ data: { username: "admin" } }), Response.json({ data: [] }), new Response(null, { status: 204 })];
  t.mock.method(globalThis, "fetch", async () => responses.shift());
  assert.deepEqual(await api("/auth/session"), { username: "admin" });
  assert.deepEqual(await api("/hosts"), []);
  assert.equal(await api("/grants/example", { method: "DELETE" }), undefined);
});

test("the settings query rejects malformed records and can recover on the next read", async (t) => {
  const valid = {
    workspace_image: "workspace:fixture", ssh_host: "", ssh_bind: "127.0.0.1", agent_public_address: "",
    codex_config_toml: 'model = "fixture"\n', codex_agents_md: "# Fixture\n",
  };
  let data;
  const { signal } = new AbortController();
  t.mock.method(globalThis, "fetch", async () => Response.json({ data }));
  for (data of [[], {}, { ...valid, workspace_image: 12 }, { ...valid, ssh_host: null },
    { ...valid, codex_config_toml: null }, { ...valid, codex_agents_md: undefined }]) {
    await assert.rejects(settingsQuery.queryFn({ signal }), { message: noResponseMessage, status: 200 });
    await assert.rejects(readSettings(), { message: noResponseMessage, status: 200 });
  }
  data = valid;
  assert.deepEqual(await settingsQuery.queryFn({ signal }), valid);
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
