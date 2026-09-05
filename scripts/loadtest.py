#!/usr/bin/env python3
"""Run an isolated, resource-capped Docker SSH load test and retain evidence."""
import argparse
import datetime
import json
import os
from pathlib import Path
import subprocess
import threading
import time
import uuid

ROOT = Path(__file__).resolve().parent.parent


def command(*args, **kwargs):
    return subprocess.run(args, cwd=ROOT, check=True, text=True, **kwargs)


def output(*args):
    return command(*args, capture_output=True).stdout.strip()


def sample(server):
    files = ["/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.peak",
             "/sys/fs/cgroup/cpu.stat", "/sys/fs/cgroup/memory.events",
             "/sys/fs/cgroup/pids.current", "/proc/1/status"]
    lines = output("docker", "exec", server, "cat", *files).splitlines()
    result = {"time": time.time(), "memory_bytes": int(lines[0]), "peak_memory_bytes": int(lines[1])}
    for line in lines[2:]:
        fields = line.split()
        if len(fields) == 1 and fields[0].isdigit():
            result["tasks"] = int(fields[0])
        elif fields[0] in ("usage_usec", "nr_periods", "nr_throttled", "throttled_usec", "oom", "oom_kill"):
            result[fields[0]] = int(fields[1])
        elif fields[0] in ("VmRSS:", "VmHWM:"):
            result[fields[0].rstrip(":") + "_bytes"] = int(fields[1]) * 1024
    return result


def monitor(server, done, path, errors):
    with path.open("w") as stream:
        while not done.is_set():
            try:
                stream.write(json.dumps(sample(server)) + "\n")
                stream.flush()
            except (subprocess.SubprocessError, ValueError, KeyError) as err:
                errors.append(str(err))
                return
            done.wait(1)


def timestamp(value):
    # Python 3.9 accepts microseconds; Go emits up to nine fractional digits.
    base = datetime.datetime.fromisoformat(value[:19]).replace(tzinfo=datetime.timezone.utc).timestamp()
    return base + (float("0." + value[20:-1]) if "." in value else 0)


