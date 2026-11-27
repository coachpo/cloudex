#!/usr/bin/env python3
"""Launch Codex with this Workspace's gateway and a binding-specific home."""

import hashlib
import json
import os
from pathlib import Path
import secrets
import sys
import tempfile
from urllib.error import HTTPError
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener


CODEX = "/usr/local/libexec/cloudex-codex"
CREDENTIAL = Path("/run/cloudex/gateway.json")
CATALOG_LIMIT = 16 * 1024 * 1024

GATEWAY_ERRORS = {
    "instance_not_verified": "The Workspace container is waiting for gateway credential verification.",
    "reauth_required": "The bound authorization needs to be reauthorized.",
    "missing_inference_scope": "The bound authorization lacks plan inference permission.",
    "catalog_route_unverified": "The bound authorization's model catalog route is unverified.",
    "codex_route_unavailable": "The bound authorization's Codex route is unavailable.",
    "model_incompatible": "The model is incompatible with this authorization channel.",
    "no_compatible_models": "The bound authorization has no compatible models.",
}


class GatewayUnavailable(Exception):
    pass


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def load_credential():
    try:
        return json.loads(CREDENTIAL.read_text())
    except FileNotFoundError:
        raise GatewayUnavailable("This Workspace has no active Cloudex gateway credential. Check its authorization binding.")


def load_catalog(base, token):
    request = Request(base + "/models", headers={"Authorization": "Bearer " + token})
    try:
        with build_opener(ProxyHandler({}), NoRedirect()).open(request, timeout=15) as response:
            catalog = response.read(CATALOG_LIMIT + 1)
    except HTTPError as error:
        try:
            code = json.loads(error.read(4096))["error"]["code"]
        except (OSError, ValueError, KeyError, TypeError):
            code = None
        finally:
            error.close()
        if isinstance(code, str) and code in GATEWAY_ERRORS:
            raise GatewayUnavailable(GATEWAY_ERRORS[code])
        if error.code == 401:
            raise GatewayUnavailable("The Workspace gateway credential is inactive. Check its authorization binding.")
        raise GatewayUnavailable(f"The Cloudex model catalog request failed (HTTP {error.code}).")
    except OSError:
        raise GatewayUnavailable("Unable to reach the Cloudex model catalog. Check gateway DNS and TLS.")
    try:
        if len(catalog) > CATALOG_LIMIT:
            raise ValueError()
        models = json.loads(catalog)["models"]
        if not isinstance(models, list):
            raise ValueError()
        if not models:
            raise GatewayUnavailable("The bound authorization has no compatible models.")
        model = models[0]["slug"]
        if not isinstance(model, str) or not model:
            raise ValueError()
    except (ValueError, KeyError, TypeError):
        raise GatewayUnavailable("The Cloudex model catalog is invalid.")
    return catalog, model


def atomic_write(target, data):
    with tempfile.NamedTemporaryFile(dir=target.parent, delete=False) as temporary:
        path = Path(temporary.name)
        try:
            temporary.write(data)
            temporary.flush()
            path.replace(target)
        finally:
            path.unlink(missing_ok=True)


def configure(args):
    try:
        credential = load_credential()
        base = credential["origin"].rstrip("/") + "/g/v1"
        token = credential["token"]
        catalog, model = load_catalog(base, token)
        namespace = f"b{credential['binding_revision']:d}-g{credential['grant_epoch']:d}"
        note = "Check the Workspace's Cloudex authorization binding."
    except GatewayUnavailable as error:
        if not args or args[0] != "app-server":
            raise
        # Desktop can initialize a protocol connection while the Workspace is
        # unbound. The custom provider requires a credential before any request.
        print(error, file=sys.stderr)
        base, token, catalog, model, namespace = "https://127.0.0.1:1/g/v1", None, None, None, "unavailable"
        note = str(error)

    os.umask(0o077)
    home = Path.home().resolve() / ".codex" / "cloudex" / namespace
    home.mkdir(parents=True, exist_ok=True)
    quote = lambda value: json.dumps(value, ensure_ascii=False)
    configuration = f'''model_provider = "cloudex"
cli_auth_credentials_store = "file"

[shell_environment_policy]
exclude = ["CLOUDEX_GATEWAY_TOKEN"]

[model_providers.cloudex]
name = "Cloudex"
base_url = {quote(base)}
wire_api = "responses"
env_key = "CLOUDEX_GATEWAY_TOKEN"
env_key_instructions = {quote(note)}
requires_openai_auth = false
supports_websockets = true
request_max_retries = 0
stream_max_retries = 0

[model_providers.cloudex.env_http_headers]
x-cloudex-run-id = "CLOUDEX_GATEWAY_RUN_ID"
'''
    atomic_write(home / "config.toml", configuration.encode())
    env = os.environ.copy()
    for name in ("OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_SQLITE_HOME", "CLOUDEX_GATEWAY_TOKEN"):
        env.pop(name, None)
    env["CODEX_HOME"] = str(home)
    env["CLOUDEX_GATEWAY_RUN_ID"] = secrets.token_hex(16)
    overrides = []
    if token is not None:
        path = home / ("models-" + hashlib.sha256(catalog).hexdigest() + ".json")
        atomic_write(path, catalog)
        overrides = ["-c", "model_catalog_json=" + quote(str(path)), "-c", "model=" + quote(model)]
        env["CLOUDEX_GATEWAY_TOKEN"] = token
    os.execve(CODEX, [CODEX, *overrides, *args], env)


def main(args):
    if args in (["--version"], ["-V"], ["--help"], ["-h"]):
        os.execv(CODEX, [CODEX, *args])
    if args and args[0] in ("login", "logout"):
        raise GatewayUnavailable("Manage Codex authorization in the Cloudex console.")
    configure(args)


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except GatewayUnavailable as error:
        print(error, file=sys.stderr)
        sys.exit(1)
    except (OSError, ValueError, KeyError, TypeError):
        print("Unable to prepare the Workspace Codex configuration.", file=sys.stderr)
        sys.exit(1)
