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
  const deviceGrant = { ...a, id: "c", display_name: "Test Device Code", subject: "device-subject",
    upstream_user_id: "test-device-user", workspaces: [] };
  const manualCodexGrant = { ...a, id: "d", display_name: "Test Pasted Callback", subject: "manual-subject",
    upstream_user_id: "test-manual-user", workspaces: [] };
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
  let deviceConfirmed = 0, deviceCancelled = 0;
  let expireOnConfirm = false, conflictOnConfirm = false, confirmationUnavailable = false;
  const expiringIdentityName = "Identity awaiting an expired save";
  const duplicateGrantError = "This identity already has a grant for this registration and workspace.";
  let releaseConflictRead;
  const conflictReadReleased = new Promise(resolve => { releaseConflictRead = resolve; });
  let manualResult;
  const manualConfirmed = [];
  const manualURLs = {
    codex: "http://127.0.0.1:1455/auth/callback?code=codex-fixture-code&state=codex-fixture-state",
    chatgpt: "http://127.0.0.1:1455/auth/callback?code=chatgpt-fixture-code&state=chatgpt-fixture-state&client_id=issued-fixture-client",
  };
  let conflict = true;
  const requestedRevisions = [];
  const expect = (condition, message) => { if (!condition) throw new Error(message); };
  const ok = (route, data) => route.fulfill({ json: { data } });
  const fail = (route, status, code, message, details) => route.fulfill({ status, json: { error: { code, message, details } } });
  const context = page.context();
  const provider = origin + "/fake-provider-login";
  const verification = "https://device-verification.example.test/codex/device";
  const userCode = "TEST-1234";
  await context.route(provider, route => {
    callbackReturned = true;
    return route.fulfill({ body: "Local OAuth fixture" });
  });
  await context.route(verification, route => {
    const request = route.request();
    if (request.method() === "POST") {
      callbackReturned = new URLSearchParams(request.postData()).get("user_code") === userCode;
      return route.fulfill({ body: callbackReturned ? "Device authorization accepted" : "Device code rejected" });
    }
    return route.fulfill({ contentType: "text/html", body: `<!doctype html><title>Device verification fixture</title>
      <form method="post"><label>Enter the user code <input name="user_code"></label><button>Authorize</button></form>` });
  });
  const pattern = base + "/**";
  await context.route(pattern, route => {
    const request = route.request();
    const path = request.url().slice(base.length);
    if (path === "/grants") return ok(route, { grants });
    if (path === "/grant-authorizations" && request.method() === "POST") {
      started = request.postDataJSON();
      callbackReturned = false;
      confirmationUnavailable = false;
      manualResult = undefined;
      const id = "attempt-" + ++attemptNumber;
      return ok(route, started.method === "device_code"
        ? { id, device: { verification_url: verification, user_code: userCode } }
        : { id, authorization_url: provider });
    }
    if (path === "/grant-authorizations/attempt-" + attemptNumber) {
      const id = "attempt-" + attemptNumber;
      const cancelDevice = request.method() === "DELETE" && started.method === "device_code";
      if (cancelDevice) deviceCancelled++;
      if (confirmationUnavailable) {
        const missing = () => fail(route, 404, "NOT_FOUND", "The requested resource was not found.");
        return conflictOnConfirm ? conflictReadReleased.then(missing) : missing();
      }
      if (cancelDevice) return route.fulfill({ status: 204 });
      if (manualResult) return ok(route, { id, status: "ready", identity: manualResult });
      if (!callbackReturned) return ok(route, { id, status: "pending" });
      if (started.grant_id) return ok(route, { id, status: "failed", code: "oauth_identity_mismatch" });
      const identity = started.method === "device_code" ? deviceGrant : b;
      return ok(route, { id, status: "ready", identity: expireOnConfirm ? { ...identity, display_name: expiringIdentityName } : identity });
    }
    if (path === "/grant-authorizations/attempt-" + attemptNumber + "/callback" && request.method() === "POST") {
      const { callback_url: callbackURL } = request.postDataJSON();
      if (callbackURL !== manualURLs[started.channel]) {
        return fail(route, 400, "invalid_oauth_state", "Rejected callback " + callbackURL);
      }
      manualResult = started.channel === "codex" ? manualCodexGrant : b;
      return route.fulfill({ status: 204 });
    }
    if ((expireOnConfirm || conflictOnConfirm) && path === "/grant-authorizations/attempt-" + attemptNumber + "/confirm" && request.method() === "POST") {
      confirmationUnavailable = true;
      if (conflictOnConfirm) return fail(route, 409, "grant_exists", duplicateGrantError);
      return fail(route, 404, "NOT_FOUND", "The requested resource was not found.");
    }
    if (path === "/grant-authorizations/attempt-1/confirm" && request.method() === "POST") {
      confirmed++;
      grants.push(b);
      return ok(route, b);
    }
    if (path === "/grant-authorizations/attempt-" + attemptNumber + "/confirm" && request.method() === "POST" && started.method === "device_code") {
      deviceConfirmed++;
      grants.push(deviceGrant);
      return ok(route, deviceGrant);
    }
    if (path === "/grant-authorizations/attempt-" + attemptNumber + "/confirm" && request.method() === "POST" && manualResult) {
      manualConfirmed.push(started.channel);
      if (started.grant_id) manualResult.grant_epoch++;
      else grants.push(manualResult);
      return ok(route, manualResult);
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
    const chatGPTSignIn = settings.getByRole("button", { name: "Sign in with ChatGPT", exact: true });
    const loopbackInstructions = settings.getByText(/connect that browser’s loopback port/);
    await chatGPTSignIn.click();
    await loopbackInstructions.waitFor();
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

    const deviceSignIn = settings.getByRole("button", { name: "Sign in with Codex using a device code", exact: true });
    await deviceSignIn.click();
    const verificationLink = settings.getByRole("link", { name: "Copy code and open verification page", exact: true });
    await verificationLink.waitFor();
    expect(started.channel === "codex" && started.method === "device_code" && !started.grant_id, "The device entry did not select Codex Device Code");
    const pasteCallback = settings.getByText("Paste callback URL", { exact: true });
    expect(await pasteCallback.count() === 0, "Device sign-in incorrectly offered callback pasting");
    expect(await verificationLink.getAttribute("href") === verification, "Device sign-in did not open the upstream verification page");
    const code = settings.getByText(userCode, { exact: true });
    await code.waitFor();
    expect(await loopbackInstructions.count() === 0, "Device sign-in showed loopback instructions");
    await page.setViewportSize({ width: 320, height: 720 });
    const deviceLayout = await page.evaluate(() => ({ viewport: innerWidth, pageWidth: document.documentElement.scrollWidth }));
    expect(deviceLayout.pageWidth <= deviceLayout.viewport, "The device prompt overflowed a narrow viewport");
    await page.setViewportSize({ width: 1100, height: 720 });
    await code.click();
    const selectedCode = await page.evaluate(() => window.getSelection()?.toString());
    expect(selectedCode === userCode, "Clicking the device code did not select it in full");

    const copied = settings.getByRole("status").filter({ hasText: /^Copied$/ });
    const copyDeviceCode = settings.getByRole("button", { name: "Copy device code", exact: true });
    async function readClipboard(tab) {
      await context.grantPermissions(["clipboard-read"], { origin: new URL(tab.url()).origin });
      try {
        return await tab.evaluate(() => navigator.clipboard.readText());
      } finally {
        await context.clearPermissions();
      }
    }
    const manualCopy = settings.getByRole("status").filter({ hasText: "Select the device code to copy it manually." });
    const clipboardFixture = await page.evaluateHandle(() => {
      const clipboard = navigator.clipboard;
      const writeText = clipboard.writeText;
      const rejection = () => new DOMException("Fixture write rejected", "NotAllowedError");
      let rejectPending;
      return {
        deny: () => { clipboard.writeText = () => Promise.reject(rejection()); },
        delayNext: () => {
          clipboard.writeText = () => {
            clipboard.writeText = writeText;
            return new Promise((_, reject) => { rejectPending = reject; });
          };
        },
        rejectDelayed: async () => {
          rejectPending(rejection());
          // Allow the clipboard promise and the resulting React render to settle.
          await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        },
        restore: () => { clipboard.writeText = writeText; },
      };
    });
    let buttonClipboard, manualCopyText, overlappingCopyText;
    try {
      await clipboardFixture.evaluate(fixture => fixture.delayNext());
      await copyDeviceCode.click();
      await copyDeviceCode.click();
      await copied.waitFor();
      buttonClipboard = await readClipboard(page);
      expect(buttonClipboard === userCode, "The copy button did not write the device code");
      await clipboardFixture.evaluate(fixture => fixture.rejectDelayed());
      overlappingCopyText = (await copyDeviceCode.locator("..").getByRole("status").textContent()).trim();
      expect(overlappingCopyText === "Copied", "A delayed clipboard rejection replaced a newer copy confirmation: " + overlappingCopyText);
      await clipboardFixture.evaluate(fixture => fixture.deny());
      await copyDeviceCode.click();
      await manualCopy.waitFor();
      manualCopyText = (await manualCopy.textContent()).trim();
    } finally {
      await clipboardFixture.evaluate(fixture => fixture.restore());
      await clipboardFixture.dispose();
    }

    let linkClipboard, verificationURL;
    async function completeFakeDeviceSignIn() {
      await context.grantPermissions(["clipboard-write"], { origin });
      try {
        await page.evaluate(() => navigator.clipboard.writeText("before opening verification"));
      } finally {
        await context.clearPermissions();
      }
      const [deviceVerification] = await Promise.all([page.waitForEvent("popup"), verificationLink.click()]);
      const verificationInput = deviceVerification.getByLabel("Enter the user code");
      await verificationInput.waitFor();
      verificationURL = deviceVerification.url();
      expect(verificationURL === verification, "The new tab did not navigate to the device verification URL");
      await copied.waitFor();
      linkClipboard = await readClipboard(deviceVerification);
      expect(linkClipboard === userCode, "Opening the verification page did not write the device code");
      await verificationInput.fill(linkClipboard);
      await deviceVerification.getByRole("button", { name: "Authorize" }).click();
      await deviceVerification.getByText("Device authorization accepted", { exact: true }).waitFor();
      await deviceVerification.close();
    }
    await completeFakeDeviceSignIn();
    await settings.getByRole("heading", { name: "Confirm the signed-in account", exact: true }).waitFor();
    await settings.getByText(deviceGrant.upstream_user_id, { exact: true }).waitFor();
    expect(deviceConfirmed === 0 && await code.count() === 0, "Device sign-in bypassed confirmation or kept showing the used code");
    await settings.getByRole("button", { name: "Save authorization", exact: true }).click();
    await settings.getByRole("article", { name: deviceGrant.display_name }).waitFor();

    const deviceReauthorize = { name: "Reauthorize with a device code", exact: true };
    expect(await settings.getByRole("article", { name: b.display_name }).getByRole("button", deviceReauthorize).count() === 0, "ChatGPT incorrectly offered device-code reauthorization");
    await cardA.getByRole("button", deviceReauthorize).click();
    await verificationLink.waitFor();
    expect(started.channel === "codex" && started.method === "device_code" && started.grant_id === a.id, "Device reauthorization did not use the selected grant");
    await settings.getByRole("button", { name: "Cancel", exact: true }).click();
    await deviceSignIn.waitFor();
    expect(deviceConfirmed === 1 && deviceCancelled === 1, "The console did not save and cancel the respective device attempts exactly once");

    const codexSignIn = settings.getByRole("button", { name: "Sign in with Codex", exact: true });
    const callbackInput = settings.getByRole("textbox", { name: "Callback URL", exact: true });
    const saveAuthorization = settings.getByRole("button", { name: "Save authorization", exact: true });
    const settingsURL = page.url();
    await codexSignIn.click();
    await pasteCallback.click();
    await callbackInput.waitFor();
    await page.setViewportSize({ width: 320, height: 720 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= 320), "The pasted callback form overflowed a narrow viewport");
    await page.setViewportSize({ width: 1100, height: 720 });
    const rejectedCode = "rejected-callback-secret";
    await callbackInput.fill("http://127.0.0.1:1455/auth/callback?code=" + rejectedCode + "&state=wrong");
    await callbackInput.press("Enter");
    await settings.getByRole("alert").filter({ hasText: "The callback does not match this sign-in or has already been used." }).waitFor();
    expect(await callbackInput.inputValue() === "" && await callbackInput.getAttribute("autocomplete") === "off" &&
      await settings.getByText(rejectedCode).count() === 0 && page.url() === settingsURL,
      "Submitting a rejected callback left its URL in the field, error, or address bar");
    await callbackInput.fill(manualURLs.codex);
    await settings.getByRole("button", { name: "Submit callback", exact: true }).click();
    await saveAuthorization.waitFor();
    await settings.getByText(manualCodexGrant.upstream_user_id, { exact: true }).waitFor();
    expect(manualConfirmed.length === 0 && await callbackInput.count() === 0, "Pasting a callback bypassed confirmation or left the field open");
    await saveAuthorization.click();
    await settings.getByRole("article", { name: manualCodexGrant.display_name }).waitFor();

    await settings.getByRole("article", { name: b.display_name }).getByRole("button", { name: "Reauthorize", exact: true }).click();
    await pasteCallback.click();
    await callbackInput.fill(manualURLs.chatgpt);
    await callbackInput.press("Enter");
    await saveAuthorization.waitFor();
    await saveAuthorization.click();
    await codexSignIn.waitFor();
    expect(manualConfirmed.join(",") === "codex,chatgpt" && b.grant_epoch === 2 && page.url() === settingsURL,
      "Pasted callbacks did not complete both sign-in channels through their confirmation steps");
    const callbackSecrets = [rejectedCode, ...Object.values(manualURLs).flatMap(value => {
      const query = new URL(value).searchParams;
      return [query.get("code"), query.get("state")];
    })];
    expect(await page.evaluate(secrets => {
      const stored = JSON.stringify([localStorage, sessionStorage]);
      return secrets.every(secret => !stored.includes(secret));
    }, callbackSecrets), "A pasted callback code or state was written to browser storage");

    expireOnConfirm = true;
    const expiringIdentity = settings.getByText(expiringIdentityName);
    for (const { start, complete, hint } of [
      { start: deviceSignIn, complete: completeFakeDeviceSignIn, hint: "This sign-in attempt has expired. Close it and start a new authorization." },
      { start: codexSignIn, complete: completeFakeSignIn, hint: "This sign-in attempt is unavailable. Start a new authorization." },
    ]) {
      await start.click();
      await complete();
      await expiringIdentity.waitFor();
      await saveAuthorization.click();
      await settings.getByRole("alert").filter({ hasText: hint }).waitFor();
      expect(confirmationUnavailable && await saveAuthorization.count() === 0 && await expiringIdentity.count() === 0 &&
        await settings.getByRole("heading", { name: "Confirm the signed-in account", exact: true }).count() === 0,
        "Confirmation still showed a stale account or Save action after the attempt expired");
      await settings.getByRole("button", { name: "Cancel", exact: true }).click();
      await codexSignIn.waitFor();
    }
    expect(deviceCancelled === 1, "Closing an expired device attempt sent an additional cancellation");

    expireOnConfirm = false;
    conflictOnConfirm = true;
    await chatGPTSignIn.click();
    await completeFakeSignIn();
    await saveAuthorization.waitFor();
    const statusURL = `${base}/grant-authorizations/attempt-${attemptNumber}`;
    const statusRead = page.waitForRequest(request => request.method() === "GET" && request.url() === statusURL);
    await saveAuthorization.click();
    await statusRead;
    const conflictAlert = settings.getByRole("alert").filter({ hasText: duplicateGrantError });
    const cancelConflict = settings.getByRole("button", { name: "Cancel", exact: true });
    await conflictAlert.waitFor();
    expect(await cancelConflict.isEnabled() && await saveAuthorization.isDisabled(),
      "Checking the failed confirmation did not allow cancel while blocking a second save");
    releaseConflictRead();
    await saveAuthorization.waitFor({ state: "hidden" });
    await conflictAlert.waitFor();
    await cancelConflict.click();

    await Promise.all([
      page.waitForURL(origin + "/workspaces/w1/settings#workspace-grant"),
      cardA.getByRole("link", { name: w1.name }).click(),
    ]);
    const workspace = page.getByRole("region", { name: "Codex gateway", exact: true });
    const select = workspace.getByLabel("Choose authorization", { exact: true });
    const apply = workspace.getByRole("button", { name: "Apply authorization", exact: true });
    const rotate = workspace.getByRole("button", { name: "Rotate container credential", exact: true });
    const waitingForContainer = workspace.getByText("Waiting for the container", { exact: true });
    await rotate.waitFor();
    // The control plane loses and then restores its running-instance credential independently of this page.
    binding.readiness = "pending";
    await waitingForContainer.waitFor();
    await rotate.waitFor({ state: "hidden" });
    binding.readiness = "ready";
    await rotate.waitFor();
    await select.selectOption(b.id);
    await workspace.getByText(/Changing the binding ends gateway connections/).waitFor();
    await apply.click();
    await workspace.getByText(/The binding changed while you were editing/).waitFor();
    expect(await select.inputValue() === a.id && await apply.isDisabled(), "The console attempted to overwrite an unseen revision");

    await select.selectOption(b.id);
    await apply.click();
    await waitingForContainer.waitFor();
    expect(requestedRevisions.join(",") === '"8","9"' && a.workspaces.map(workspace => workspace.id).join(",") === w2.id && b.workspaces.map(workspace => workspace.id).join(",") === w1.id,
      "The console did not apply the new binding to this workspace only");
    binding.readiness = "ready";
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
    return {
      confirmed, deviceConfirmed, deviceCancelled, manualConfirmed, requestedRevisions, rotated, revoked, removed,
      deviceCopy: { selectedCode, buttonClipboard, linkClipboard, verificationURL, manualCopyText, overlappingCopyText, ...deviceLayout },
    };
  } finally {
    releaseConflictRead();
    await context.unroute(pattern);
    await context.unroute(provider);
    await context.unroute(verification);
  }
}
