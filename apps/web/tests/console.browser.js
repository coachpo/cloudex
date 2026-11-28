// Run with Playwright CLI on a local Cloudex page. The API is intercepted with local fixtures.
async page => {
  const origin = await page.evaluate(() => window.location.origin);
  const base = origin + "/api/v1";
  const resources = { cpu_milli: 10000, memory_bytes: 1e10, disk_bytes: 1e13 };
  const timestamp = "2026-11-25T00:00:00Z";
  const host = {
    id: "fixture", name: "h".repeat(64), status: "online", connection_type: "agent",
    scheduling_state: "enabled", scheduling_version: 1, agent_version: "fixture",
    engine: "fixture", os: "linux", arch: "arm64", healthy: true, reachable: true,
    images: [{ image: "workspace:saved", ready: true }], max_operations: 2, max_operations_target: 2,
    last_seen: timestamp, running_containers: 1, total_containers: 1,
    available: resources, allocatable: resources, reserved: resources,
  };
  const workspace = {
    id: "fixture", name: "w".repeat(64), image: "workspace:saved", repo_url: "https://example.test/repo",
    branch: "b".repeat(128), cpu_milli: 2000, memory_mb: 4096, disk_bytes: 0, assigned_host_id: host.id,
    desired_state: "running", observed_state: "running", paused: false, generation: 1, error: "", updated_at: timestamp,
  };
  const operations = [{
    id: "op", workspace_id: workspace.id, type: "start", status: "succeeded",
    attempts: 1, target_generation: 1, created_at: timestamp,
  }];
  const data = {
    "/auth/session": { username: "fixture" },
    "/auth/setup": { required: false },
    "/hosts": [host],
    "/hosts/fixture": host,
    "/hosts/fixture/capacity": host,
    "/hosts/fixture/containers": [],
    "/hosts/fixture/images": [],
    "/workspaces": [workspace],
    "/workspaces/fixture": workspace,
    "/workspaces/fixture/operations": operations,
    "/workspaces/fixture/repository-credential": { configured: false, username: "" },
    "/workspaces/fixture/access": {
      has_public_keys: true, status: "ready", host: "127.0.0.1", bind: "127.0.0.1",
      port: 2222, user: "coder", alias: "fixture", project_path: "/workspace/project",
      ssh_config: "Host fixture", known_hosts: "fixture ssh-ed25519 sample",
    },
    "/workspaces/fixture/git": { head: "head", status: "status\n".repeat(120), patch: "diff --git a/x b/x\n--- a/x\n+++ b/x\n" + "-old\n+new\n".repeat(60), truncated: false },
    "/workspaces/fixture/logs": { text: "line\n".repeat(120), truncated: false },
    "/operations": operations,
    "/settings": {
      workspace_image: "workspace:saved", ssh_host: "", ssh_bind: "127.0.0.1", agent_public_address: "",
      codex_config_toml: "", codex_agents_md: "",
    },
    "/settings/ssh-keys": { public_keys: [] },
    "/grants": { grants: [] },
    "/workspaces/fixture/grant-binding": { workspace_id: workspace.id, grant_id: "", binding_revision: 0, readiness: "unbound" },
    "/templates": [{ id: "codex", name: "Codex", image: "workspace:saved", cpu_milli: 2000, memory_mb: 4096 }],
  };
  const check = (condition, message) => {
    if (!condition) throw new Error(message);
  };
  const focused = locator => locator.evaluate(element => element === document.activeElement);
  const contrast = (locator, foreground = "color") => locator.evaluate((element, foreground) => {
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 1;
    const context = canvas.getContext("2d", { willReadFrequently: true });
    const luminance = color => {
      context.fillStyle = color;
      context.fillRect(0, 0, 1, 1);
      const linear = Array.from(context.getImageData(0, 0, 1, 1).data, channel => {
        const value = channel / 255;
        return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
      });
      return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
    };
    const style = getComputedStyle(element);
    const [low, high] = [luminance(style[foreground]), luminance(style.backgroundColor)].sort((a, b) => a - b);
    return (high + 0.05) / (low + 0.05);
  }, foreground);
  await page.setExtraHTTPHeaders({ "accept-language": "en-US,en;q=0.9" });
  const apiPattern = base + "/**";
  const createAttempts = [];
  let failAccess = false;
  let failWorkspaces = false;
  let setupPosts = 0;
  await page.route(apiPattern, route => {
    const request = route.request();
    const path = request.url().slice(base.length);
    if (path === "/hosts/ssh/probe" && request.method() === "POST") {
      return route.fulfill({ json: { data: { host_key: "ssh-ed25519 sample", fingerprint: "SHA256:sample" } } });
    }
    if (path === "/settings/ssh-keys" && request.method() === "PUT") {
      data[path] = request.postDataJSON();
    }
    if (path === "/settings/codex" && request.method() === "PUT") {
      const files = request.postDataJSON();
      data["/settings"] = { ...data["/settings"], codex_agents_md: files.agents_md, codex_config_toml: files.config_toml };
      return route.fulfill({ json: { data: files } });
    }
    if (path === "/workspaces/fixture/access" && failAccess) {
      return route.fulfill({ status: 409, json: { error: { code: "WORKSPACE_BUSY", message: "Access refresh fixture failure" } } });
    }
    if (path === "/workspaces" && request.method() === "GET" && failWorkspaces) {
      return route.fulfill({ status: 409, json: { error: { code: "FIXTURE_FAILURE", message: "Workspace list unavailable" } } });
    }
    if (path === "/auth/setup" && request.method() === "POST") {
      setupPosts++;
      return route.fulfill({ json: { data: data["/auth/session"] } });
    }
    if (path === "/workspaces" && request.method() === "POST") {
      const key = request.headers()["idempotency-key"];
      createAttempts.push({ key, body: request.postDataJSON() });
      if (createAttempts.length === 1) return route.abort();
      if (createAttempts[0].key === key) {
        return route.fulfill({ status: 202, json: { data: { workspace_id: workspace.id, operation_id: "create", status: "queued" } } });
      }
      return route.fulfill({ status: 409, json: { error: { code: "CONFLICT", message: "Workspace name is already in use." } } });
    }
    return path in data
      ? route.fulfill({ json: { data: data[path] } })
      : route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: path } } });
  });

  try {
    const layoutPages = [
      ["/workspaces", "a.entry-link"],
      ["/hosts", "a.entry-link"],
      ["/hosts/fixture", "#operation-limit-heading"],
      ["/workspaces/new", "#target-host"],
      ["/workspaces/fixture/logs", "pre.log"],
      ["/workspaces/fixture/git", "pre.log"],
      ["/settings", "#settings-workspace-image"],
    ];
    for (const width of [320, 641, 1100, 1280]) {
      await page.setViewportSize({ width, height: 720 });
      for (const [path, ready] of layoutPages) {
        await page.goto(origin + path);
        await page.locator(ready).first().waitFor();
        const pageWidth = await page.evaluate(() => document.documentElement.scrollWidth);
        check(pageWidth <= width, `${path} overflowed its ${width}px viewport to ${pageWidth}px`);
      }
    }

    await page.goto(origin + "/workspaces");
    await page.getByRole("heading", { name: "Workspaces", exact: true }).waitFor();
    const primaryNav = page.getByRole("navigation", { name: "Main navigation" });
    check(await primaryNav.getByRole("link", { name: "Workspaces" }).getAttribute("aria-current") === "page",
      "The desktop navigation did not identify the current page");
    const skip = page.getByRole("link", { name: "Skip to main content" });
    await page.keyboard.press("Tab");
    check(await focused(skip), "The skip link was not the first keyboard stop");
    await page.keyboard.press("Enter");
    check(await focused(page.getByRole("main")), "The skip link did not focus main content");

    data["/auth/session"] = { username: "Administrator-Long-Fixture" };
    const longUsername = page.getByRole("banner").getByText(data["/auth/session"].username, { exact: true });
    for (const width of [320, 760, 761, 768]) {
      await page.setViewportSize({ width, height: 720 });
      await page.reload();
      await longUsername.waitFor({ state: "attached" });
      check(await longUsername.evaluate(element => element.getBoundingClientRect().width >= 24),
        `The header left no usable room for the signed-in username at ${width}px`);
    }
    data["/auth/session"] = { username: "fixture" };

    await page.setViewportSize({ width: 320, height: 400 });
    await page.goto(origin + "/hosts/fixture");
    await page.getByRole("banner").getByText("fixture", { exact: true }).waitFor();
    const hostTab = primaryNav.getByRole("link", { name: "Hosts", exact: true });
    check(await hostTab.getAttribute("aria-current") === "page" && await primaryNav.evaluate(nav => nav.getBoundingClientRect().bottom === innerHeight),
      "The mobile navigation did not identify the active section at the bottom of the viewport");
    const disable = page.getByRole("button", { name: "Disable host", exact: true });
    await disable.click();
    const dialog = page.getByRole("alertdialog");
    await dialog.waitFor();
    check(await dialog.evaluate(element => element.scrollWidth <= element.clientWidth),
      "A long host name widened the confirmation dialog's contents");
    check(await focused(dialog.getByRole("button", { name: "Keep host" })), "The confirmation did not focus the safe action");
    await page.keyboard.press("Escape");
    await disable.and(page.locator(":focus")).waitFor({ timeout: 5000 });

    const disk = page.getByRole("meter", { name: /^Disk:/ });
    await disk.waitFor();
    const capacityHost = { ...host, allocatable: { ...resources, disk_bytes: 0 }, reserved: { ...resources, disk_bytes: 0 },
      available: { ...resources, disk_bytes: 0 } };
    data["/hosts/fixture/capacity"] = capacityHost;
    await page.reload();
    await page.getByText("Allocatable 0 B", { exact: true }).waitFor();
    check(await disk.count() === 0, "A host with zero disk capacity exposed a meter with no range");
    data["/hosts/fixture/capacity"] = host;

    await page.goto(origin + "/settings");
    await page.getByRole("button", { name: "Save account" }).focus();
    const nextInput = page.getByLabel("Default workspace image", { exact: true });
    await nextInput.waitFor();
    await nextInput.evaluate(element => window.scrollBy(0, element.getBoundingClientRect().top - (innerHeight - 57)));
    await page.keyboard.press("Tab");
    check(await nextInput.evaluate(element => {
      const rail = document.querySelector(".app-nav").getBoundingClientRect();
      return document.activeElement === element && element.getBoundingClientRect().bottom <= rail.top;
    }), "The bottom navigation covered the focused field");
    await page.setViewportSize({ width: 1100, height: 720 });

    await page.goto(origin + "/hosts");
    const add = page.getByRole("button", { name: "Add host", exact: true });
    await add.focus();
    await page.keyboard.press("Enter");
    const enrollment = page.getByRole("heading", { name: "Add host", exact: true });
    await enrollment.waitFor();
    check(await focused(enrollment), "Opening host enrollment did not move focus into the panel");
    await page.keyboard.press("Tab");
    const ssh = page.getByRole("radio", { name: "Direct SSH" });
    const agent = page.getByRole("radio", { name: "Agent", exact: true });
    check(await focused(ssh), "Tab did not reach the selected connection type");
    await page.keyboard.press("ArrowRight");
    check(await focused(agent) && await agent.isChecked(), "Selecting Agent moved focus out of the radio group");
    await page.keyboard.press("ArrowLeft");
    check(await focused(ssh) && await ssh.isChecked(), "ArrowLeft did not switch back to Direct SSH");
    await page.getByLabel("Host name", { exact: true }).fill("Browser fixture");
    await page.locator("#ssh-host-address").fill("example.test:2222");
    const getFingerprint = page.getByRole("button", { name: "Get host fingerprint", exact: true });
    await getFingerprint.click();
    await page.getByText("ssh-ed25519 SHA256:sample", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Check fingerprint again", exact: true }).click();
    check(await focused(getFingerprint), "Resetting fingerprint verification lost focus");
    await page.getByRole("button", { name: "Cancel", exact: true }).click();
    check(await focused(add), "Closing enrollment did not restore trigger focus");

    await page.goto(origin + "/settings");
    const accountSettings = page.getByRole("region", { name: "Account", exact: true });
    const accountMismatch = accountSettings.getByRole("alert").filter({ hasText: "Passwords do not match." });
    const currentPassword = page.getByLabel("Current password", { exact: true });
    check(await currentPassword.evaluate(field => field.form.method === "post"), "The account form permits credentials in the URL");
    await currentPassword.fill("fixture-password");
    await page.getByLabel("New password (optional)", { exact: true }).fill("new-password");
    const accountConfirmation = page.getByLabel("Confirm new password", { exact: true });
    await accountConfirmation.fill("mistyped-password");
    await accountSettings.getByRole("button", { name: "Save account" }).click();
    await accountMismatch.waitFor();
    check(await focused(accountConfirmation) && await accountConfirmation.getAttribute("aria-invalid") === "true",
      "The account form did not focus and identify its invalid confirmation");
    await accountConfirmation.fill("new-password");
    await accountMismatch.waitFor({ state: "hidden" });
    check(await accountConfirmation.getAttribute("aria-invalid") === "false",
      "Correcting the account confirmation left the field invalid");

    const keys = page.getByLabel("Authorized keys", { exact: true });
    await keys.fill("ssh-ed25519 fixture Saved");
    await page.getByRole("button", { name: "Save public keys" }).click();
    const keysSaved = page.getByText("Public keys saved.", { exact: true });
    await keysSaved.waitFor();
    await keys.fill("ssh-ed25519 fixture Draft");
    check(await keysSaved.count() === 0, "An unsaved draft still displayed the public-key save confirmation");

    const codex = page.getByRole("region", { name: "Codex settings", exact: true });
    const agents = codex.getByLabel("AGENTS.md", { exact: true });
    const config = codex.getByLabel("config.toml", { exact: true });
    const settingsBeforeCodex = data["/settings"];
    const agentsDraft = "# Saved in another tab\n";
    const latestAgents = "# Latest server version\n";
    const configDraft = "# Local draft\nmodel = \"fixture\"\n";
    await agents.fill(agentsDraft);
    await config.fill(configDraft);

    data["/settings"] = { ...settingsBeforeCodex, codex_agents_md: agentsDraft, ssh_host: "converged.example.test" };
    await page.locator('#settings-ssh-host[value="converged.example.test"]').waitFor();
    check(await config.inputValue() === configDraft, "Matching a saved Codex file discarded the other file's draft");

    data["/settings"] = { ...settingsBeforeCodex, codex_agents_md: latestAgents, ssh_host: "latest.example.test" };
    await page.locator('#settings-ssh-host[value="latest.example.test"]').waitFor();
    check(await agents.inputValue() === latestAgents, "A saved Codex draft hid a newer server value");
    const codexWrite = page.waitForRequest(req => req.url() === base + "/settings/codex" && req.method() === "PUT");
    await codex.getByRole("button", { name: "Save Codex settings" }).click();
    const codexFiles = (await codexWrite).postDataJSON();
    check(codexFiles.agents_md === latestAgents && codexFiles.config_toml === configDraft,
      "Saving one Codex file overwrote the newer saved value of the other file");
    await codex.getByRole("status").filter({ hasText: "Codex settings saved." }).waitFor();
    data["/settings"] = settingsBeforeCodex;

    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto(origin + "/workspaces/new");
    await page.getByLabel("Target host", { exact: true }).selectOption(host.id);
    const name = page.getByLabel("Workspace name", { exact: true });
    const create = page.getByRole("button", { name: "Create workspace", exact: true });
    await page.waitForFunction(() => !!document.querySelector("#name")?.value);
    const suggestedName = await name.inputValue();
    const roll = page.getByRole("button", { name: "Generate a random name", exact: true });
    await roll.click();
    const rerolledName = await name.inputValue();
    check([suggestedName, rerolledName].every(value => /^[a-z]+-[a-z]+$/.test(value)) && suggestedName !== rerolledName,
      "The name field did not start with a valid suggestion and change when rerolled");
    check(createAttempts.length === 0, "Generating a workspace name submitted the form");
    const newWorkspaceLayout = await page.evaluate(() => ({ viewport: innerWidth, pageWidth: document.documentElement.scrollWidth }));
    check(newWorkspaceLayout.viewport === 320 && newWorkspaceLayout.pageWidth <= newWorkspaceLayout.viewport,
      "Rerolling the workspace name overflowed a narrow viewport");
    const cores = page.locator("#cores");
    const initialCores = await cores.inputValue();
    const manualName = "manual workspace draft";
    await name.fill(manualName);
    await cores.fill("3");
    check(await name.inputValue() === manualName, "Updating workspace resources overwrote the manually edited name");
    await name.clear();
    await cores.fill(initialCores);
    check(await name.inputValue() === "", "Updating workspace resources repopulated a cleared name");
    await page.setViewportSize({ width: 1100, height: 720 });
    const unicodeName = String.fromCodePoint(0x10400).repeat(64);
    await name.focus();
    await page.keyboard.insertText(unicodeName);
    check(await name.inputValue() === unicodeName, "The workspace name field truncated a valid 64-letter name");
    await page.locator("#url").fill("https://example.test/fixture.git");
    await page.locator("#repo-username").fill("fixture");
    const repositoryToken = page.locator("#repo-token");
    check(await repositoryToken.evaluate(field => field.form.method === "post"), "The workspace creation form permits credentials in the URL");
    await repositoryToken.fill("test-value");
    await name.fill("  Browser fixture  ");
    await create.click();
    await page.locator("form").getByRole("alert").waitFor();
    await name.fill("Browser fixture");
    await create.click();
    await page.waitForURL(origin + "/workspaces/fixture/access", { timeout: 5000 });
    const [first, retry] = createAttempts;
    check(createAttempts.length === 2 && first.key && first.key === retry.key,
      "Retrying a creation with equivalent workspace names used a different idempotency key");
    check(createAttempts.every(({ body }) => body.name === "Browser fixture" && body.repository.credential.token === "test-value"),
      "Workspace creation did not submit the normalized name and repository credential on both attempts");

    await page.getByText("Gateway ready", { exact: true }).waitFor();
    const clipboardContext = page.context();
    const access = data["/workspaces/fixture/access"];
    const sshCopy = [
      { label: "Copy known_hosts entries", value: access.known_hosts },
      { label: "Copy SSH config", value: access.ssh_config },
    ].map(({ label, value }) => {
      const button = page.getByRole("button", { name: label, exact: true });
      return { button, status: button.locator("..").getByRole("status"), value };
    });
    await clipboardContext.grantPermissions(["clipboard-read", "clipboard-write"], { origin });
    try {
      for (const { button, status, value } of sshCopy) {
        await button.click();
        await status.filter({ hasText: /^Copied$/ }).waitFor();
        check(await page.evaluate(() => navigator.clipboard.readText()) === value, "Copied SSH text differs from the displayed entries");
      }
    } finally {
      await clipboardContext.clearPermissions();
    }
    const restoreClipboard = await page.evaluateHandle(() => {
      const writeText = navigator.clipboard.writeText;
      navigator.clipboard.writeText = async () => { throw new DOMException("Fixture write rejected", "NotAllowedError"); };
      return () => { navigator.clipboard.writeText = writeText; };
    });
    try {
      for (const { button, status, value } of sshCopy) {
        await button.click();
        await status.filter({ hasText: "Select the text below to copy it manually." }).waitFor();
        await page.getByText(value, { exact: true }).selectText();
        check(await page.evaluate(() => window.getSelection()?.toString()) === value, "SSH text could not be selected for manual copying");
      }
    } finally {
      await restoreClipboard.evaluate(restore => restore());
      await restoreClipboard.dispose();
    }
    access.known_hosts = "fixture ssh-ed25519 updated";
    access.ssh_config = "Host fresh-fixture";
    await page.getByText(access.known_hosts, { exact: true }).waitFor();
    await page.getByText(access.ssh_config, { exact: true }).waitFor();
    for (const { status } of sshCopy) {
      check((await status.textContent()).trim() === "", "Updated SSH entries still displayed the previous copy result");
    }
    failAccess = true;
    const refreshErrors = page.getByRole("alert").filter({ hasText: "Access refresh fixture failure" });
    await refreshErrors.first().waitFor();
    check(await refreshErrors.count() === 1, "A failed Access refresh displayed duplicate alerts");
    failAccess = false;
    await refreshErrors.first().waitFor({ state: "hidden" });

    for (const [path, labels] of [
      ["/workspaces/fixture/logs", ["Container logs"]],
      ["/workspaces/fixture/git", ["Working tree", "Git patch"]],
    ]) {
      await page.goto(origin + path);
      for (const label of labels) {
        const output = page.getByRole("region", { name: label, exact: true });
        await output.waitFor();
        if (label === "Git patch") {
          check(await output.textContent() === data["/workspaces/fixture/git"].patch,
            "The highlighted patch changed the API's Git text");
        }
        await output.focus();
        check(await focused(output), `${label} could not receive focus`);
        await page.keyboard.press("PageDown");
        await page.waitForFunction(label => Array.from(document.querySelectorAll("pre")).some(
          element => element.getAttribute("aria-label") === label && element.scrollTop > 0,
        ), label);
      }
    }

    const terminalURL = origin.replace(/^http/, "ws") + "/api/v1/workspaces/fixture/terminal";
    await page.routeWebSocket(terminalURL, () => {});
    await page.goto(origin + "/workspaces/fixture/terminal");
    await page.getByText("Connected", { exact: true }).waitFor();
    await page.getByRole("textbox", { name: "Terminal input" }).focus();
    await page.keyboard.press("Shift+Tab");
    check(await focused(page.getByRole("button", { name: "Reconnect" })),
      "Shift+Tab skipped the control immediately before the terminal");

    // Next streams the matched workspace route and renders its not-found boundary after headers are sent.
    for (const [path, status] of [["/unknown", 404], ["/workspaces/fixture/unknown", 200]]) {
      const response = await page.goto(origin + path);
      await page.getByRole("heading", { name: "Page not found" }).waitFor();
      check(response?.status() === status, `${path} changed its HTTP ${status} response`);
      check(await page.getByRole("main").count() === 1, `${path} did not render one main landmark`);
    }

    await page.setViewportSize({ width: 320, height: 720 });
    await page.goto(origin + "/workspaces");
    const all = page.getByRole("group", { name: "Filter by state" }).getByRole("button", { name: "All", exact: true });
    await all.focus();
    check(await all.evaluate(button => parseFloat(getComputedStyle(button).outlineOffset) < 0),
      "The filter focus outline extends outside the scroll container");

    const gitURL = base + "/workspaces/fixture/git";
    let releaseGit;
    const waitForGit = new Promise(resolve => { releaseGit = resolve; });
    const delayGit = async route => {
      await waitForGit;
      await route.fulfill({ json: { data: data["/workspaces/fixture/git"] } }).catch(() => {});
    };
    await page.route(gitURL, delayGit);
    try {
      const requested = page.waitForRequest(gitURL);
      await page.goto(origin + "/workspaces/fixture/git");
      await requested;
      const aborted = page.waitForEvent("requestfailed", { predicate: request => request.url() === gitURL, timeout: 5000 });
      await page.getByRole("link", { name: "Logs", exact: true }).click();
      await aborted;
    } finally {
      releaseGit();
      await page.unroute(gitURL, delayGit);
    }

    failWorkspaces = true;
    await page.goto(origin + "/workspaces");
    const listError = page.getByRole("alert").filter({ hasText: "Workspace list unavailable" });
    await listError.waitFor();
    data["/workspaces"] = [];
    failWorkspaces = false;
    await listError.getByRole("button", { name: "Retry" }).click();
    await page.getByText("No workspaces yet.", { exact: true }).waitFor();
    check(await listError.count() === 0, "A successful retry kept the previous list error visible");
    data["/workspaces"] = [workspace];

    await page.goto("about:blank");
    let releaseSettings;
    let recoverSettings = false;
    const waitForSettings = new Promise(resolve => { releaseSettings = resolve; });
    const settingsURL = base + "/settings";
    const delaySettings = async route => {
      await waitForSettings;
      if (recoverSettings) return route.fallback();
      return route.fulfill({ status: 409, json: { error: { code: "FIXTURE_FAILURE", message: "Settings unavailable" } } });
    };
    await page.route(settingsURL, delaySettings);
    try {
      await page.goto(origin + "/settings");
      const defaults = page.getByRole("region", { name: "Workspace defaults" });
      const defaultsLink = page.getByRole("navigation", { name: "Settings sections" }).getByRole("link", { name: "Workspace defaults" });
      await defaults.getByRole("status").filter({ hasText: "Loading" }).waitFor();
      releaseSettings();
      const settingsError = page.getByRole("alert").filter({ hasText: "Settings unavailable" });
      await settingsError.waitFor();
      await defaultsLink.waitFor({ state: "detached" });
      check(await defaults.count() === 0, "A failed settings read left an empty defaults section");
      recoverSettings = true;
      await settingsError.getByRole("button", { name: "Retry" }).click();
      await defaults.getByLabel("Default workspace image").waitFor();
      await defaultsLink.waitFor();
    } finally {
      releaseSettings();
      await page.unroute(settingsURL, delaySettings);
    }

    await page.emulateMedia({ reducedMotion: "reduce" });
    await page.goto(origin + "/login");
    check(await page.locator("#password").evaluate(field => field.form.method === "post"), "The login form permits credentials in the URL");
    const themeRatios = [];
    for (const [system, selection, color] of [
      ["light", "System", "rgb(244, 244, 242)"], ["dark", "System", "rgb(17, 17, 16)"],
      ["dark", "Light", "rgb(244, 244, 242)"], ["light", "Dark", "rgb(17, 17, 16)"],
    ]) {
      await page.emulateMedia({ colorScheme: system });
      const themeTrigger = page.getByRole("button", { name: /^Theme \(/ });
      await themeTrigger.click();
      await page.getByRole("menuitemradio", { name: selection }).click();
      await page.reload();
      check(await page.locator("body").evaluate(body => getComputedStyle(body).backgroundColor) === color,
        `${selection} theme did not render correctly with a ${system} system preference`);
      check(await page.evaluate(() => document.documentElement.scrollHeight === innerHeight),
        "The mobile login reserved space for the absent bottom navigation");

      await themeTrigger.press("ArrowDown");
      await page.getByRole("menuitemradio", { name: /system/i }).and(page.locator(":focus")).waitFor();
      await page.keyboard.press("ArrowDown");
      const focusedTheme = page.getByRole("menuitemradio", { name: "Light", exact: true }).and(page.locator(":focus-visible"));
      await focusedTheme.waitFor();
      check(await focusedTheme.evaluate(item => {
        const style = getComputedStyle(item);
        return style.outlineStyle === "solid" && parseFloat(style.outlineOffset) < 0;
      }) && await contrast(focusedTheme, "outlineColor") >= 3,
      `The theme menu had no contrasting, unclipped keyboard focus indicator in ${selection}/${system}`);
      await page.keyboard.press("Escape");

      await page.goto(origin + "/workspaces/fixture/settings");
      check(await page.locator("#repo-credential-token").evaluate(field => field.form.method === "post"),
        "The repository settings form permits credentials in the URL");
      const remove = page.getByRole("button", { name: "Delete workspace", exact: true });
      await remove.click();
      const confirm = page.getByRole("alertdialog").getByRole("button", { name: "Delete workspace", exact: true });
      const normalContrast = await contrast(confirm);
      await confirm.hover();
      const hoverContrast = await contrast(confirm);
      check(normalContrast >= 4.5 && hoverContrast >= 4.5,
        `${selection} theme with ${system} system preference had delete button contrast ${normalContrast.toFixed(2)} / ${hoverContrast.toFixed(2)}`);
      themeRatios.push(`${selection}/${system} ${normalContrast.toFixed(2)}/${hoverContrast.toFixed(2)}`);
      await page.keyboard.press("Escape");
      await page.goto(origin + "/login");
    }

    data["/auth/setup"] = { required: true };
    await page.reload();
    await page.getByRole("textbox", { name: "Username" }).fill("fixture");
    await page.getByLabel("Password", { exact: true }).fill("fixture-password");
    const confirmation = page.locator("#confirmation");
    await confirmation.fill("mistyped-password");
    await page.getByRole("button", { name: "Create administrator" }).click();
    const setupMismatch = page.getByRole("alert").filter({ hasText: "Passwords do not match." });
    await setupMismatch.waitFor();
    check(await focused(confirmation) && await confirmation.getAttribute("aria-invalid") === "true" && setupPosts === 0,
      "Invalid setup confirmation was not focused, marked invalid and kept local");
    await confirmation.fill("fixture-password");
    await setupMismatch.waitFor({ state: "hidden" });
    check(await confirmation.getAttribute("aria-invalid") === "false", "Correcting setup confirmation left the field invalid");

    await page.emulateMedia({ forcedColors: "active" });
    await page.keyboard.press("Shift+Tab");
    const password = page.getByLabel("Password", { exact: true });
    check(await password.evaluate(input => input === document.activeElement && getComputedStyle(input).outlineStyle === "solid"),
      "The password field lost its keyboard focus indicator in forced colors");
    await page.emulateMedia({ forcedColors: null });
    await page.getByRole("button", { name: "Create administrator" }).click();
    await page.waitForURL(origin + "/workspaces");
    check(setupPosts === 1, "The corrected administrator setup was not submitted exactly once");
    return {
      newWorkspace: { suggestedName, rerolledName, manualName, clearPreserved: true, sameRetryKey: first.key === retry.key, ...newWorkspaceLayout },
      sshCopy: sshCopy.map(({ value }) => value),
      deleteConfirmationContrast: themeRatios,
    };
  } finally {
    await page.unroute(apiPattern);
    await page.emulateMedia({ colorScheme: null, reducedMotion: null, forcedColors: null });
  }
}