def summarize(evidence):
    samples = [json.loads(line) for line in (evidence / "resources.jsonl").read_text().splitlines()]
    events = [json.loads(line) for line in (evidence / "events.jsonl").read_text().splitlines()]
    indexed = {event["event"]: event for event in events}
    start = timestamp(indexed["movement_start"]["time"])
    end = timestamp(indexed["movement_complete"]["time"])
    active = [sample for sample in samples if start <= sample["time"] <= end]
    first, last = active[0], active[-1]
    log = (evidence / "server.log").read_text()
    state = json.loads((evidence / "final-state.json").read_text())[0]
    summary = {
        "peak_cgroup_mib": max(sample["peak_memory_bytes"] for sample in samples) / 2**20,
        "peak_server_rss_mib": max(sample["VmHWM_bytes"] for sample in samples) / 2**20,
        "final_cgroup_mib": samples[-1]["memory_bytes"] / 2**20,
        "movement_cpu_percent_one_core": (last["usage_usec"] - first["usage_usec"]) / (last["time"] - first["time"]) / 10000,
        "throttled_periods": samples[-1]["nr_throttled"],
        "oom_kills": samples[-1]["oom_kill"],
        "max_tasks_including_sampler": max(sample["tasks"] for sample in samples),
        "connected": log.count("player connected"),
        "disconnected": log.count("player disconnected"),
        "matches_started": log.count("match started"),
        "container_running": state["State"]["Running"],
        "container_oom_killed": state["State"]["OOMKilled"],
        "container_restarts": state["RestartCount"],
        "movement": indexed["movement_complete"],
        "recovery": indexed["recovery"],
        "capacity": indexed["capacity"],
    }
    (evidence / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    if summary["connected"] != 256 or summary["disconnected"] != 256:
        raise RuntimeError("Server log does not confirm cleanup of all 256 sessions")
    if summary["oom_kills"] or not summary["container_running"] or summary["container_restarts"]:
        raise RuntimeError("Server failed the resource/lifecycle checks")
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--duration", default="2m", help="movement duration, at most 2m30s")
    parser.add_argument("--skip-build", action="store_true", help="reuse locally built load-test binaries/image")
    args = parser.parse_args()
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    name = "bomber-load-" + uuid.uuid4().hex[:10]
    evidence = ROOT / ".local-test" / "loadtest" / stamp
    evidence.mkdir(parents=True)
    binary = evidence.parent / "client"
    image = "bomber-cli:loadtest"
    if not args.skip_build:
        command("docker", "build", "-t", image, ".")
        arch = output("docker", "image", "inspect", image, "--format", "{{.Architecture}}")
        command("go", "build", "-o", str(binary), "./scripts/loadtest",
                env={**os.environ, "GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"})
    server = None
    done = threading.Event()
    watcher = None
    errors = []
    try:
        server = output("docker", "run", "-d", "--name", name,
                        "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                        "--memory", "512m", "--memory-swap", "512m", "--cpus", "2", "--pids-limit", "256",
                        "--ulimit", "nofile=1024:1024", "--log-opt", "max-size=10m", "--log-opt", "max-file=3",
                        "--tmpfs", "/data:rw,noexec,nosuid,size=1m,uid=10001,gid=10001,mode=0700", image)
        (evidence / "container.json").write_text(output("docker", "inspect", server) + "\n")
        (evidence / "environment.json").write_text(json.dumps({
            "revision": output("git", "rev-parse", "HEAD"),
            "docker": output("docker", "info", "--format",
                             "{{.OSType}}/{{.Architecture}} CPUs={{.NCPU}} RAM={{.MemTotal}} Kernel={{.KernelVersion}} Docker={{.ServerVersion}}"),
            "duration": args.duration,
        }, indent=2) + "\n")
        watcher = threading.Thread(target=monitor, args=(server, done, evidence / "resources.jsonl", errors))
        watcher.start()
        time.sleep(3)
        # Separate cgroup: only the network namespace is shared with the server.
        with (evidence / "events.jsonl").open("w") as stream:
            process = subprocess.Popen([
                "docker", "run", "--rm", "--name", name + "-clients",
                "--network", "container:" + server, "--read-only", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges", "--memory", "1g", "--cpus", "4",
                "--ulimit", "nofile=2048:2048", "--mount", "type=bind,src=" + str(binary) + ",dst=/loadgen,readonly",
                "--entrypoint", "/loadgen", image, "-duration", args.duration,
            ], cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            try:
                for line in process.stdout:
                    print(line, end="", flush=True)
                    stream.write(line)
                    stream.flush()
                status = process.wait(timeout=10)
            finally:
                if process.poll() is None:
                    subprocess.run(["docker", "stop", "-t", "2", name + "-clients"], check=False, capture_output=True)
                    process.wait(timeout=10)
        done.set()
        watcher.join(timeout=10)
        # Docker logs writes application stderr to its own stderr.
        logs = subprocess.run(["docker", "logs", server], capture_output=True, text=True, check=True)
        (evidence / "server.log").write_text(logs.stdout + logs.stderr)
        (evidence / "final-state.json").write_text(output("docker", "inspect", server) + "\n")
        print("Evidence: " + str(evidence), flush=True)
        if errors:
            raise RuntimeError("Resource monitoring failed: " + "; ".join(errors))
        if status:
            raise RuntimeError("Load generator failed with status " + str(status))
        print(json.dumps(summarize(evidence), indent=2), flush=True)
    finally:
        done.set()
        if watcher:
            watcher.join(timeout=10)
        if server:
            # These containers were created by this invocation under unique names.
            subprocess.run(["docker", "rm", "-f", "-v", server], check=False, capture_output=True)


if __name__ == "__main__":
    main()
