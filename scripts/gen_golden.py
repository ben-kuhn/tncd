#!/usr/bin/env python3
"""Generate golden byte fixtures from the frozen Python implementation.

Run inside the project venv/nix-shell:  python scripts/gen_golden.py
"""
import json
import struct
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent.parent))
import ax25  # noqa: E402
from tncd import _cmd_frame, _resp_frame, AGWPE_HEADER_FORMAT  # noqa: E402

ROOT = Path(__file__).parent.parent


def ax25_cases():
    cases = []

    def add(name, frame, **fields):
        cases.append({"name": name, "hex": bytes(frame).hex(), **fields})

    add("ui_cq", _cmd_frame("CQ", "KU0HN-1",
        control=ax25.Control(ax25.FrameType.UI), pid=0xF0, data=b"hello"),
        type="UI", dst="CQ", src="KU0HN-1", pid=0xF0, info="hello")
    add("ui_via_two", _cmd_frame("APRS", "KU0HN-9", via=["WIDE1-1", "WIDE2-1"],
        control=ax25.Control(ax25.FrameType.UI), pid=0xF0, data=b"pos"),
        type="UI", dst="APRS", src="KU0HN-9", via=["WIDE1-1", "WIDE2-1"],
        pid=0xF0, info="pos")
    add("sabm_p1", _cmd_frame("N0CALL-2", "KU0HN-10",
        control=ax25.Control(ax25.FrameType.SABM, poll_final=True)),
        type="SABM", dst="N0CALL-2", src="KU0HN-10", pf=True, command=True)
    add("sabme_p1", _cmd_frame("N0CALL-2", "KU0HN-10",
        control=ax25.Control(ax25.FrameType.SABME, poll_final=True)),
        type="SABME", dst="N0CALL-2", src="KU0HN-10", pf=True, command=True)
    add("ua_f1", _resp_frame("N0CALL-2", "KU0HN-10",
        control=ax25.Control(ax25.FrameType.UA, poll_final=True)),
        type="UA", dst="N0CALL-2", src="KU0HN-10", pf=True, command=False)
    add("dm_f1", _resp_frame("N0CALL-2", "KU0HN-10",
        control=ax25.Control(ax25.FrameType.DM, poll_final=True)),
        type="DM", dst="N0CALL-2", src="KU0HN-10", pf=True, command=False)
    add("disc_p1", _cmd_frame("N0CALL-2", "KU0HN-10",
        control=ax25.Control(ax25.FrameType.DISC, poll_final=True)),
        type="DISC", dst="N0CALL-2", src="KU0HN-10", pf=True, command=True)
    for nr in range(8):
        add(f"rr_cmd_p1_nr{nr}", _cmd_frame("N0CALL-2", "KU0HN-10",
            control=ax25.Control(ax25.FrameType.RR, poll_final=True, recv_seqno=nr)),
            type="RR", nr=nr, pf=True, command=True,
            dst="N0CALL-2", src="KU0HN-10")
        add(f"rr_resp_nr{nr}", _resp_frame("N0CALL-2", "KU0HN-10",
            control=ax25.Control(ax25.FrameType.RR, recv_seqno=nr)),
            type="RR", nr=nr, pf=False, command=False,
            dst="N0CALL-2", src="KU0HN-10")
    add("rnr_nr3", _resp_frame("N0CALL-2", "KU0HN-10",
        control=ax25.Control(ax25.FrameType.RNR, recv_seqno=3)),
        type="RNR", nr=3, pf=False, command=False,
        dst="N0CALL-2", src="KU0HN-10")
    add("rej_f1_nr5", _resp_frame("N0CALL-2", "KU0HN-10",
        control=ax25.Control(ax25.FrameType.REJ, poll_final=True, recv_seqno=5)),
        type="REJ", nr=5, pf=True, command=False,
        dst="N0CALL-2", src="KU0HN-10")
    for ns in range(8):
        add(f"i_ns{ns}_nr{(ns+2) % 8}", _cmd_frame("N0CALL-2", "KU0HN-10",
            control=ax25.Control(ax25.FrameType.I, send_seqno=ns,
                                 recv_seqno=(ns + 2) % 8),
            pid=0xF0, data=b"DATA" + bytes([0x30 + ns])),
            type="I", ns=ns, nr=(ns + 2) % 8, pf=False, command=True,
            dst="N0CALL-2", src="KU0HN-10", pid=0xF0,
            info=("DATA" + chr(0x30 + ns)))
    add("i_p1_via", _cmd_frame("N0CALL-2", "KU0HN-10", via=["DIGI-1"],
        control=ax25.Control(ax25.FrameType.I, send_seqno=1, recv_seqno=4,
                             poll_final=True),
        pid=0xF0, data=b"x" * 256),
        type="I", ns=1, nr=4, pf=True, command=True, via=["DIGI-1"],
        dst="N0CALL-2", src="KU0HN-10", pid=0xF0, info="x" * 256)
    return cases


