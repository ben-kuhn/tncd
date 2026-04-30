"""End-to-end integration tests using Direwolf and PAT."""

import asyncio
import json
import os
import re
import shutil
import signal
import sys
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


def get_pw_ports(pid):
    """Find PipeWire port IDs for a Direwolf process by its PID.

    Returns dict with:
      - 'tx_output': playback output port ID (TX audio out, first channel)
      - 'rx_input': capture input port ID (RX audio in)
      - 'all': list of all port IDs (for disconnecting defaults)

    PipeWire object hierarchy: Client (has PID) → Node (has client.id) → Port (has node.id)
    Direwolf creates two nodes:
      - alsa_playback.direwolf: output_FL, output_FR (TX audio)
      - alsa_capture.direwolf: input_MONO (RX audio), monitor_MONO
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

    # Find node IDs and their names belonging to those clients
    nodes = {}  # node_id -> node_name
    for obj in data:
        props = obj.get("info", {}).get("props", {})
        if (props.get("client.id") in client_ids
                and obj.get("type") == "PipeWire:Interface:Node"):
            nodes[obj["id"]] = props.get("node.name", "")

    # Find relevant ports
    tx_output = None
    rx_input = None
    all_ports = []
    for obj in data:
        if obj.get("type") != "PipeWire:Interface:Port":
            continue
        props = obj.get("info", {}).get("props", {})
        node_id = props.get("node.id")
        if node_id not in nodes:
            continue
        port_id = obj["id"]
        port_name = props.get("port.name", "")
        node_name = nodes[node_id]
        all_ports.append(port_id)

        # TX: first output channel from playback node
        if "playback" in node_name and port_name == "output_FL":
            tx_output = port_id
        # RX: input from capture node
        if "capture" in node_name and port_name == "input_MONO":
            rx_input = port_id

    return {"tx_output": tx_output, "rx_input": rx_input, "all": all_ports}


def pw_disconnect_links(port_ids, pw_data):
    """Disconnect all PipeWire links involving the given port IDs."""
    port_set = set(port_ids)
    for obj in pw_data:
        if obj.get("type") != "PipeWire:Interface:Link":
            continue
        props = obj.get("info", {}).get("props", {})
        out_port = props.get("link.output.port")
        in_port = props.get("link.input.port")
        if out_port in port_set or in_port in port_set:
            link_id = obj["id"]
            subprocess.run(
                ["pw-link", "-d", str(link_id)],
                capture_output=True, timeout=5,
            )


def pw_crosslink(pid_a, pid_b):
    """Cross-link two Direwolf instances' audio via PipeWire.

    Disconnects both from default audio devices, then links:
      - Direwolf-A TX output → Direwolf-B RX input
      - Direwolf-B TX output → Direwolf-A RX input
    """
    ports_a = get_pw_ports(pid_a)
    ports_b = get_pw_ports(pid_b)

    if not ports_a["tx_output"] or not ports_a["rx_input"]:
        raise RuntimeError(
            f"Direwolf PID {pid_a} missing PipeWire ports: {ports_a}"
        )
    if not ports_b["tx_output"] or not ports_b["rx_input"]:
        raise RuntimeError(
            f"Direwolf PID {pid_b} missing PipeWire ports: {ports_b}"
        )

    # Get full pw-dump data for finding links
    result = subprocess.run(
        ["pw-dump"], capture_output=True, text=True, timeout=5,
    )
    pw_data = json.loads(result.stdout)

    # Disconnect all existing links for both processes
    pw_disconnect_links(ports_a["all"] + ports_b["all"], pw_data)

    # Cross-link: A TX → B RX
    subprocess.run(
        ["pw-link", str(ports_a["tx_output"]), str(ports_b["rx_input"])],
        capture_output=True, timeout=5, check=True,
    )

    # Cross-link: B TX → A RX
    subprocess.run(
        ["pw-link", str(ports_b["tx_output"]), str(ports_a["rx_input"])],
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
        "FULLDUP ON",
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


def write_tncd_config(path, agwpe_port, kiss_type, kiss_host=None,
                      kiss_port=None, kiss_device=None, callsign="N0CALL-1"):
    """Write a tncd INI configuration file."""
    lines = [
        "[server]",
        "listen_host = 127.0.0.1",
        f"listen_port = {agwpe_port}",
        f"callsign = {callsign}",
        "",
        "[client]",
        f"type = {kiss_type}",
    ]
    if kiss_type == "tcp":
        lines.append(f"host = {kiss_host}")
        lines.append(f"port = {kiss_port}")
    elif kiss_type == "serial":
        lines.append(f"device = {kiss_device}")
        lines.append("baudrate = 9600")
    lines.extend([
        "",
        "[kiss]",
        "tx_delay = 10",
        "persistence = 63",
        "slot_time = 10",
        "tx_tail = 5",
        "full_duplex = 1",
    ])
    Path(path).write_text("\n".join(lines) + "\n")


@pytest.fixture()
def tncd_instance(direwolf_pair, tmp_path):
    """Start tncd connected to Direwolf-A's KISS interface.

    Yields a dict with:
      - agwpe_port: tncd's AGWPE listen port
      - proc: tncd subprocess
    """
    agwpe_port = free_port()
    config_path = tmp_path / "tncd.ini"

    if direwolf_pair["kiss_pty_a"]:
        write_tncd_config(
            config_path, agwpe_port, "serial",
            kiss_device=direwolf_pair["kiss_pty_a"],
        )
    else:
        write_tncd_config(
            config_path, agwpe_port, "tcp",
            kiss_host="127.0.0.1",
            kiss_port=direwolf_pair["kiss_port_a"],
        )

    proc = subprocess.Popen(
        [sys.executable, "tncd.py", "-c", str(config_path)],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )

    try:
        wait_for_port(agwpe_port)
        yield {
            "agwpe_port": agwpe_port,
            "proc": proc,
        }
    finally:
        kill_proc(proc)


class TestDirewolfFixture:
    def test_direwolf_pair_starts(self, direwolf_pair):
        """Both Direwolf instances should be running after audio cross-link."""
        assert direwolf_pair["proc_a"].poll() is None
        assert direwolf_pair["proc_b"].poll() is None
        assert direwolf_pair["agwpe_port_b"] > 0


class TestTncdFixture:
    def test_tncd_accepts_connections(self, tncd_instance):
        """tncd should accept TCP connections on its AGWPE port."""
        port = tncd_instance["agwpe_port"]
        with socket.create_connection(("127.0.0.1", port), timeout=5):
            pass  # Connection succeeded


class TestAPRS:
    """Test APRS/UI frame forwarding through tncd."""

    def test_aprs_packets(self, tncd_instance, direwolf_pair):
        """Send APRS position, message, and telemetry through tncd,
        verify Direwolf-B decodes them."""
        import threading
        from tests.emulator_agwpe import AGWPEClientEmulator, create_agwpe_frame

        agwpe_port = tncd_instance["agwpe_port"]
        log_b_path = direwolf_pair["log_b_path"]

        # Send APRS packets through tncd
        async def send_packets():
            client = AGWPEClientEmulator("127.0.0.1", agwpe_port)
            await client.connect()

            # Register callsign first (X frame)
            reg = create_agwpe_frame(0, ord(b'X'), 'N0CALL-1', '', b'')
            client.writer.write(reg)
            await client.writer.drain()
            await client.receive(timeout=2.0)

            # Allow audio link to settle
            await asyncio.sleep(3.0)

            # Position report
            await client.send_unproto(
                "N0CALL-1", "APRS",
                b"!4903.50N/07201.75W-Test Position",
            )
            await asyncio.sleep(5.0)

            # Message
            await client.send_unproto(
                "N0CALL-1", "APRS",
                b":BLN1     :Test E2E message",
            )
            await asyncio.sleep(5.0)

            # Telemetry
            await client.send_unproto(
                "N0CALL-1", "APRS",
                b"T#001,100,200,300,400,500,10000000",
            )
            await asyncio.sleep(5.0)

            await client.close()

        asyncio.run(send_packets())

        # Read Direwolf-B's log and verify packets arrived
        log_content = Path(log_b_path).read_text(errors="replace")

        assert "4903.50N" in log_content, (
            f"Position packet not found in Direwolf-B output:\n{log_content}"
        )
        assert "BLN1" in log_content, (
            f"Message packet not found in Direwolf-B output:\n{log_content}"
        )
        assert "T#001" in log_content, (
            f"Telemetry packet not found in Direwolf-B output:\n{log_content}"
        )


def write_pat_config(config_dir, mailbox_dir, mycall, locator, agwpe_addr):
    """Write a minimal PAT configuration file."""
    config = {
        "mycall": mycall,
        "secure_login_password": "TESTPASSWORD",
        "locator": locator,
        "service_codes": ["PUBLIC"],
        "http_addr": f"127.0.0.1:{free_port()}",
        "motd": [],
        "connect_aliases": {},
        "listen": [],
        "ax25": {
            "engine": "agwpe",
            "beacon": {"every": 0, "message": "", "destination": ""},
        },
        "agwpe": {"addr": agwpe_addr, "radio_port": 0},
    }
    config_path = Path(config_dir) / "config.json"
    config_path.write_text(json.dumps(config, indent=2))


@pytest.fixture()
def pat_pair(tncd_instance, direwolf_pair, tmp_path):
    """Set up two PAT instances with isolated configs and mailboxes.

    PAT-A connects to tncd's AGWPE port.
    PAT-B connects to Direwolf-B's native AGWPE port.

    Yields a dict with config paths, mailbox paths, and callsigns.
    """
    config_dir_a = tmp_path / "pat-a" / "config"
    config_dir_b = tmp_path / "pat-b" / "config"
    mbox_a = tmp_path / "pat-a" / "mailbox"
    mbox_b = tmp_path / "pat-b" / "mailbox"

    config_dir_a.mkdir(parents=True)
    config_dir_b.mkdir(parents=True)
    mbox_a.mkdir(parents=True)
    mbox_b.mkdir(parents=True)

    tncd_agwpe = f"127.0.0.1:{tncd_instance['agwpe_port']}"
    dw_b_agwpe = f"127.0.0.1:{direwolf_pair['agwpe_port_b']}"

    write_pat_config(config_dir_a, mbox_a, "N0CALL-1", "AA00aa", tncd_agwpe)
    write_pat_config(config_dir_b, mbox_b, "N0CALL-2", "AA00ab", dw_b_agwpe)

    yield {
        "config_a": str(config_dir_a / "config.json"),
        "mbox_a": str(mbox_a),
        "config_b": str(config_dir_b / "config.json"),
        "mbox_b": str(mbox_b),
        "call_a": "N0CALL-1",
        "call_b": "N0CALL-2",
    }


def pat_compose_and_send(config_path, mbox_path, from_call, to_call,
                         subject, body, attachment_path, timeout=180):
    """Compose a PAT message and send it via AGWPE connect."""
    pat_base = ["pat", "--config", config_path, "--mbox", mbox_path,
                "--mycall", from_call]

    # Compose the message
    compose_cmd = pat_base + [
        "compose",
        "--subject", subject,
        "--p2p-only",
    ]
    if attachment_path:
        compose_cmd.extend(["--attachment", str(attachment_path)])
    compose_cmd.append(to_call)

    result = subprocess.run(
        compose_cmd,
        input=body,
        capture_output=True, text=True, timeout=30,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"pat compose failed: {result.stderr}\n{result.stdout}"
        )

    # Connect and send — pipe "c\n" to stdin to auto-confirm the
    # Winlink account activation prompt that PAT shows for unknown callsigns
    connect_cmd = pat_base + [
        "--send-only",
        "connect", f"ax25+agwpe:///{to_call}",
    ]
    result = subprocess.run(
        connect_cmd,
        input="c\n",
        capture_output=True, text=True, timeout=timeout,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"pat connect failed: {result.stderr}\n{result.stdout}"
        )


def find_received_messages(mbox_path, callsign):
    """List message files in PAT's inbox for a given callsign."""
    inbox = Path(mbox_path) / callsign / "in"
    if not inbox.exists():
        return []
    return sorted(inbox.iterdir())


