"""End-to-end test: AX.25 v2.2 (SABME / mod-128) connected round-trip.

Mirrors TestConnectedModeKISSTCP but configures tncd with
``ax25_version = 2.2`` so it initiates (and accepts) SABME frames.
Direwolf defaults to v2.2, so both ends negotiate mod-128.

Assertions:
  - A PAT message with attachment completes in both directions (data integrity).
  - Direwolf-B's log contains the v2.2 connect banner — proving mod-128 was
    negotiated and no v2.0 fallback occurred.
  - Direwolf-B's log does NOT contain the fallback string that would indicate
    tncd rejected SABME and Direwolf had to retry with SABM.

Skip conditions are identical to the rest of the e2e suite: direwolf,
pw-link, and pat must all be on PATH.
"""

import os
import shutil
import subprocess
import time
from pathlib import Path

import pytest

from conftest import tncd_command
from test_e2e import (
    _run_p2p_test,
    find_received_messages,
    free_port,
    kill_proc,
    pat_compose_and_send,
    pat_listen,
    pw_configure_for_test,
    pw_crosslink,
    pw_restore_settings,
    wait_for_port,
    write_direwolf_config,
    write_pat_config,
    write_tncd_config,
)


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


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture()
def direwolf_pair_v22(tmp_path):
    """Start a pair of Direwolf instances cross-linked via PipeWire.

    Identical to the ``direwolf_pair`` fixture in test_e2e.py (TCP KISS mode),
    duplicated here because pytest fixtures are scoped to the file that defines
    them (only conftest.py fixtures are shared).

    Yields a dict with:
      - kiss_port_a: KISS TCP port for Direwolf-A
      - agwpe_port_b: AGWPE port for Direwolf-B
      - proc_a, proc_b: subprocesses
      - log_b_path: path to Direwolf-B's log file
    """
    pw_original = pw_configure_for_test()

    agwpe_port_b = free_port()
    kiss_port_a = free_port()

    conf_a = tmp_path / "direwolf-a.conf"
    conf_b = tmp_path / "direwolf-b.conf"

    write_direwolf_config(conf_a, "N0CALL-1", agwport=0, kissport=kiss_port_a)
    write_direwolf_config(conf_b, "N0CALL-2", agwport=agwpe_port_b, kissport=0)

    log_a = open(tmp_path / "direwolf-a.log", "w+b")
    log_b = open(tmp_path / "direwolf-b.log", "w+b")

    proc_a = subprocess.Popen(
        ["direwolf", "-c", str(conf_a), "-t", "0"],
        stdout=log_a,
        stderr=subprocess.STDOUT,
    )
    proc_b = subprocess.Popen(
        ["direwolf", "-c", str(conf_b), "-t", "0"],
        stdout=log_b,
        stderr=subprocess.STDOUT,
    )

    sink_ids = []
    try:
        wait_for_port(kiss_port_a)
        wait_for_port(agwpe_port_b)
        time.sleep(1.0)
        sink_ids = pw_crosslink(proc_a.pid, proc_b.pid)

        yield {
            "kiss_port_a": kiss_port_a,
            "kiss_pty_a": None,
            "agwpe_port_b": agwpe_port_b,
            "proc_a": proc_a,
            "proc_b": proc_b,
            "log_b_path": tmp_path / "direwolf-b.log",
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
def tncd_instance_v22(direwolf_pair_v22, tmp_path):
    """Start tncd connected to Direwolf-A's KISS TCP port with ax25_version=2.2.

    Mirrors ``tncd_instance`` from test_e2e.py but passes ax25_version="2.2"
    to write_tncd_config so tncd initiates (and accepts) SABME.
    """
    agwpe_port = free_port()
    config_path = tmp_path / "tncd-v22.ini"

    write_tncd_config(
        config_path, agwpe_port, "tcp",
        kiss_host="127.0.0.1",
        kiss_port=direwolf_pair_v22["kiss_port_a"],
        ax25_version="2.2",
    )

    proc = subprocess.Popen(
        tncd_command() + ["-c", str(config_path)],
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


@pytest.fixture()
def pat_pair_v22(tncd_instance_v22, direwolf_pair_v22, tmp_path):
    """PAT pair for v2.2 test: PAT-A via tncd (v2.2), PAT-B via Direwolf-B AGWPE.

    Mirrors the ``pat_pair`` fixture in test_e2e.py.
    """
    config_dir_a = tmp_path / "pat-a" / "config"
    config_dir_b = tmp_path / "pat-b" / "config"
    mbox_a = tmp_path / "pat-a" / "mailbox"
    mbox_b = tmp_path / "pat-b" / "mailbox"

    config_dir_a.mkdir(parents=True)
    config_dir_b.mkdir(parents=True)
    mbox_a.mkdir(parents=True)
    mbox_b.mkdir(parents=True)

    tncd_agwpe = f"127.0.0.1:{tncd_instance_v22['agwpe_port']}"
    dw_b_agwpe = f"127.0.0.1:{direwolf_pair_v22['agwpe_port_b']}"

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


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestAX25V22ConnectedMode:
    """Connected-mode P2P round-trip asserting AX.25 v2.2 (mod-128) negotiation."""

    @needs_pat
    def test_p2p_v22_both_directions(self, pat_pair_v22, direwolf_pair_v22,
                                     tmp_path):
        """Send a P2P message with attachment in both directions via SABME/mod-128.

        Asserts:
          1. Messages arrive intact in both directions (data integrity).
          2. Direwolf-B log shows the "(v2.2)" connect banner — confirming
             SABME was accepted and mod-128 negotiated.
          3. Direwolf-B log does NOT contain the "Trying v2.0" fallback string,
             which would indicate tncd rejected SABME and forced a v2.0 retry.
        """
        # Run the shared connected-mode round-trip (A→B then B→A).
        _run_p2p_test(pat_pair_v22, tmp_path)

        # Read Direwolf-B's log for v2.2 negotiation evidence.
        log_b = Path(direwolf_pair_v22["log_b_path"]).read_text(errors="replace")

        print("=== Direwolf-B log (last 3000 chars) ===")
        print(log_b[-3000:])

        # Direwolf logs "Connected to <call>.  (v2.2)" when the link comes up
        # in extended (mod-128) mode — this appears in ax25_link.c for both
        # the responder (SABME received, UA sent) and initiator (UA received).
        assert "(v2.2)" in log_b, (
            "AX.25 v2.2 connect banner not found in Direwolf-B log — "
            "SABME/mod-128 negotiation did not succeed.\n"
            f"Direwolf-B log tail:\n{log_b[-2000:]}"
        )

        # If tncd sent DM or FRMR in response to SABME, Direwolf falls back to
        # SABM (v2.0).  This string appears in ax25_link.c when that happens.
        assert "Trying v2.0" not in log_b, (
            "Direwolf fell back to v2.0 — tncd rejected SABME instead of "
            "accepting it.  Check ax25_version config and SABME handling.\n"
            f"Direwolf-B log tail:\n{log_b[-2000:]}"
        )

    @needs_pat
    def test_srej_negotiated_no_regression(self, pat_pair_v22, direwolf_pair_v22,
                                           tmp_path):
        """Assert SREJ negotiation + no regression: v2.2 round-trip succeeds with srej=true.

        tncd defaults srej=true (phase 3.5).  Direwolf v2.2 advertises SREJ via XID
        immediately after the SABME/UA handshake.  This test verifies that tncd's SREJ
        support does not break the normal connected-mode path.

        WHY NO POSITIVE "SREJ ENABLED" LOG ASSERTION:
          Direwolf does not emit a banner when SREJ is negotiated.  The internal
          srej_enable field is set silently in complete_negotiation() (ax25_link.c:6683).
          No dw_printf() is called at that point.  The only SREJ-related log lines that
          would appear in Direwolf output are:
            - "sending REJ, at ... SREJ not enabled case" (ax25_link.c:2800) — fires
              when SREJ is DISABLED and Direwolf must fall back to REJ on a gap.
            - "SREJ frames received: N" in the final statistics dump — appears only if
              SREJ frames were actually exchanged (requires induced frame loss).
          On a clean PipeWire loopback there is no frame loss, so SREJ frames are never
          exchanged and the statistics line never appears.  The negative assertion
          (no "SREJ not enabled case" string) is the best structural check available
          from Direwolf's log alone.

        WHAT THIS TEST PROVES:
          - SREJ-capable connection setup (negotiation) does not regress the round-trip.
          - No SREJ-fallback path was hit ("SREJ not enabled case" absent).
          - v2.2 mod-128 link still completes cleanly when both ends advertise SREJ.

        WHAT THIS TEST DOES NOT PROVE:
          - SREJ *recovery* (retransmitting a specific missing frame without discarding
            the window).  A clean PipeWire cross-link has no frame loss, so the recovery
            path is never exercised here.
          Recovery is proven by the Task-4 unit tests (ax25/l2/l2_test.go) and the
          phase-3.5 OTA gate (bench test with real radio hardware).
        """
        # Run the full P2P round-trip with SREJ enabled (default).
        _run_p2p_test(pat_pair_v22, tmp_path)

        # Read Direwolf-B's log.
        log_b = Path(direwolf_pair_v22["log_b_path"]).read_text(errors="replace")

        print("=== Direwolf-B log (last 3000 chars) ===")
        print(log_b[-3000:])

        # Positive check: v2.2 connect banner still present — SABME/mod-128 negotiated
        # even with SREJ enabled on the tncd side.
        assert "(v2.2)" in log_b, (
            "AX.25 v2.2 connect banner missing from Direwolf-B log — "
            "SREJ-enabled tncd may have disrupted the SABME handshake.\n"
            f"Direwolf-B log tail:\n{log_b[-2000:]}"
        )

        # Negative check: Direwolf did NOT fall back to v2.0.
        assert "Trying v2.0" not in log_b, (
            "Direwolf fell back to v2.0 while SREJ was enabled in tncd — "
            "check XID handling and ax25_version config.\n"
            f"Direwolf-B log tail:\n{log_b[-2000:]}"
        )

        # Negative SREJ-fallback check: the "SREJ not enabled case" string appears in
        # Direwolf (ax25_link.c:2800) when it tries to send SREJ but srej_enable is
        # srej_none.  Its presence would mean XID negotiation did not enable SREJ on
        # Direwolf's end (e.g. tncd sent SREJNone in its XID response).
        assert "SREJ not enabled case" not in log_b, (
            "Direwolf hit the 'SREJ not enabled' fallback path — XID negotiation "
            "may not have correctly advertised SREJ.  "
            "Check tncd's XID response and srej config.\n"
            f"Direwolf-B log tail:\n{log_b[-2000:]}"
        )
