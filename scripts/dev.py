#!/usr/bin/env python3
"""Local verification tools. Never manage the normal server on port 2323."""
import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import re
import select
import signal
import socket
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parent.parent
STATE = ROOT / ".local-test"
BINARY = STATE / "bomber-cli"
PORT = 23230
ADDRESS = ("127.0.0.1", PORT)


def run(*args):
    subprocess.run(args, cwd=ROOT, check=True)


def identity(pid):
    result = subprocess.run(
        ["ps", "-p", str(pid), "-o", "lstart=", "-o", "command="],
        capture_output=True, text=True, check=False,
    )
    if result.returncode not in (0, 1):
        raise RuntimeError("Cannot inspect process identity: " + result.stderr.strip())
    return result.stdout.strip() if result.returncode == 0 else ""


def alive(record):
    current = identity(record["pid"])
    return bool(current) and current == record["identity"]


def read_record(path):
    if not path.exists():
        return None
    record = json.loads(path.read_text())
    if not isinstance(record.get("pid"), int) or record["pid"] <= 1:
        raise RuntimeError("Invalid process record: " + str(path))
    if record.get("kind") not in ("server", "ssh"):
        raise RuntimeError("Invalid process kind: " + str(path))
    # Records must identify a command scoped to this workspace's test data.
    if str(STATE) not in record.get("identity", ""):
        raise RuntimeError("Process record is not scoped to this workspace")
    return record


def remember(process, kind):
    fingerprint = identity(process.pid)
    if not fingerprint or process.poll() is not None:
        raise RuntimeError(kind + " process exited during startup")
    record = {"pid": process.pid, "identity": fingerprint, "kind": kind}
    path = STATE / ("server.json" if kind == "server" else "ssh-%d.json" % process.pid)
    path.write_text(json.dumps(record) + "\n")
    return path, record


@contextlib.contextmanager
def locked():
    STATE.mkdir(mode=0o700, exist_ok=True)
    with (STATE / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def terminate(record):
    if not alive(record):
        return
    try:
        os.kill(record["pid"], signal.SIGTERM)
    except ProcessLookupError:
        return
    for _ in range(50):
        if not alive(record):
            return
        time.sleep(0.1)
    if alive(record):
        # A stale/reused PID never reaches this branch unless its full launch
        # time and command also match the process originally recorded here.
        try:
            os.kill(record["pid"], signal.SIGKILL)
        except ProcessLookupError:
            pass


def build():
    STATE.mkdir(mode=0o700, exist_ok=True)
    run("go", "build", "-o", str(BINARY), "./cmd/bomber-cli")


def test():
    run("go", "test", "-race", "./...")
    run("go", "vet", "./...")
    build()


def start():
    with locked():
        path = STATE / "server.json"
        record = read_record(path)
        if record and alive(record):
            print("Test server already running (PID %d, port %d)." % (record["pid"], PORT))
            return False
        path.unlink(missing_ok=True)
        with socket.socket() as probe:
            try:
                probe.bind(ADDRESS)
            except OSError as err:
                raise RuntimeError("Port %d is in use; refusing to stop an untracked server." % PORT) from err
        build()
        with (STATE / "server.log").open("wb") as log:
            process = subprocess.Popen(
                [str(BINARY), "--listen", "127.0.0.1:%d" % PORT,
                 "--host-key", str(STATE / "ssh_host_ed25519_key")],
                cwd=ROOT, stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                start_new_session=True,
            )
        try:
            remember(process, "server")
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError("Server exited; see " + str(STATE / "server.log"))
                if "INFO listening" in (STATE / "server.log").read_text():
                    print("Test server started (PID %d): ssh on 127.0.0.1:%d" % (process.pid, PORT))
                    print("Log: " + str(STATE / "server.log"))
                    return True
                time.sleep(0.05)
            raise RuntimeError("Server did not become ready within 5 seconds")
        except BaseException:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            path.unlink(missing_ok=True)
            raise


def ssh_command(name):
    return ["ssh", "-F", "/dev/null", "-tt", "-p", str(PORT),
            "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=" + str(STATE / "known_hosts"),
            name + "@127.0.0.1"]


def connect(name, smoke=False):
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,16}", name):
        raise RuntimeError("Use a player name of 1-16 ASCII letters, digits, underscores or hyphens")
    with locked():
        server = read_record(STATE / "server.json")
        if not server or not alive(server):
            raise RuntimeError("No managed test server. Run ./scripts/dev.sh start first.")
        if not smoke and sys.stdin.isatty():
            # Some automation PTYs have no initial size. Preserve real sizes.
            size = os.get_terminal_size(sys.stdin.fileno())
            if size.columns == 0 or size.lines == 0:
                run("stty", "rows", "24", "cols", "60")
        options = dict(stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT) if smoke else {}
        process = subprocess.Popen(ssh_command(name), cwd=ROOT, **options)
        try:
            path, record = remember(process, "ssh")
        except BaseException:
            process.terminate()
            process.wait()
            raise
    try:
        if smoke:
            transcript = b""
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                readable, _, _ = select.select([process.stdout], [], [], 0.2)
                if readable:
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk:
                        break
                    transcript += chunk
                    # With no local PTY, SSH requests a zero-size terminal. Both
                    # screens prove the real interactive SSH app was reached.
                    if b"LOBBY" in transcript or b"Resize terminal" in transcript:
                        process.stdin.write(b"\x03")
                        process.stdin.flush()
                        if process.wait(timeout=5) != 0:
                            raise RuntimeError("SSH smoke session exited unsuccessfully")
                        print("SSH smoke check passed; Ctrl-C closed the session.")
                        return
            raise RuntimeError("SSH smoke check did not reach the UI:\n" + transcript.decode(errors="replace"))
        status = process.wait()
        if status not in (0, -signal.SIGTERM):
            # OpenSSH handles SIGTERM itself and commonly exits 255. Cleanup
            # removes its record under this lock; that is an expected exit.
            with locked():
                if path.exists():
                    raise RuntimeError("SSH exited with status %d" % status)
    except KeyboardInterrupt:
        pass
    finally:
        with locked():
            if process.poll() is None:
                terminate(record)
            process.wait()
            path.unlink(missing_ok=True)


