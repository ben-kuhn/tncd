"""Local-only e2e test: AX.25 v2.2 → v2.0 fallback against a real BPQ CMS.

Reproduces the exact over-the-air scenario where a Winlink CMS gateway (linbpq)
FRMRs tncd's SABME and tncd must fall back to SABM (mod-8) to connect. Unlike
the loopback v2.2 test (which uses a Dire Wolf responder that *accepts* SABME),
this drives the frames through a real modem path and a real BPQ node running the
CMS/RMS application, which rejects v2.2 just like KU0HN-10 does OTA.

Topology (all local, no radio):

    tncd (v2.2)  ──KISS/TCP──▶  Dire Wolf-A ──PipeWire audio──▶ Dire Wolf-B ──KISS/TCP──▶  linbpq (CMS)
         ▲                                    (1200 AFSK)                                       │
         └──────────────────────────── SABME→FRMR→SABM→UA→CMS ─────────────────────────────────┘

Both Dire Wolf instances run as *dumb KISS modems* (KISSPORT, not AGWPORT) so the
AX.25 connected-mode L2 is done by tncd on one side and BPQ on the other — BPQ is
the station that FRMRs the SABME.

LOCAL-ONLY. Skipped unless ALL of the following are present:
  - direwolf, pw-link on PATH (like the rest of the e2e suite)
  - linbpq resolvable (TNCD_LINBPQ env var, or `linbpq` on PATH)
  - TNCD_BPQ_CMSPASS  — Winlink CMS secure-login password (never committed)
  - TNCD_BPQ_CMSCALL  — CMS callsign (default: value of TNCD_BPQ_CMSCALL)
Requires live internet to the Winlink CMS servers. Not run in CI.
"""

import os
import shutil
import socket
import subprocess
import time
from pathlib import Path

import pytest

from conftest import tncd_command
from test_e2e import (
    free_port,
    kill_proc,
    pw_configure_for_test,
    pw_crosslink,
    pw_restore_settings,
    wait_for_port,
    write_direwolf_config,
    write_tncd_config,
)


# ---------------------------------------------------------------------------
# Discovery / skip conditions
# ---------------------------------------------------------------------------

def _find_linbpq():
    return os.environ.get("TNCD_LINBPQ") or shutil.which("linbpq")


CMSPASS = os.environ.get("TNCD_BPQ_CMSPASS")
CMSCALL = os.environ.get("TNCD_BPQ_CMSCALL")
LINBPQ = _find_linbpq()

pytestmark = [
    pytest.mark.skipif(not shutil.which("direwolf"), reason="direwolf not installed"),
    pytest.mark.skipif(not shutil.which("pw-link"), reason="pipewire not available"),
    pytest.mark.skipif(not LINBPQ, reason="linbpq not found (set TNCD_LINBPQ)"),
    pytest.mark.skipif(not CMSPASS, reason="TNCD_BPQ_CMSPASS not set (local-only)"),
    pytest.mark.skipif(not CMSCALL, reason="TNCD_BPQ_CMSCALL not set (local-only)"),
]

# The RMS/CMS application callsign tncd connects to (matches OTA gateway style).
RMS_CALL = "KU0HN-10"
NODE_CALL = "KU0HN-7"
# The station tncd connects *as*.
CLIENT_CALL = "N0CALL-1"


# ---------------------------------------------------------------------------
# BPQ config generation (secret injected at runtime, never written to the repo)
# ---------------------------------------------------------------------------

def write_bpq_config(path, telnet_port, cmd_port, http_port, kiss_host, kiss_port):
    """Write a minimal bpq32.cfg: Telnet+CMS uplink + a KISS/TCP radio port,
    with the RMS application dialing CMS. CMSPASS comes from the environment."""
    cfg = f"""SIMPLE
NODECALL={NODE_CALL}
NODEALIAS=MNLEW
LOCATOR=EN43BX

PORT
 PORTNUM=1
 ID=Telnet Server
 DRIVER=TELNET
 CONFIG
 LOGGING=1
 LOCALNET=127.0.0.1/32
 HTTPPORT={http_port}
 TCPPORT={telnet_port}
 CMDPORT={cmd_port}
 MAXSESSIONS=10
 CMSCALL={CMSCALL}
 CMSPASS={CMSPASS}
 CMS=1
  USER=sysop,sysoppass,{NODE_CALL},,SYSOP;
ENDPORT

PORT
 PORTNUM=2
 ID=KISS Radio 1200
 TYPE=ASYNC
 IPADDR={kiss_host}
 TCPPORT={kiss_port}
 SPEED=19200
 CHANNEL=A
 MAXFRAME=3
 FRACK=5000
 RESPTIME=40
 RETRIES=10
 PACLEN=128
 CONFIG
  UPDATEMAP
ENDPORT

APPLICATION 2,RMS,C 1 CMS,{RMS_CALL},HNRMS,255
"""
    Path(path).write_text(cfg)


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture()
def direwolf_kiss_pair(tmp_path):
    """Two Dire Wolf instances as dumb KISS modems, cross-linked via PipeWire.

    Yields the KISS TCP ports for A (tncd side) and B (BPQ side), plus paths to
    each Dire Wolf log (Dire Wolf logs decoded frames, so the A log shows the
    SABME→FRMR→SABM→UA exchange for assertions).
    """
    pw_original = pw_configure_for_test()

    kiss_port_a = free_port()
    kiss_port_b = free_port()

    conf_a = tmp_path / "dw-a.conf"
    conf_b = tmp_path / "dw-b.conf"
    write_direwolf_config(conf_a, "DWA-1", agwport=0, kissport=kiss_port_a)
    write_direwolf_config(conf_b, "DWB-2", agwport=0, kissport=kiss_port_b)

    log_a = open(tmp_path / "dw-a.log", "w+b")
    log_b = open(tmp_path / "dw-b.log", "w+b")
    proc_a = subprocess.Popen(["direwolf", "-c", str(conf_a), "-t", "0"],
                              stdout=log_a, stderr=subprocess.STDOUT)
    proc_b = subprocess.Popen(["direwolf", "-c", str(conf_b), "-t", "0"],
                              stdout=log_b, stderr=subprocess.STDOUT)

    sink_ids = []
    try:
        wait_for_port(kiss_port_a)
        wait_for_port(kiss_port_b)
        time.sleep(1.0)
        sink_ids = pw_crosslink(proc_a.pid, proc_b.pid)
        yield {
            "kiss_port_a": kiss_port_a,
            "kiss_port_b": kiss_port_b,
            "log_a_path": tmp_path / "dw-a.log",
            "log_b_path": tmp_path / "dw-b.log",
        }
    finally:
        kill_proc(proc_a)
        kill_proc(proc_b)
        for lb in sink_ids:
            kill_proc(lb)
        pw_restore_settings(pw_original)
        log_a.close()
        log_b.close()


