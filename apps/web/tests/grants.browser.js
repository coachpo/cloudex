// Run with Playwright on a local Cloudex page. Both OAuth and control-plane responses are fixtures.
async page => {
  page.setDefaultTimeout(8000);
  const origin = await page.evaluate(() => window.location.origin);
  const base = origin + "/api/v1";
  const timestamp = "2026-11-27T00:00:00Z";
  const w1 = {
    id: "w1", name: "Workspace One", image: "workspace:fixture", repo_url: "",
    branch: "main", cpu_milli: 1000, memory_mb: 512, disk_bytes: 0, assigned_host_id: "test-host",
    desired_state: "running", observed_state: "running", paused: false, generation: 1, error: "", updated_at: timestamp,
  };
  const w2 = { ...w1, id: "w2", name: "Workspace Two" };
  const a = {
    id: "a", channel: "codex", display_name: "Test Codex", email: "fixture@example.test",
    subject: "codex-subject", client_id: "codex-client", upstream_user_id: "test-codex-user",
    upstream_workspace_id: "test-codex-workspace", status: "active", workspaces: [w1, w2], grant_epoch: 1,
  };
  const b = {
    id: "b", channel: "chatgpt", display_name: "Test ChatGPT", email: a.email,
    subject: "chatgpt-subject", client_id: "issued-fixture-client",
    status: "active", workspaces: [], grant_epoch: 1,
  };
  const fixtures = {
    "/auth/session": { username: "fixture" },
    "/settings": { workspace_image: "workspace:fixture", ssh_host: "", ssh_bind: "127.0.0.1", agent_public_address: "" },
    "/settings/ssh-keys": { public_keys: [] },
    "/hosts": [],
    "/hosts/test-host": { id: "test-host", name: "Fixture", reachable: true, healthy: true },
    "/workspaces": [w1, w2],
    "/workspaces/w1": w1,
    "/workspaces/w1/operations": [],
  };
  const grants = [a];
  let binding = { workspace_id: w1.id, grant_id: a.id, binding_revision: 8, readiness: "ready" };
  let started, attemptNumber = 0, callbackReturned = false, confirmed = 0, revoked = 0, rotated = 0, removed = 0;
  let conflict = true;
  const requestedRevisions = [];
  const expect = (condition, message) => { if (!condition) throw new Error(message); };
  const ok = (route, data) => route.fulfill({ json: { data } });
  const fail = (route, status, code, message, details) => route.fulfill({ status, json: { error: { code, message, details } } });
  const context = page.context();
  const provider = origin + "/fake-provider-login";
  await context.route(provider, route => {
    callbackReturned = true;
    return route.fulfill({ body: "Local OAuth fixture" });
  });
  const pattern = base + "/**";
  await context.route(pattern, route => {
    const request = route.request();
    const path = request.url().slice(base.length);
    if (path === "/grants") return ok(route, { grants });
    if (path === "/grant-authorizations" && request.method() === "POST") {
      started = request.postDataJSON();
      callbackReturned = false;
      return ok(route, { id: "attempt-" + ++attemptNumber, authorization_url: provider });
    }
    if (path === "/grant-authorizations/attempt-" + attemptNumber) {
      const id = "attempt-" + attemptNumber;
      return ok(route, !callbackReturned ? { id, status: "pending" } : started.grant_id
        ? { id, status: "failed", code: "oauth_identity_mismatch" }
        : { id, status: "ready", identity: b });
    }
    if (path === "/grant-authorizations/attempt-1/confirm" && request.method() === "POST") {
      confirmed++;
      grants.push(b);
      return ok(route, b);
    }
    if (path === "/grants/a/disconnect" && request.method() === "POST") {
      revoked++;
      a.status = "disconnected";
      a.reason = "remote_revocation_unconfirmed";
      return ok(route, a);
    }
    if (path === "/grants/b" && request.method() === "DELETE") {
      removed++;
      grants.splice(grants.indexOf(b), 1);
      return route.fulfill({ status: 204 });
    }
    if (path === "/workspaces/w1/grant-binding") {
      if (request.method() === "GET") return ok(route, binding);
      const ifMatch = request.headers()["if-match"];
      requestedRevisions.push(ifMatch);
      if (conflict) {
        conflict = false;
        binding.binding_revision++;
        return fail(route, 409, "binding_conflict", "Binding revision changed.", { binding_revision: binding.binding_revision });
      }
      expect(ifMatch === '"' + binding.binding_revision + '"', "The console did not submit the current binding revision");
      const previous = grants.find(grant => grant.id === binding.grant_id);
      previous.workspaces = previous.workspaces.filter(workspace => workspace.id !== w1.id);
      const grantID = request.method() === "DELETE" ? "" : request.postDataJSON().grant_id;
      if (grantID) grants.find(grant => grant.id === grantID).workspaces.push(w1);
      binding = { ...binding, grant_id: grantID, binding_revision: binding.binding_revision + 1, readiness: grantID ? "pending" : "unbound" };
      return ok(route, binding);
    }
    if (path === "/workspaces/w1/gateway-credential/rotate" && request.method() === "POST") {
      rotated++;
      return ok(route, binding);
    }
    return path in fixtures ? ok(route, fixtures[path]) : fail(route, 404, "NOT_FOUND", path);
  });
  try {
    await page.goto("about:blank");
    await page.setExtraHTTPHeaders({ "accept-language": "en-US,en;q=0.9" });
    await page.goto(origin + "/settings?fixture=grants#grants");
    const settings = page.getByRole("region", { name: "Codex authorizations", exact: true });
    const cardA = settings.getByRole("article", { name: a.display_name });
    await cardA.getByRole("link", { name: w2.name }).waitFor();
    await cardA.getByText(a.upstream_workspace_id, { exact: true }).waitFor();
    await page.setViewportSize({ width: 320, height: 720 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= 320), "The grant list overflowed a narrow viewport");
    await page.setViewportSize({ width: 1100, height: 720 });

    async function completeFakeSignIn() {
      const [login] = await Promise.all([
        page.waitForEvent("popup"),
        settings.getByRole("link", { name: "Open sign-in in a new tab", exact: true }).click(),
      ]);
      await login.getByText("Local OAuth fixture", { exact: true }).waitFor();
      await login.close();
    }
    await settings.getByRole("button", { name: "Sign in with ChatGPT", exact: true }).click();
    await completeFakeSignIn();
    await settings.getByRole("heading", { name: "Confirm the signed-in account", exact: true }).waitFor();
    await settings.getByText(b.client_id, { exact: true }).waitFor();
    expect(started.channel === "chatgpt" && !started.grant_id && confirmed === 0, "The new authorization did not require confirmation");
    await settings.getByRole("button", { name: "Save authorization", exact: true }).click();
    await settings.getByRole("article", { name: b.display_name }).waitFor();
    expect(confirmed === 1 && a.workspaces.length === 2 && b.workspaces.length === 0, "Adding a grant changed a binding");

    await cardA.getByRole("button", { name: "Reauthorize", exact: true }).click();
    await completeFakeSignIn();
    await settings.getByRole("alert").filter({ hasText: "The signed-in account does not match" }).waitFor();
    expect(started.channel === "codex" && started.grant_id === a.id && confirmed === 1, "Reauthorization used the wrong grant or saved a mismatched identity");
    await settings.getByRole("button", { name: "Cancel", exact: true }).click();

    await Promise.all([
      page.waitForURL(origin + "/workspaces/w1/settings#workspace-grant"),
      cardA.getByRole("link", { name: w1.name }).click(),
    ]);
    const workspace = page.getByRole("region", { name: "Codex gateway", exact: true });
    const select = workspace.getByLabel("Choose authorization", { exact: true });
    const apply = workspace.getByRole("button", { name: "Apply authorization", exact: true });
    await select.selectOption(b.id);
    await workspace.getByText(/Changing the binding ends gateway connections/).waitFor();
    await apply.click();
    await workspace.getByText(/The binding changed while you were editing/).waitFor();
    expect(await select.inputValue() === a.id && await apply.isDisabled(), "The console attempted to overwrite an unseen revision");

    await select.selectOption(b.id);
    await apply.click();
    await workspace.getByText("Waiting for the container", { exact: true }).waitFor();
    expect(requestedRevisions.join(",") === '"8","9"' && a.workspaces.map(workspace => workspace.id).join(",") === w2.id && b.workspaces.map(workspace => workspace.id).join(",") === w1.id,
      "The console did not apply the new binding to this workspace only");
    binding.readiness = "ready";
    const rotate = workspace.getByRole("button", { name: "Rotate container credential", exact: true });
    await rotate.click();
    let dialog = page.getByRole("dialog", { name: "Rotate the container credential?" });
    await dialog.waitFor();
    expect(await dialog.getByRole("button", { name: "Cancel", exact: true }).evaluate(button => button === document.activeElement), "The safe action did not receive dialog focus");
    await page.keyboard.press("Escape");
    expect(await rotate.evaluate(button => button === document.activeElement) && rotated === 0, "Closing the dialog did not restore focus or prevented cancellation");
    await rotate.click();
    await dialog.getByRole("button", { name: "Rotate container credential", exact: true }).click();
    await workspace.getByText("Credential rotation requested.", { exact: true }).waitFor();
    expect(rotated === 1, "Rotation was not requested exactly once");

    await select.selectOption("");
    await apply.click();
    await workspace.getByText("Workspace authorization cleared.", { exact: true }).waitFor();
    expect(binding.grant_id === "" && b.workspaces.length === 0, "Unbinding did not clear the grant");

    await workspace.getByRole("link", { name: "Manage authorizations" }).click();
    await cardA.getByRole("button", { name: "Disconnect", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "Disconnect Test Codex?" });
    await dialog.getByRole("link", { name: w2.name }).waitFor();
    expect(await dialog.getByRole("button", { name: "Cancel", exact: true }).evaluate(button => button === document.activeElement),
      "The affected workspace link displaced safe initial dialog focus");
    await dialog.getByRole("button", { name: "Disconnect", exact: true }).click();
    await cardA.getByText("Local access is blocked. Upstream revocation could not be confirmed.", { exact: true }).waitFor();
    expect(revoked === 1 && a.workspaces.map(workspace => workspace.id).join(",") === w2.id, "Disconnect changed the workspace binding");
    const cardB = settings.getByRole("article", { name: b.display_name });
    await cardB.getByRole("button", { name: "Delete authorization", exact: true }).click();
    dialog = page.getByRole("dialog", { name: "Delete the authorization for Test ChatGPT?" });
    await dialog.getByRole("button", { name: "Delete authorization", exact: true }).click();
    await cardB.waitFor({ state: "hidden" });
    expect(removed === 1 && grants[0] === a, "Deleting the unbound grant also affected the other channel");
    return { confirmed, requestedRevisions, rotated, revoked, removed };
  } finally {
    await context.unroute(pattern);
    await context.unroute(provider);
  }
}
