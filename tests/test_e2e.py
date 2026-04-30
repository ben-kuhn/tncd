"""End-to-end integration tests using Direwolf and PAT."""

import asyncio
import json
import os
import re
import shutil
import signal
import socket
import subprocess
import tempfile
import time
from pathlib import Path

import pytest

pytestmark = [
    pytest.mark.skipif(
        not shutil.which("direwolf"), reason="direwolf not installed"
    ),
    pytest.mark.skipif(
        not shutil.which("pw-link"), reason="pipewire not available"
    ),
]

needs_pat = pytest.mark.skipif(
    not shutil.which("pat"), reason="pat not installed"
)


def free_port():
    """Return a free TCP port in the range Direwolf accepts (1024-49151)."""
    import random
    while True:
        port = random.randint(10000, 49151)
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            try:
                s.bind(("127.0.0.1", port))
                return port
            except OSError:
                continue


def wait_for_port(port, host="127.0.0.1", timeout=60.0):
    """Block until a TCP port is accepting connections."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((host, port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.1)
    raise TimeoutError(f"Port {port} not ready after {timeout}s")


def kill_proc(proc):
    """Terminate a subprocess, escalating to SIGKILL if needed."""
    if proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5)


def get_pw_port_ids(pid):
    """Find PipeWire port IDs for a process by its PID.

    Returns dict with 'output' and 'input' port ID lists.
    Direwolf creates:
      - alsa_playback.direwolf output ports (TX audio)
      - alsa_capture.direwolf input port (RX audio)

    PipeWire object hierarchy: Client (has PID) → Node (has client.id) → Port (has node.id)
    """
    result = subprocess.run(
        ["pw-dump"], capture_output=True, text=True, timeout=5,
    )
    data = json.loads(result.stdout)

    # Find client IDs belonging to this PID
    client_ids = set()
    for obj in data:
        props = obj.get("info", {}).get("props", {})
        if (props.get("application.process.id") == pid
                and obj.get("type") == "PipeWire:Interface:Client"):
            client_ids.add(obj["id"])

    # Find node IDs belonging to those clients
    node_ids = set()
    for obj in data:
        props = obj.get("info", {}).get("props", {})
        if (props.get("client.id") in client_ids
                and obj.get("type") == "PipeWire:Interface:Node"):
            node_ids.add(obj["id"])

    # Find ports belonging to those nodes
    output_ports = []
    input_ports = []
    for obj in data:
        if obj.get("type") != "PipeWire:Interface:Port":
            continue
        props = obj.get("info", {}).get("props", {})
        if props.get("node.id") not in node_ids:
            continue
        port_id = obj["id"]
        direction = obj.get("info", {}).get("direction")
        if direction == "output":
            output_ports.append(port_id)
        elif direction == "input":
            input_ports.append(port_id)

    return {"output": output_ports, "input": input_ports}


def pw_crosslink(pid_a, pid_b):
    """Cross-link two Direwolf instances' audio via PipeWire.

    Disconnects both from default audio devices, then links:
      - Direwolf-A playback output → Direwolf-B capture input
      - Direwolf-B playback output → Direwolf-A capture input
    """
    ports_a = get_pw_port_ids(pid_a)
    ports_b = get_pw_port_ids(pid_b)

    if not ports_a["output"] or not ports_a["input"]:
        raise RuntimeError(
            f"Direwolf PID {pid_a} missing PipeWire ports: {ports_a}"
        )
    if not ports_b["output"] or not ports_b["input"]:
        raise RuntimeError(
            f"Direwolf PID {pid_b} missing PipeWire ports: {ports_b}"
        )

    # Disconnect existing links for both processes
    for ports in [ports_a, ports_b]:
        for port_id in ports["output"] + ports["input"]:
            subprocess.run(
                ["pw-link", "-d", str(port_id)],
                capture_output=True, timeout=5,
            )

    # Cross-link: A output → B input (use first output port for mono)
    for out_id in ports_a["output"]:
        for in_id in ports_b["input"]:
            subprocess.run(
                ["pw-link", str(out_id), str(in_id)],
                capture_output=True, timeout=5, check=True,
            )

    # Cross-link: B output → A input
    for out_id in ports_b["output"]:
        for in_id in ports_a["input"]:
            subprocess.run(
                ["pw-link", str(out_id), str(in_id)],
                capture_output=True, timeout=5, check=True,
            )


def write_direwolf_config(path, mycall, agwport=0, kissport=0):
    """Write a Direwolf configuration file.

    Uses ADEVICE default — audio routing is handled by pw_crosslink()
    after both instances are started.
    """
    lines = [
        "ADEVICE default",
        "ACHANNELS 1",
        "ARATE 44100",
        f"MYCALL {mycall}",
        "MODEM 1200",
        # Disable default ports first (port 0), then set desired port
        "AGWPORT 0",
    ]
    if agwport:
        lines.append(f"AGWPORT {agwport}")
    lines.append("KISSPORT 0")
    if kissport:
        lines.append(f"KISSPORT {kissport}")
    lines.extend([
        "TXDELAY 10",
        "TXTAIL 5",
        "SLOTTIME 10",
        "PERSIST 63",
    ])
    Path(path).write_text("\n".join(lines) + "\n")


def parse_direwolf_pty(proc, timeout=10.0):
    """Read Direwolf stdout until the PTY path is found.

    Returns the PTY device path (e.g., /dev/pts/3).
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        line = proc.stdout.readline()
        if not line:
            time.sleep(0.1)
            continue
        line = line.decode(errors="replace").strip()
        m = re.search(r"Virtual KISS TNC is available on (/dev/pts/\d+)", line)
        if m:
            return m.group(1)
    raise TimeoutError("Direwolf did not report PTY path")