class TestPATFixture:
    @needs_pat
    def test_pat_config_valid(self, pat_pair):
        """PAT should accept the generated config files."""
        result = subprocess.run(
            ["pat", "--config", pat_pair["config_a"],
             "--mbox", pat_pair["mbox_a"], "env"],
            capture_output=True, text=True, timeout=10,
        )
        assert result.returncode == 0
        assert "N0CALL-1" in result.stdout


def pat_listen(config_path, mbox_path, mycall):
    """Start PAT in listen mode to accept incoming P2P connections.

    Returns the subprocess. Caller must kill_proc() when done.
    """
    proc = subprocess.Popen(
        ["pat", "--config", config_path, "--mbox", mbox_path,
         "--mycall", mycall, "--listen", "ax25", "interactive"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    # Auto-confirm the Winlink account activation prompt
    try:
        proc.stdin.write(b"c\n")
        proc.stdin.flush()
    except BrokenPipeError:
        pass
    # Give PAT time to register with AGWPE and start listening
    time.sleep(3.0)
    return proc


class TestConnectedModeKISSTCP:
    """Connected-mode P2P messaging over KISS TCP."""

    @needs_pat
    def test_p2p_message_both_directions(self, pat_pair, tmp_path):
        """Send a P2P message with ~10KB attachment in both directions."""
        # Generate random attachment
        attachment_data = os.urandom(10 * 1024)
        attachment_file = tmp_path / "test_attachment.bin"
        attachment_file.write_bytes(attachment_data)

        # --- A → B ---
        # Start PAT-B listening for incoming connections
        listener_b = pat_listen(
            pat_pair["config_b"], pat_pair["mbox_b"], pat_pair["call_b"],
        )
        try:
            pat_compose_and_send(
                config_path=pat_pair["config_a"],
                mbox_path=pat_pair["mbox_a"],
                from_call=pat_pair["call_a"],
                to_call=pat_pair["call_b"],
                subject="Test A to B",
                body="E2E test message from A to B",
                attachment_path=attachment_file,
                timeout=180,
            )
            time.sleep(5.0)
        finally:
            kill_proc(listener_b)

        # Verify B received the message
        msgs_b = find_received_messages(pat_pair["mbox_b"], pat_pair["call_b"])
        assert len(msgs_b) > 0, "PAT-B did not receive any messages"

        # --- B → A ---
        # Start PAT-A listening for incoming connections
        listener_a = pat_listen(
            pat_pair["config_a"], pat_pair["mbox_a"], pat_pair["call_a"],
        )
        try:
            pat_compose_and_send(
                config_path=pat_pair["config_b"],
                mbox_path=pat_pair["mbox_b"],
                from_call=pat_pair["call_b"],
                to_call=pat_pair["call_a"],
                subject="Test B to A",
                body="E2E test message from B to A",
                attachment_path=attachment_file,
                timeout=180,
            )
            time.sleep(5.0)
        finally:
            kill_proc(listener_a)

        # Verify A received the message
        msgs_a = find_received_messages(pat_pair["mbox_a"], pat_pair["call_a"])
        assert len(msgs_a) > 0, "PAT-A did not receive any messages"
