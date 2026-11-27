#!/usr/bin/env python3
"""Internal workspace helper; invoked by Docker Exec as the workspace user."""

import ctypes
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import tempfile
import time


GIT_CREDENTIAL_FILE = Path("/etc/cloudex/git-credential.json")
GATEWAY_DIR = Path("/run/cloudex")
GATEWAY_FILE = GATEWAY_DIR / "gateway.json"
BOOT_MARKER_FILE = GATEWAY_DIR / "boot"
WORKSPACE_UID = 10001


def gateway_mount():
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        mount, filesystem = line.split(" - ", 1)
        if mount.split()[4] == str(GATEWAY_DIR) and filesystem.split()[0] == "tmpfs":
            return
    raise OSError("workspace gateway requires its own tmpfs")


def gateway_init():
    gateway_mount()
    try:
        with BOOT_MARKER_FILE.open("x") as marker:
            marker.write(secrets.token_hex(16))
    except FileExistsError:
        pass
    return 0


def gateway_credential():
    try:
        return json.loads(GATEWAY_FILE.read_text())
    except FileNotFoundError:
        return None


def gateway_confirmation(marker, record):
    response = dict(boot_marker=marker)
    if record is not None:
        response.update(sequence=record["sequence"], started_at=record["started_at"],
                        origin=record["origin"], binding_revision=record["binding_revision"],
                        grant_id=record["grant_id"], grant_epoch=record["grant_epoch"],
                        digest=hashlib.sha256(record["token"].encode("ascii")).hexdigest())
    print(json.dumps(response))


def gateway_state():
    gateway_mount()
    gateway_confirmation(BOOT_MARKER_FILE.read_text(), gateway_credential())
    return 0