@pytest.fixture()
def direwolf_pair(tmp_path, request):
    """Start a pair of Direwolf instances with audio cross-linked via PipeWire.

    The fixture parameter 'kiss_mode' controls how Direwolf-A exposes KISS:
      - "tcp": KISS TCP port (default)
      - "pty": KISS pseudo-TTY

    Yields a dict with:
      - kiss_port_a: KISS TCP port for Direwolf-A (0 if PTY mode)
      - kiss_pty_a: PTY device path for Direwolf-A (None if TCP mode)
      - agwpe_port_b: AGWPE port for Direwolf-B
      - proc_a: Direwolf-A subprocess
      - proc_b: Direwolf-B subprocess
    """
    kiss_mode = getattr(request, "param", "tcp")

    agwpe_port_b = free_port()
    kiss_port_a = free_port() if kiss_mode == "tcp" else 0
    kiss_pty = kiss_mode == "pty"

    conf_a = tmp_path / "direwolf-a.conf"
    conf_b = tmp_path / "direwolf-b.conf"

    write_direwolf_config(conf_a, "N0CALL-1", agwport=0, kissport=kiss_port_a)
    write_direwolf_config(conf_b, "N0CALL-2", agwport=agwpe_port_b, kissport=0)

    # Log files to avoid stdout pipe buffer blocking Direwolf
    log_a = open(tmp_path / "direwolf-a.log", "w+b")
    log_b = open(tmp_path / "direwolf-b.log", "w+b")

    # Start Direwolf-A
    cmd_a = ["direwolf", "-c", str(conf_a), "-t", "0"]
    if kiss_pty:
        cmd_a.append("-p")

    # PTY mode needs stdout=PIPE so we can parse the PTY path
    proc_a = subprocess.Popen(
        cmd_a,
        stdout=subprocess.PIPE if kiss_pty else log_a,
        stderr=subprocess.STDOUT,
    )

    # Start Direwolf-B — log to file, we'll read it for APRS validation
    proc_b = subprocess.Popen(
        ["direwolf", "-c", str(conf_b), "-t", "0"],
        stdout=log_b,
        stderr=subprocess.STDOUT,
    )

    try:
        # Wait for readiness
        pty_path = None
        if kiss_pty:
            pty_path = parse_direwolf_pty(proc_a)
        elif kiss_port_a:
            wait_for_port(kiss_port_a)

        wait_for_port(agwpe_port_b)

        # Give PipeWire time to register both Direwolf nodes
        time.sleep(1.0)

        # Cross-link audio: A's TX → B's RX, B's TX → A's RX
        pw_crosslink(proc_a.pid, proc_b.pid)

        yield {
            "agwpe_port_b": agwpe_port_b,
            "kiss_port_a": kiss_port_a,
            "kiss_pty_a": pty_path,
            "proc_a": proc_a,
            "proc_b": proc_b,
            "log_b_path": tmp_path / "direwolf-b.log",
        }
    finally:
        kill_proc(proc_a)
        kill_proc(proc_b)
        log_a.close()
        log_b.close()


class TestDirewolfFixture:
    def test_direwolf_pair_starts(self, direwolf_pair):
        """Both Direwolf instances should be running after audio cross-link."""
        assert direwolf_pair["proc_a"].poll() is None
        assert direwolf_pair["proc_b"].poll() is None
        assert direwolf_pair["agwpe_port_b"] > 0
