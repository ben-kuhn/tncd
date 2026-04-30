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
        not shutil.which("pw-loopback"), reason="pipewire not available"
    ),
]

needs_pat = pytest.mark.skipif(
    not shutil.which("pat"), reason="pat not installed"
)


def free_port():
    """Return a free TCP port number."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for_port(port, host="127.0.0.1", timeout=10.0):
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


@pytest.fixture()
def pipewire_audio_link():
    """Create two PipeWire loopback nodes for bidirectional audio.

    Returns (sink_ab, source_ab, sink_ba, source_ba) node names.
    Direwolf-A plays into sink_ab, Direwolf-B captures from source_ab.
    Direwolf-B plays into sink_ba, Direwolf-A captures from source_ba.
    """
    procs = []
    # Loopback A→B: Direwolf-A output → Direwolf-B input
    ab = subprocess.Popen(
        [
            "pw-loopback",
            "--name", "e2e-ab",
            "--channels", "1",
            "--channel-map", "[ MONO ]",
            "--playback-props", "media.class=Audio/Sink node.name=e2e-sink-ab",
            "--capture-props", "media.class=Audio/Source node.name=e2e-source-ab",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    procs.append(ab)

    # Loopback B→A: Direwolf-B output → Direwolf-A input
    ba = subprocess.Popen(
        [
            "pw-loopback",
            "--name", "e2e-ba",
            "--channels", "1",
            "--channel-map", "[ MONO ]",
            "--playback-props", "media.class=Audio/Sink node.name=e2e-sink-ba",
            "--capture-props", "media.class=Audio/Source node.name=e2e-source-ba",
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    procs.append(ba)

    # Give PipeWire time to register the nodes
    time.sleep(1.0)

    try:
        yield {
            "a_playback": "e2e-sink-ab",
            "a_capture": "e2e-source-ba",
            "b_playback": "e2e-sink-ba",
            "b_capture": "e2e-source-ab",
        }
    finally:
        for p in procs:
            kill_proc(p)


class TestPipeWireFixture:
    def test_loopback_nodes_created(self, pipewire_audio_link):
        """Verify pw-loopback nodes exist in PipeWire."""
        result = subprocess.run(
            ["pw-cli", "list-objects"],
            capture_output=True, text=True, timeout=5,
        )
        assert "e2e-sink-ab" in result.stdout
        assert "e2e-source-ab" in result.stdout
        assert "e2e-sink-ba" in result.stdout
        assert "e2e-source-ba" in result.stdout
