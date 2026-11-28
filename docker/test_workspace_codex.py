from concurrent.futures import ThreadPoolExecutor
from contextlib import redirect_stderr
from fcntl import LOCK_NB, flock
from http.client import HTTPResponse
import importlib.util
import io
import json
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Event, local
import tomllib
import unittest
from unittest.mock import Mock, patch
from urllib.error import HTTPError


spec = importlib.util.spec_from_file_location("workspace_codex", Path(__file__).with_name("workspace-codex.py"))
workspace_codex = importlib.util.module_from_spec(spec)
spec.loader.exec_module(workspace_codex)

CREDENTIAL = {"origin": "https://gateway.example.test", "token": "fixture", "grant_id": "grant", "grant_epoch": 1}
CATALOG = b'{"models":[{"slug":"first-model"},{"slug":"preferred-model"}]}'


def interrupted_response(status):
    socket = Mock()
    socket.makefile.return_value = io.BytesIO(
        b"HTTP/1.1 " + status + b"\r\nTransfer-Encoding: chunked\r\n\r\n10\r\n{}"
    )
    response = HTTPResponse(socket)
    response.begin()
    return response


class CatalogConnectionTest(unittest.TestCase):
    def test_app_server_starts_without_a_credential_after_catalog_read_fails(self):
        stderr = io.StringIO()
        with (
            TemporaryDirectory() as directory,
            patch.object(Path, "home", return_value=Path(directory)),
            patch.object(workspace_codex, "os") as launch,
            patch.object(workspace_codex, "load_credential", return_value=CREDENTIAL),
            patch.object(workspace_codex, "build_opener") as build_opener,
            redirect_stderr(stderr),
        ):
            launch.environ = {}
            build_opener.return_value.open.return_value = interrupted_response(b"200 OK")

            workspace_codex.main(["app-server"])

            launch.execve.assert_called_once()
            _, args, env = launch.execve.call_args.args
            self.assertEqual(args[-1], "app-server")
            overrides = tomllib.loads("\n".join(args[2:-1:2]))
            self.assertEqual(overrides["model_provider"], "cloudex")
            self.assertEqual(overrides["shell_environment_policy"]["exclude"], ["CLOUDEX_GATEWAY_TOKEN"])
            provider = overrides["model_providers"]["cloudex"]
            self.assertEqual(provider["base_url"], "https://127.0.0.1:1/g/v1")
            self.assertIn("Unable to reach the Cloudex model catalog", provider["env_key_instructions"])
            self.assertNotIn("CLOUDEX_GATEWAY_TOKEN", env)
            self.assertEqual(Path(env["CODEX_HOME"]).name, "unavailable")
            self.assertEqual((Path(env["CODEX_HOME"]) / "config.toml").read_bytes(), b"")
            self.assertIn("Unable to reach the Cloudex model catalog", stderr.getvalue())
            build_opener.return_value.open.assert_called_once()

    def test_incomplete_error_body_preserves_http_status(self):
        response = interrupted_response(b"503 Service Unavailable")
        error = HTTPError("https://gateway.example.test/g/v1/models", 503, response.reason, response.headers, response)
        with patch.object(workspace_codex, "build_opener") as build_opener:
            build_opener.return_value.open.side_effect = error

            with self.assertRaisesRegex(workspace_codex.GatewayUnavailable, "HTTP 503"):
                workspace_codex.load_catalog("https://gateway.example.test/g/v1", "fixture")


