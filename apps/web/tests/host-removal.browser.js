// Run with Playwright CLI on a local Cloudex page. All API responses are fixtures.
async page => {
  const origin = await page.evaluate(() => window.location.origin);
  const base = origin + "/api/v1";
  const pattern = base + "/**";
  const image = "workspace:fixture";
  const resources = { cpu_milli: 2000, memory_bytes: 1 << 30, disk_bytes: 0 };
  const local = {
    id: "local", name: "Local Host", status: "online", connection_type: "agent",
    scheduling_state: "enabled", scheduling_version: 1, max_operations: 2, max_operations_target: 2,
    healthy: true, reachable: true, images: [{ image, ready: true }], running_containers: 0, total_containers: 0,
    allocatable: resources, reserved: { cpu_milli: 0, memory_bytes: 0, disk_bytes: 0 }, available: resources,
  };
  const agent = { ...local, id: "agent", name: "Build Agent" };
  const ssh = { ...local, id: "ssh", name: "Offline SSH", connection_type: "ssh", status: "offline", healthy: false, reachable: false };
  const delayed = { ...local, id: "delayed", name: "Delayed Agent" };
  const hosts = new Map([local, agent, ssh, delayed].map(host => [host.id, host]));
  const deletions = [];
  let deleteAgent = "pending";
  let hostReadUnavailable = false;
  let delayedReadUnavailable = false;
  let holdStaleSSHRead = false;
  let releaseSSHRead;
  const staleSSHRead = new Promise(resolve => { releaseSSHRead = resolve; });

  function check(condition, message) {
    if (!condition) throw new Error(message);
  }

  await page.setExtraHTTPHeaders({ "accept-language": "en-US,en;q=0.9" });
  await page.route(pattern, route => {
    const request = route.request();
    const path = request.url().slice(base.length);
    const data = value => route.fulfill({ json: { data: value } });
    const problem = (status, code, message) => route.fulfill({ status, json: { error: { code, message } } });
    if (path === "/auth/session") return data({ username: "fixture" });
    if (path === "/settings") return data({ workspace_image: image });
    if (path === "/templates") return data([{ id: "codex", name: "Codex", image, cpu_milli: 1000, memory_mb: 512 }]);
    if (path === "/workspaces" || path === "/operations") return data([]);
    if (path === "/hosts") return data([...hosts.values()]);
    const [, id, section] = /^\/hosts\/([^/]+)(?:\/([^/]+))?$/.exec(path) ?? [];
    if (request.method() === "DELETE") {
      deletions.push(id);
      check(request.postData() === null, "Host removal sent an unexpected request body");
      if (id === delayed.id) {
        delayedReadUnavailable = true;
        return route.abort();
      }
      if (id === agent.id) {
        if (deleteAgent === "pending") return problem(409, "HOST_HAS_PENDING_OPERATIONS", "Wait for queued or running operations.");
        if (deleteAgent === "workspaces") return problem(409, "HOST_HAS_WORKSPACES", "Delete this host's workspaces first.");
        if (deleteAgent === "lost") {
          hostReadUnavailable = true;
          return route.abort();
        }
      }
      hosts.delete(id);
      return id === ssh.id ? route.abort() : route.fulfill({ status: 204 });
    }
    const host = hosts.get(id);
    if (!host) return problem(404, "NOT_FOUND", "The requested resource was not found.");
    if (id === agent.id && !section && hostReadUnavailable) return route.abort();
    if (id === delayed.id && !section && delayedReadUnavailable) return route.abort();
    if (id === ssh.id && !section && holdStaleSSHRead) {
      holdStaleSSHRead = false;
      return staleSSHRead.then(() => data(host));
    }
    return data(section === "containers" || section === "images" ? [] : host);
  });

  const deleteHost = page.getByRole("button", { name: "Delete host", exact: true });
  const dialog = page.getByRole("alertdialog");
  const openHost = name => page.getByRole("link", { name, exact: true }).click();
  const confirm = async name => {
    await deleteHost.click();
    await dialog.getByRole("heading", { name: `Delete ${name}?` }).waitFor();
    await dialog.getByRole("button", { name: "Delete host", exact: true }).click();
  };

  try {
    await page.goto(origin + "/hosts");
    await openHost(local.name);
    await page.getByRole("heading", { name: /Local Host/ }).waitFor();
    check(await deleteHost.count() === 0, "The control-plane Host showed a deletion action");
    await page.getByRole("navigation", { name: "Breadcrumb" }).getByRole("link", { name: "Hosts" }).click();
    await openHost(agent.name);
    await deleteHost.click();
    await dialog.getByRole("heading", { name: "Delete Build Agent?" }).waitFor();
    await dialog.getByText("Cloudex deletes this host registration and its workspace operation history. The remote host, its containers, volumes, images, and networks are kept.", { exact: true }).waitFor();
    const keepHost = dialog.getByRole("button", { name: "Keep host", exact: true });
    check(await keepHost.evaluate(element => element === document.activeElement), "The destructive confirmation did not focus Keep host");
    await keepHost.click();
    await deleteHost.and(page.locator(":focus")).waitFor();
    check(deletions.length === 0, "Cancelling the confirmation sent a DELETE");

    await confirm(agent.name);
    await page.getByRole("alert").filter({ hasText: "Wait for queued or running operations." }).waitFor();
    deleteAgent = "workspaces";
    await confirm(agent.name);
    await page.getByRole("alert").filter({ hasText: "Delete this host's workspaces first." }).waitFor();

    deleteAgent = "lost";
    await confirm(agent.name);
    await page.getByRole("alert").filter({ hasText: "Cloudex could not confirm whether the host was removed." }).waitFor();
    check(await deleteHost.getAttribute("aria-disabled") === "true", "The UI allowed deletion before rechecking the uncertain result");
    hostReadUnavailable = false;
    const attempts = deletions.length;
    await page.getByRole("button", { name: "Check again", exact: true }).click();
    await page.getByRole("status").filter({ hasText: "The host is still registered. You can retry deletion." }).waitFor();
    check(deletions.length === attempts, "Rechecking the uncertain result sent another DELETE");

    deleteAgent = "success";
    await confirm(agent.name);
    await page.getByRole("heading", { name: "Build Agent was removed" }).and(page.locator(":focus")).waitFor();
    await page.getByRole("link", { name: "View hosts" }).click();
    await page.getByRole("link", { name: ssh.name, exact: true }).waitFor();
    check(await page.getByRole("link", { name: agent.name, exact: true }).count() === 0, "The removed Agent was still in the Host list");

    await openHost(ssh.name);
    await page.getByRole("heading", { name: /Offline SSH/ }).waitFor();
    holdStaleSSHRead = true;
    await page.waitForRequest(request => request.method() === "GET" && request.url() === base + "/hosts/ssh", { timeout: 10000 });
    await Promise.all([
      page.waitForResponse(response => response.url() === base + "/hosts/ssh" &&
        response.request().method() === "GET" && response.status() === 404),
      confirm(ssh.name),
    ]);
    await page.getByRole("heading", { name: "Offline SSH was removed" }).waitFor();
    releaseSSHRead();
    await page.getByRole("link", { name: "View hosts" }).click();
    await page.getByRole("link", { name: local.name, exact: true }).waitFor();
    check(await page.getByRole("link", { name: ssh.name, exact: true }).count() === 0, "The Host list still showed the SSH Host after a fresh read returned 404");

    await openHost(delayed.name);
    await confirm(delayed.name);
    await page.getByRole("alert").filter({ hasText: "Cloudex could not confirm whether the host was removed." }).waitFor();
    hosts.delete(delayed.id);
    await page.getByRole("heading", { name: "Delayed Agent was removed" }).and(page.locator(":focus")).waitFor({ timeout: 12000 });
    await page.getByRole("link", { name: "View hosts" }).click();

    await page.getByRole("link", { name: "Workspaces", exact: true }).click();
    await page.getByRole("link", { name: "New workspace", exact: true }).click();
    const placement = page.getByRole("combobox", { name: "Target host" });
    await placement.selectOption(local.id);
    const targets = await placement.evaluate(select => Array.from(select.options, option => option.value).filter(Boolean));
    check(targets.length === 1 && targets[0] === local.id, `Placement offered unexpected Hosts: ${targets.join(",")}`);
    check(deletions.join(",") === "agent,agent,agent,agent,ssh,delayed", `Unexpected deletion attempts: ${deletions.join(",")}`);
  } finally {
    releaseSSHRead();
    await page.unroute(pattern);
  }
}