def agwpe_cases():
    cases = []

    def hdr(port, kind, pid, call_from, call_to, data):
        h = struct.pack(AGWPE_HEADER_FORMAT, port, 0, 0, 0, ord(kind), 0,
                        pid, 0,
                        call_from.encode().ljust(10, b"\x00"),
                        call_to.encode().ljust(10, b"\x00"),
                        len(data), 0)
        return h + data

    cases.append({"name": "version_R", "kind": "R", "port": 0,
                  "hex": hdr(0, "R", 0, "", "", struct.pack("<II", 2, 0)).hex()})
    cases.append({"name": "register_X_ok", "kind": "X", "port": 0,
                  "call_from": "KU0HN-10",
                  "hex": hdr(0, "X", 0, "KU0HN-10", "", b"\x01").hex()})
    caps = struct.pack("<8BI", 0, 255, 40, 30, 63, 20, 7, 0, 0)
    cases.append({"name": "caps_g", "kind": "g", "port": 0,
                  "hex": hdr(0, "g", 0, "", "", caps).hex()})
    cases.append({"name": "portinfo_G_2ports", "kind": "G", "port": 0,
                  "hex": hdr(0, "G", 0, "", "",
                             b"2;Port 0;Port 1;").hex()})
    cases.append({"name": "data_D", "kind": "D", "port": 0,
                  "call_from": "N0CALL-2", "call_to": "KU0HN-10", "pid": 0xF0,
                  "hex": hdr(0, "D", 0xF0, "N0CALL-2", "KU0HN-10",
                             b"payload bytes").hex()})
    cases.append({"name": "conn_C_incoming", "kind": "C", "port": 1,
                  "call_from": "N0CALL-2", "call_to": "KU0HN-10",
                  "hex": hdr(1, "C", 0, "N0CALL-2", "KU0HN-10",
                             b"*** CONNECTED To Station N0CALL-2\r").hex()})
    cases.append({"name": "y_zero", "kind": "y", "port": 0,
                  "hex": hdr(0, "y", 0, "", "", struct.pack("<I", 0)).hex()})
    cases.append({"name": "Y_count", "kind": "Y", "port": 0,
                  "call_from": "KU0HN-10", "call_to": "N0CALL-2",
                  "hex": hdr(0, "Y", 0, "KU0HN-10", "N0CALL-2",
                             struct.pack("<I", 5)).hex()})
    return cases


def main():
    (ROOT / "ax25" / "testdata").mkdir(parents=True, exist_ok=True)
    (ROOT / "agwpe" / "testdata").mkdir(parents=True, exist_ok=True)
    (ROOT / "ax25" / "testdata" / "frames.json").write_text(
        json.dumps(ax25_cases(), indent=1))
    (ROOT / "agwpe" / "testdata" / "frames.json").write_text(
        json.dumps(agwpe_cases(), indent=1))
    print("wrote ax25/testdata/frames.json and agwpe/testdata/frames.json")


if __name__ == "__main__":
    main()
