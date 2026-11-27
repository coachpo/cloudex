from contextlib import redirect_stderr
from http.client import HTTPResponse
import importlib.util
import io
from pathlib import Path
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import Mock, patch
from urllib.error import HTTPError


spec = importlib.util.spec_from_file_location("workspace_codex", Path(__file__).with_name("workspace-codex.py"))
workspace_codex = importlib.util.module_from_spec(spec)
spec.loader.exec_module(workspace_codex)


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
        credential = {"origin": "https://gateway.example.test", "token": "fixture", "grant_id": "grant", "grant_epoch": 1}
        stderr = io.StringIO()
        with (
            TemporaryDirectory() as directory,
            patch.object(Path, "home", return_value=Path(directory)),
            patch.object(workspace_codex, "os") as launch,
            patch.object(workspace_codex, "load_credential", return_value=credential),
            patch.object(workspace_codex, "build_opener") as build_opener,
            redirect_stderr(stderr),
        ):
            launch.environ = {}
            build_opener.return_value.open.return_value = interrupted_response(b"200 OK")

            workspace_codex.main(["app-server"])

            launch.execve.assert_called_once()
            _, args, env = launch.execve.call_args.args
            self.assertEqual(args[1:], ["app-server"])
            self.assertNotIn("CLOUDEX_GATEWAY_TOKEN", env)
            self.assertEqual(Path(env["CODEX_HOME"]).name, "unavailable")
            self.assertIn("Unable to reach the Cloudex model catalog", stderr.getvalue())

    def test_incomplete_error_body_preserves_http_status(self):
        response = interrupted_response(b"503 Service Unavailable")
        error = HTTPError("https://gateway.example.test/g/v1/models", 503, response.reason, response.headers, response)
        with patch.object(workspace_codex, "build_opener") as build_opener:
            build_opener.return_value.open.side_effect = error

            with self.assertRaisesRegex(workspace_codex.GatewayUnavailable, "HTTP 503"):
                workspace_codex.load_catalog("https://gateway.example.test/g/v1", "fixture")