class StartupSettingsTest(unittest.TestCase):
    def setUp(self):
        directory = Path(self.enterContext(TemporaryDirectory())).resolve()
        self.home = directory / ".codex" / "cloudex" / "grant-g1"
        self.enterContext(patch.object(Path, "home", return_value=directory))
        self.launch = self.enterContext(patch.object(workspace_codex, "os"))
        self.launch.environ = {
            "CLOUDEX_GATEWAY_TOKEN": "stale",
            "OPENAI_API_KEY": "synthetic",
            "CODEX_API_KEY": "synthetic",
            "CODEX_SQLITE_HOME": "/fixture",
        }
        self.load_credential = self.enterContext(patch.object(workspace_codex, "load_credential", return_value=CREDENTIAL))
        self.build_opener = self.enterContext(patch.object(workspace_codex, "build_opener"))
        self.open = self.build_opener.return_value.open

    def start(self, config_toml, agents_md):
        settings = json.dumps({"config_toml": config_toml, "agents_md": agents_md}).encode()
        self.open.side_effect = [io.BytesIO(CATALOG), io.BytesIO(settings)]
        workspace_codex.main(["exec", "fixture"])
        _, args, env = self.launch.execve.call_args.args
        self.assertEqual(args[-2:], ["exec", "fixture"])
        return tomllib.loads("\n".join(args[2:-2:2])), env

    def test_applies_raw_files_and_overrides_with_preferred_model(self):
        config = (
            '# 配置 🚀\r\nmodel = "preferred-model"\r\nmodel_context_window = 50000\r\n\r\n'
            '[mcp_servers.fixture]\r\ncommand = "fixture-mcp"\r\n'
            '[shell_environment_policy]\r\ninherit = "core"\r\n'
            'exclude = ["SECRET_*", "CLOUDEX_GATEWAY_TOKEN", "SECRET_*", "🐚"]\r\n'
        )
        instructions = "# 工作说明\r\nFixture instructions.\r\n"
        overrides, env = self.start(config, instructions)

        self.assertEqual(Path(env["CODEX_HOME"]), self.home)
        self.assertEqual((self.home / "config.toml").read_bytes(), config.encode())
        self.assertEqual((self.home / "AGENTS.md").read_bytes(), instructions.encode())
        catalog = Path(overrides.pop("model_catalog_json"))
        self.assertEqual(catalog.parent, self.home)
        self.assertEqual(catalog.read_bytes(), CATALOG)
        self.assertEqual(overrides, {
            "model_provider": "cloudex",
            "cli_auth_credentials_store": "file",
            "model_providers": {"cloudex": {
                "name": "Cloudex",
                "base_url": CREDENTIAL["origin"] + "/g/v1",
                "wire_api": "responses",
                "env_key": "CLOUDEX_GATEWAY_TOKEN",
                "env_key_instructions": "Check the Workspace's Cloudex authorization binding.",
                "requires_openai_auth": False,
                "supports_websockets": True,
                "request_max_retries": 0,
                "stream_max_retries": 0,
                "env_http_headers": {"x-cloudex-run-id": "CLOUDEX_GATEWAY_RUN_ID"},
            }},
            "shell_environment_policy": {"exclude": ["SECRET_*", "CLOUDEX_GATEWAY_TOKEN", "🐚"]},
            "model": "preferred-model",
        })
        self.assertEqual(env["CLOUDEX_GATEWAY_TOKEN"], CREDENTIAL["token"])
        self.assertRegex(env["CLOUDEX_GATEWAY_RUN_ID"], r"^[0-9a-f]{32}$")
        for name in ("OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_SQLITE_HOME"):
            self.assertNotIn(name, env)
        requests = self.open.call_args_list
        self.assertEqual([call.args[0].full_url for call in requests], [
            CREDENTIAL["origin"] + "/g/v1/models",
            CREDENTIAL["origin"] + "/g/v1/codex-settings",
        ])
        for call in requests:
            request = call.args[0]
            self.assertEqual(request.get_method(), "GET")
            self.assertEqual(request.get_header("Authorization"), "Bearer fixture")
            self.assertEqual(call.kwargs["timeout"], 15)
        for call in self.build_opener.call_args_list:
            proxy, redirect = call.args
            self.assertEqual(proxy.proxies, {})
            self.assertIsInstance(redirect, workspace_codex.NoRedirect)

    def test_replaces_settings_on_next_launch_and_removes_cleared_agents(self):
        self.start('model = "preferred-model"', "# Previous instructions\n")

        overrides, _ = self.start('model = "unavailable-model"\n', "")
        self.assertEqual(overrides["model"], "first-model")
        self.assertEqual(overrides["shell_environment_policy"]["exclude"], ["CLOUDEX_GATEWAY_TOKEN"])
        self.assertEqual((self.home / "config.toml").read_bytes(), b'model = "unavailable-model"\n')
        self.assertFalse((self.home / "AGENTS.md").exists())

        overrides, _ = self.start("", "")
        self.assertEqual(overrides["model"], "first-model")
        self.assertEqual((self.home / "config.toml").read_bytes(), b"")

    def test_concurrent_launches_fetch_and_install_one_settings_pair_at_a_time(self):
        first_config_written = Event()
        first_agents_written = Event()
        second_ready = Event()
        first_may_finish = Event()
        launch = local()
        first = ('model_reasoning_effort = "low"\n', "# First revision\n")
        second = ('model_reasoning_effort = "high"\n', "# Second revision\n")
        real_write = workspace_codex.atomic_write

        def settings(*_):
            if launch.files == second:
                first_finished = first_agents_written.is_set()
                second_ready.set()
                self.assertTrue(first_finished, "second launch fetched settings before the first pair was written")
            config, agents = launch.files
            return config, agents, tomllib.loads(config)

        def write(path, data):
            real_write(path, data)
            if launch.files == first:
                if path.name == "config.toml":
                    first_config_written.set()
                    self.assertTrue(first_may_finish.wait(5), "first launch was not released")
                elif path.name == "AGENTS.md":
                    first_agents_written.set()

        def lock(file, operation):
            if launch.files == second:
                try:
                    flock(file, operation | LOCK_NB)
                except BlockingIOError:
                    second_ready.set()
                else:
                    self.fail("second launch could acquire the settings lock during the first write")
            flock(file, operation)

        def start(files):
            launch.files = files
            workspace_codex.main(["exec", "fixture"])

        with (
            patch.object(workspace_codex, "load_catalog", return_value=(CATALOG, ["first-model"])),
            patch.object(workspace_codex, "load_settings", side_effect=settings),
            patch.object(workspace_codex, "atomic_write", side_effect=write),
            patch.object(workspace_codex, "flock", side_effect=lock, create=True),
            ThreadPoolExecutor(max_workers=2) as workers,
        ):
            initial = workers.submit(start, first)
            try:
                self.assertTrue(first_config_written.wait(5), "first launch did not write config.toml")
                latest = workers.submit(start, second)
                self.assertTrue(second_ready.wait(5), "second launch did not reach the settings lock")
            finally:
                first_may_finish.set()
            initial.result()
            latest.result()

        self.assertEqual((self.home / "config.toml").read_bytes(), second[0].encode())
        self.assertEqual((self.home / "AGENTS.md").read_bytes(), second[1].encode())

    def test_keyed_shell_filters_keep_the_keyed_representation(self):
        config = '[shell_environment_policy.filters]\n"SECRET_*" = "exclude"\n"PATH" = "include"\n'
        overrides, _ = self.start(config, "")

        self.assertEqual((self.home / "config.toml").read_bytes(), config.encode())
        self.assertEqual(overrides["shell_environment_policy"], {
            "filters": {"CLOUDEX_GATEWAY_TOKEN": "exclude"},
        })

    def test_settings_request_failure_reports_stage_and_http_status(self):
        url = CREDENTIAL["origin"] + "/g/v1/codex-settings"
        error = HTTPError(url, 503, "Service Unavailable", {}, io.BytesIO(b"{}"))
        self.open.side_effect = [io.BytesIO(CATALOG), error]
        with self.assertRaisesRegex(workspace_codex.GatewayUnavailable, r"Codex startup settings request failed \(HTTP 503\)"):
            workspace_codex.main(["exec", "fixture"])
        self.launch.execve.assert_not_called()

    def test_unbound_app_server_skips_gateway_requests_and_clears_settings(self):
        self.load_credential.side_effect = workspace_codex.GatewayUnavailable("No active credential.")
        unavailable = self.home.with_name("unavailable")
        unavailable.mkdir(parents=True)
        (unavailable / "AGENTS.md").write_text("Old instructions")
        (unavailable / "config.toml").write_text('model = "old-model"')
        with redirect_stderr(io.StringIO()):
            workspace_codex.main(["app-server"])

        self.open.assert_not_called()
        _, args, env = self.launch.execve.call_args.args
        self.assertEqual(args[-1], "app-server")
        overrides = tomllib.loads("\n".join(args[2:-1:2]))
        self.assertEqual(overrides["model_provider"], "cloudex")
        self.assertEqual(overrides["cli_auth_credentials_store"], "file")
        self.assertNotIn("model_catalog_json", overrides)
        self.assertNotIn("model", overrides)
        self.assertNotIn("CLOUDEX_GATEWAY_TOKEN", env)
        self.assertEqual(Path(env["CODEX_HOME"]), unavailable)
        self.assertEqual((unavailable / "config.toml").read_bytes(), b"")
        self.assertFalse((unavailable / "AGENTS.md").exists())