def gateway_install():
    gateway_mount()
    # One line over Docker attach stdin. Docker doesn't persist the stream.
    data = sys.stdin.buffer.readline(4097)
    try:
        if len(data) > 4096 or not data.endswith(b"\n"):
            raise ValueError()
        record = json.loads(data)
        marker = BOOT_MARKER_FILE.read_text()
        if (marker != record["boot_marker"]
                or not re.fullmatch(r"[A-Za-z0-9_-]{43}", record["token"])
                or not re.fullmatch(r"[0-9a-f]{32}", record["grant_id"])):
            raise ValueError()
        with (GATEWAY_DIR / "install.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            current = gateway_credential()
            if current is not None and current["sequence"] >= record["sequence"]:
                if current != record:
                    raise ValueError()
            else:
                with tempfile.NamedTemporaryFile(dir=GATEWAY_DIR, delete=False) as temporary:
                    path = Path(temporary.name)
                    try:
                        temporary.write(data)
                        temporary.flush()
                        os.fchown(temporary.fileno(), 0, WORKSPACE_UID)
                        os.fchmod(temporary.fileno(), 0o440)
                        path.replace(GATEWAY_FILE)
                    finally:
                        path.unlink(missing_ok=True)
        gateway_confirmation(marker, record)
        return 0
    except (KeyError, OSError, TypeError, ValueError):
        print("Workspace gateway credential installation was rejected", file=sys.stderr)
        return 1


def git_credential(operation):
    if operation != "get":
        return 0
    request = {}
    for line in sys.stdin:
        line = line.rstrip("\n")
        if not line:
            break
        key, _, value = line.partition("=")
        request[key] = value
    if request.get("protocol") != "https":
        return 0
    try:
        credential = json.loads(GIT_CREDENTIAL_FILE.read_text())
    except FileNotFoundError:
        return 0
    if request.get("host") == credential["host"]:
        print("username=" + credential["username"])
        print("password=" + credential["token"])
    return 0


def initialize(url, branch):
    parent = Path("/workspace")
    project = parent / "project"
    staging = parent / ".cloudex-initializing"
    with (parent / ".cloudex-init-lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        # Existing project data is authoritative, even if the owner removed .git.
        if project.is_dir():
            return 0
        if staging.exists():
            shutil.rmtree(staging)
        if url:
            env = dict(os.environ, GIT_TERMINAL_PROMPT="0")
            subprocess.run(["git", "clone", "--branch", branch, "--", url, staging],
                           env=env, check=True, stdin=subprocess.DEVNULL, stdout=sys.stderr)
        else:
            subprocess.run(["git", "init", "--quiet", "--initial-branch", branch, staging], check=True)
            subprocess.run(["git", "-C", staging, "-c", "user.name=Workspace",
                            "-c", "user.email=workspace@localhost", "-c", "commit.gpgsign=false",
                            "commit", "--quiet", "--allow-empty", "-m", "Initialize workspace"], check=True)
        staging.rename(project)
    return 0


def git_snapshot():
    budget = 32 * 1024
    truncated = False

    def capture(*args):
        nonlocal budget, truncated
        with subprocess.Popen(["git", *args], stdout=subprocess.PIPE,
                              stderr=subprocess.DEVNULL) as proc:
            data = proc.stdout.read(budget + 1)
            if len(data) > budget:
                data = data[:budget]
                truncated = True
                proc.kill()
                proc.wait()
            elif proc.wait() != 0:
                raise ValueError("Git snapshot unavailable")
            budget -= len(data)
            return data.decode("utf-8", "replace")

    head = capture("rev-parse", "HEAD").strip()
    status = capture("status", "--short", "--untracked-files=normal")
    patch = capture("diff", "--no-color", "--no-ext-diff", "--no-textconv", head, "--")
    print(json.dumps(dict(head=head, status=status, patch=patch, truncated=truncated), ensure_ascii=False))
    return 0


def directory(session_id):
    if not re.fullmatch(r"[0-9a-f]{32}", session_id):
        raise ValueError("invalid terminal session ID")
    root = Path("/tmp/cloudex-terminal-sessions")
    root.mkdir(mode=0o700, exist_ok=True)
    path = root / session_id
    path.mkdir(mode=0o700, exist_ok=True)
    return path


def children(pid):
    """Include children created by worker threads, not only the main thread."""
    result = set()
    for path in Path(f"/proc/{pid}/task").glob("*/children"):
        try:
            result.update(map(int, path.read_text().split()))
        except FileNotFoundError:
            pass
    return result


def stop_children(sig, deadline):
    me = os.getpid()
    while pids := children(me):
        visited = set()
        while pids:
            pid = pids.pop()
            if pid in visited:
                continue
            visited.add(pid)
            try:
                os.kill(pid, sig)
            except ProcessLookupError:
                continue
            pids.update(children(pid) - visited)
        while True:
            try:
                if os.waitpid(-1, os.WNOHANG)[0] == 0:
                    break
            except ChildProcessError:
                break
        if time.monotonic() >= deadline:
            return not children(me)
        time.sleep(0.05)
    return True


def supervise(path):
    # A subreaper adopts daemonized descendants so cancellation can wait for them.
    if ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "enable child subreaper")
    stopping = False

    def request_stop(*_):
        nonlocal stopping
        stopping = True

    for sig in (signal.SIGHUP, signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, request_stop)
    with (path / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if (path / "cancel").exists() or stopping:
            return 130
        lease = path / "lease"
        lease.touch()
        # Bash must inherit the Docker controlling tty so job control works.
        proc = subprocess.Popen(["bash", "-li"])
        try:
            while proc.poll() is None:
                if stopping or (path / "cancel").exists() or time.time() - lease.stat().st_mtime > 65:
                    stopping = True
                    break
                time.sleep(0.1)
        finally:
            stop_children(signal.SIGTERM, time.monotonic() + 0.6)
            # Hold the lock until the last descendant exits. The caller can
            # time out without misreporting a worker stuck in kernel I/O.
            while not stop_children(signal.SIGKILL, time.monotonic() + 1):
                pass
        return 130 if stopping else (proc.returncode or 0)


def cancel(path):
    # Write the marker before waiting for the lock, covering a late exec start.
    (path / "cancel").touch()
    with (path / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
    return 0


def main():
    action, *args = sys.argv[1:]
    if action == "init":
        return initialize(*args)
    if action == "git-credential":
        return git_credential(*args)
    if action == "gateway-init":
        return gateway_init()
    if action == "gateway-state":
        return gateway_state()
    if action == "gateway-install":
        return gateway_install()
    if action == "git":
        return git_snapshot(*args)
    if action == "terminal":
        return supervise(directory(args[0]))
    if action == "renew":
        (directory(args[0]) / "lease").touch()
        return 0
    if action == "cancel":
        return cancel(directory(args[0]))
    raise ValueError("unknown workspace operation")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(error, file=sys.stderr)
        sys.exit(1)