def stop(sessions_only=False):
    with locked():
        paths = sorted(STATE.glob("ssh-*.json"))
        if not sessions_only:
            paths.append(STATE / "server.json")
        for path in paths:
            record = read_record(path)
            if record:
                if alive(record):
                    print("Stopping %s PID %d." % (record["kind"], record["pid"]))
                    terminate(record)
                else:
                    print("Removing stale record for PID %d; no signal sent." % record["pid"])
                path.unlink(missing_ok=True)
        print("Managed SSH sessions stopped." if sessions_only else "Managed test server and SSH sessions stopped.")


def status():
    with locked():
        found = False
        for path in sorted(STATE.glob("*.json")):
            record = read_record(path)
            if record:
                found = True
                print("%s PID %d: %s" % (record["kind"], record["pid"], "running" if alive(record) else "stale"))
        if not found:
            print("No managed test processes.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for command, help_text in (
        ("test", "race tests, vet, and build"), ("build", "build private test executable"),
        ("start", "build and start test server on loopback port 23230"),
        ("status", "show tracked process status"),
        ("latency", "measure keypress-to-render latency through real SSH"),
        ("stress", "repeat SSH churn, shutdown, and host-key failure tests under the race detector"),
        ("stop", "stop tracked SSH sessions and server"),
        ("kill-sessions", "stop tracked SSH clients, keeping server running"),
        ("smoke", "start server if needed and verify real SSH UI/exit; clean up if started"),
    ):
        commands.add_parser(command, help=help_text)
    client = commands.add_parser("ssh", help="open a tracked interactive SSH session")
    client.add_argument("name", nargs="?", default="player")
    args = parser.parse_args()
    os.umask(0o077)
    if args.command == "ssh":
        connect(args.name)
    elif args.command == "latency":
        run("go", "test", "-race", "-v", "./internal/host", "-run",
            "^TestRealSSHMultiplayerResizeAndDisconnect$", "-count=1")
    elif args.command == "stress":
        run("go", "test", "-race", "-count=10", "-timeout=3m",
            "./internal/host", "./internal/room", "-run",
            "^(TestSSH|TestHostKey(ConcurrentCreation|WriteFailure|FailurePreservesExistingFiles)|TestCloseReleasesRoomsAndSessions)")
    elif args.command == "kill-sessions":
        stop(sessions_only=True)
    elif args.command == "smoke":
        created = start()
        try:
            connect("smoke", smoke=True)
        finally:
            if created:
                stop()
    else:
        {"test": test, "build": build, "start": start, "stop": stop, "status": status}[args.command]()


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.SubprocessError) as error:
        print("Error: " + str(error), file=sys.stderr)
        sys.exit(1)
