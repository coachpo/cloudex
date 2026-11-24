// Run with Playwright CLI on a local Cloudex /login page connected to its API.
async page => {
  await page.setExtraHTTPHeaders({ "Accept-Language": "en-US,en;q=0.9" });
  await page.reload();
  const trigger = page.getByRole("button", { name: /^Theme \(/ });
  const menu = page.getByRole("menu");
  const username = page.getByRole("textbox", { name: "Username", exact: true });
  await username.waitFor();

  for (const key of ["Tab", "Shift+Tab", "Escape"]) {
    await trigger.focus();
    await page.keyboard.press("Enter");
    await page.waitForFunction(() => document.activeElement?.getAttribute("role") === "menuitemradio", undefined, { timeout: 5000 });
    await page.keyboard.press(key);
    const afterKey = await page.evaluate(() => {
      const active = document.activeElement;
      return { role: active?.getAttribute("role"), id: active?.id };
    });
    if (afterKey.role === "menuitemradio") {
      throw new Error(`${key} left the theme menu open with focus on ${JSON.stringify(afterKey)}`);
    }
    await menu.waitFor({ state: "hidden", timeout: 5000 });

    if (key === "Tab" && !(await username.evaluate(input => input === document.activeElement))) {
      throw new Error("Tab did not move focus from the menu to Username");
    }
    const triggerFocused = await trigger.evaluate(button => button === document.activeElement);
    if (triggerFocused !== (key === "Escape")) {
      throw new Error(`${key} incorrectly ${triggerFocused ? "restored" : "left"} focus on the theme button`);
    }
  }
  return "PASS theme menu Tab and Shift+Tab exit; Escape restores focus";
}
