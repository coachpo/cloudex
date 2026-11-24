import assert from "node:assert/strict";
import test from "node:test";
import { environmentManager, QueryClient, QueryObserver } from "@tanstack/react-query";
import { APIError, canPlaceOnHost, change, hostListQuery, repositoryCredentialPath, saveSetting, settingsQuery,
  templatesQuery, workspaceImageDraft } from "../lib/api.ts";

test("a delayed settings acknowledgement cannot replace a newer read", async (t) => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const queryKey = ["host", "local"];
  client.setQueryData(queryKey, { max_operations_target: 3 });
  const acknowledgement = Promise.withResolvers();
  let reads = 0;
  const observer = new QueryObserver(client, {
    queryKey,
    queryFn: async () => {
      if (++reads === 1) return { max_operations_target: 8 };
      throw new APIError(503, "Temporarily unavailable");
    },
  });
  const unsubscribe = observer.subscribe(() => {});
  t.after(() => { unsubscribe(); client.clear(); });

  const save = saveSetting(client, queryKey, () => acknowledgement.promise);
  await observer.refetch();
  acknowledgement.resolve({ max_operations_target: 7 });
  assert.equal(await save, true);
  assert.equal(reads, 2);
  assert.equal(client.getQueryState(queryKey).status, "error");
  assert.deepEqual(client.getQueryData(queryKey), { max_operations_target: 8 });
});

test("a lost settings reply refreshes the query while a confirmed rejection preserves it", async (t) => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const queryKey = ["settings"];
  client.setQueryData(queryKey, { workspace_image: "workspace:old" });
  let reads = 0;
  const observer = new QueryObserver(client, {
    queryKey,
    queryFn: async () => {
      reads++;
      return { workspace_image: "workspace:new" };
    },
  });
  const unsubscribe = observer.subscribe(() => {});
  t.after(() => { unsubscribe(); client.clear(); });

  assert.equal(await saveSetting(client, queryKey, async () => { throw new TypeError("Failed to fetch"); }), false);
  assert.equal(reads, 1);
  assert.deepEqual(client.getQueryData(queryKey), { workspace_image: "workspace:new" });

  const invalid = new APIError(400, "Invalid address", "INVALID_SSH_HOST");
  await assert.rejects(saveSetting(client, queryKey, async () => { throw invalid; }), invalid);
  assert.equal(reads, 1);
});

test("open console views observe an updated default image without navigating", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  environmentManager.setIsServer(() => false);
  let image = "workspace:before";
  t.mock.method(globalThis, "fetch", async (path) => {
    const data = {
      "/api/v1/settings": { workspace_image: image },
      "/api/v1/templates": [{ image }],
      "/api/v1/hosts": [{ status: "online", scheduling_state: "enabled", images: [{ image, ready: true }] }],
    }[path];
    return { ok: true, json: async () => ({ data }) };
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
  const settings = new QueryObserver(client, settingsQuery);
  const templates = new QueryObserver(client, templatesQuery);
  const hosts = new QueryObserver(client, hostListQuery);
  const active = [settings, templates, hosts];
  const unsubscribers = active.map(observer => observer.subscribe(() => {}));
  t.after(() => {
    for (const unsubscribe of unsubscribers) unsubscribe();
    client.clear();
    environmentManager.setIsServer(() => true);
  });
  await Promise.all(active.map(observer => observer.getCurrentQuery().promise));
  assert.equal(templates.getCurrentResult().data[0].image, image);

  // Another client saves a new default and the Host reports it on heartbeat.
  image = "workspace:after";
  t.mock.timers.tick(5000);
  await Promise.all(active.map(observer => observer.getCurrentQuery().promise));
  const template = templates.getCurrentResult().data[0];
  assert.equal(template.image, image);
  assert.equal(settings.getCurrentResult().data.workspace_image, image);
  assert.ok(canPlaceOnHost(hosts.getCurrentResult().data[0], template.image));
});

test("whitespace around the saved image does not retain a draft over later settings", () => {
  const saved = "workspace:initial";
  const later = "workspace:later";
  const draft = workspaceImageDraft(` ${saved} `, saved);
  assert.equal(draft ?? later, later);
  const edited = workspaceImageDraft(` ${later} `, saved);
  assert.equal(edited, ` ${later} `);
  assert.equal(workspaceImageDraft(edited, later), undefined);
});


test("deleting a saved repository credential accepts an empty 204 reply", async (t) => {
  const path = repositoryCredentialPath("workspace");
  t.mock.method(globalThis, "fetch", async (url, init) => {
    assert.equal(url, "/api/v1" + path);
    assert.equal(init.method, "DELETE");
    return new Response(null, { status: 204 });
  });
  assert.equal(await change(path, "DELETE"), undefined);
});
