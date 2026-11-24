import assert from "node:assert/strict";
import test from "node:test";
import { localizeProblem, localizeSSHMessage } from "../lib/i18n.ts";

test("SSH host registration and host status show the specific failure in the selected language", () => {
  const login = "SSH login failed. Check the username, authentication method, and credentials.";
  const docker = "Cannot access Docker over SSH. Check the remote Docker CLI, daemon, and socket permissions.";
  const bridge = "Cannot prepare the Workspace bridge. Check its name, driver, and Docker permissions.";
  const translated = [login, docker, bridge].map(original => {
    const status = localizeSSHMessage("zh-CN", original);
    assert.match(status, /\p{Script=Han}/u);
    assert.equal(localizeProblem("zh-CN", "SSH_CONNECTION_FAILED", original), status);
    assert.equal(localizeSSHMessage("en", original), original);
    return status;
  });
  assert.equal(new Set(translated).size, 3);
  assert.match(localizeSSHMessage("zh-CN", "SSH host key has changed"), /指纹|密钥/);
});