@pytest.fixture()
def linbpq_cms(direwolf_kiss_pair, tmp_path):
    """Start linbpq attached to Dire Wolf-B's KISS port, with the CMS uplink.

    linbpq exits if the KISS TCP server isn't reachable at startup, so Dire Wolf-B
    (started by direwolf_kiss_pair) must already be listening — which it is.
    """
    cfgdir = tmp_path / "bpq" / "cfg"
    datadir = tmp_path / "bpq" / "data"
    logdir = tmp_path / "bpq" / "log"
    for d in (cfgdir, datadir, logdir):
        d.mkdir(parents=True)

    telnet_port = free_port()
    cmd_port = free_port()
    http_port = free_port()

    write_bpq_config(
        cfgdir / "bpq32.cfg", telnet_port, cmd_port, http_port,
        kiss_host="127.0.0.1", kiss_port=direwolf_kiss_pair["kiss_port_b"],
    )

    log = open(tmp_path / "linbpq.out", "w+b")
    proc = subprocess.Popen(
        [LINBPQ, "-c", str(cfgdir), "-d", str(datadir), "-l", str(logdir)],
        stdout=log, stderr=subprocess.STDOUT,
    )
    try:
        wait_for_port(telnet_port)   # BPQ is up (Telnet listening) → KISS attached
        time.sleep(2.0)
        yield {"telnet_port": telnet_port, "out_path": tmp_path / "linbpq.out"}
    finally:
        kill_proc(proc)
        log.close()


@pytest.fixture()
def tncd_v22(direwolf_kiss_pair, tmp_path):
    """tncd with ax25_version=2.2, KISS/TCP to Dire Wolf-A."""
    agwpe_port = free_port()
    api_port = free_port()
    cfg = tmp_path / "tncd-v22.ini"
    write_tncd_config(
        cfg, agwpe_port, "tcp",
        kiss_host="127.0.0.1", kiss_port=direwolf_kiss_pair["kiss_port_a"],
        callsign=NODE_CALL, ax25_version="2.2", api_port=api_port,
    )
    proc = subprocess.Popen(tncd_command() + ["-c", str(cfg), "-vv"],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    try:
        wait_for_port(agwpe_port)
        yield {"agwpe_port": agwpe_port, "api_port": api_port, "proc": proc}
    finally:
        kill_proc(proc)


# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

class TestV22FallbackAgainstBPQCMS:
    def test_sabme_frmr_sabm_fallback_reaches_cms(self, tncd_v22, direwolf_kiss_pair,
                                                  linbpq_cms):
        """tncd leads with SABME, BPQ CMS FRMRs it, tncd falls back to SABM,
        connects, and reaches CMS — the exact OTA fallback, verified on the air."""
        from emulator_agwpe import AGWPEClientEmulator, create_agwpe_frame

        received = []

        async def drive():
            client = AGWPEClientEmulator("127.0.0.1", tncd_v22["agwpe_port"])
            await client.connect()
            # Register our source callsign so tncd owns the connection.
            client.writer.write(create_agwpe_frame(0, ord("X"), CLIENT_CALL, "", b""))
            await client.writer.drain()
            await client.send_connect(CLIENT_CALL, RMS_CALL)
            # Collect frames for a while — expect a 'C' connect notification and
            # 'D' data carrying the CMS banner.
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                frame = await client.receive(timeout=5.0)
                if frame:
                    received.append(frame)
            await client.close()

        import asyncio
        asyncio.run(drive())

        dw_a = Path(direwolf_kiss_pair["log_a_path"]).read_text(errors="replace")
        print("=== Dire Wolf-A log tail ===")
        print(dw_a[-3000:])

        # The fallback sequence, observed on the air by Dire Wolf-A:
        assert "SABME" in dw_a, "tncd did not send SABME (v2.2 not attempted)"
        assert "FRMR" in dw_a, "BPQ CMS did not FRMR the SABME"
        assert "SABM cmd" in dw_a, "tncd did not fall back to SABM after the FRMR"
        assert "UA res" in dw_a, "BPQ did not accept the fallback SABM (no UA)"

        # And the connection actually completed to CMS: either a 'C' connect
        # notification or CMS banner data arrived at the AGWPE client.
        text = "".join(
            (f.get("data") or b"").decode("latin1", "replace") for f in received
        )
        kinds = {f["type"] for f in received}
        assert "C" in kinds or "CMS" in text or "Connected to CMS" in text, (
            f"connection did not complete to CMS; frames={[f['type'] for f in received]}\n"
            f"data={text[:500]}"
        )
