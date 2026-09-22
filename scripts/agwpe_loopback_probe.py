#!/usr/bin/env python3
"""Send one UI frame via AGWPE and print any frames received back.

Intended for TNC audio-loopback testing: wire the TNC's audio output back to
its audio input, run this script, and we should see the transmitted UI frame
return as a received UI frame within a few seconds.

Usage:
    python scripts/agwpe_loopback_probe.py [--host 127.0.0.1] [--port 8005]
                                            [--callsign KU0HN] [--payload "..."]
                                            [--listen-secs 15]
"""
import argparse
import socket
import struct
import sys
import time

AGWPE_HEADER = '<BBBBBBBB10s10sII'
HEADER_SIZE = 36


def build_frame(kind, port=0, call_from=b'', call_to=b'', data=b'', pid=0xF0):
    # AGWPE header bytes: port, 3x reserved, kind, reserved, pid, reserved, ...
    return struct.pack(
        AGWPE_HEADER,
        port, 0, 0, 0,
        ord(kind), 0, pid, 0,
        call_from.ljust(10, b'\x00'),
        call_to.ljust(10, b'\x00'),
        len(data), 0,
    ) + data


def parse_header(buf):
    fields = struct.unpack(AGWPE_HEADER, buf[:HEADER_SIZE])
    return {
        'port': fields[0],
        'kind': chr(fields[4]),
        'pid': fields[6],
        'call_from': fields[8].rstrip(b'\x00').decode('ascii', 'replace'),
        'call_to': fields[9].rstrip(b'\x00').decode('ascii', 'replace'),
        'datalen': fields[10],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--host', default='127.0.0.1')
    ap.add_argument('--port', type=int, default=8005)
    ap.add_argument('--callsign', default='KU0HN')
    ap.add_argument('--to', default='TEST')
    ap.add_argument('--payload', default='AGWPE loopback probe')
    ap.add_argument('--listen-secs', type=float, default=15.0)
    args = ap.parse_args()

    call_from = args.callsign.encode('ascii').upper()
    call_to = args.to.encode('ascii').upper()
    payload = args.payload.encode('utf-8')

    s = socket.socket()
    s.connect((args.host, args.port))
    print(f"Connected to {args.host}:{args.port}")

    # Register callsign so tncd will route received frames to us.
    s.sendall(build_frame('X', call_from=call_from))
    # Enable monitor mode (raw RX of all frames on the port).
    s.sendall(build_frame('m'))
    print(f"Registered {args.callsign!r}, monitor mode on")

    # Send one UI (unproto) frame.
    s.sendall(build_frame('M', call_from=call_from, call_to=call_to,
                          data=payload, pid=0xF0))
    sent_at = time.monotonic()
    print(f"TX UI: {args.callsign} -> {args.to} {payload!r}")

    # Listen for any returning frames.
    deadline = sent_at + args.listen_secs
    s.settimeout(0.5)
    buf = b''
    rx_count = 0
    while time.monotonic() < deadline:
        try:
            chunk = s.recv(4096)
        except socket.timeout:
            continue
        if not chunk:
            print("Server closed connection")
            break
        buf += chunk
        while len(buf) >= HEADER_SIZE:
            hdr = parse_header(buf)
            total = HEADER_SIZE + hdr['datalen']
            if len(buf) < total:
                break
            frame_data = buf[HEADER_SIZE:total]
            buf = buf[total:]
            elapsed = time.monotonic() - sent_at
            print(f"[+{elapsed:5.2f}s] RX '{hdr['kind']}' "
                  f"{hdr['call_from']}->{hdr['call_to']} "
                  f"len={hdr['datalen']}: {frame_data[:80]!r}"
                  f"{'...' if hdr['datalen'] > 80 else ''}")
            if hdr['kind'] in ('U', 'K') and payload in frame_data:
                rx_count += 1
                print(f"  ^^^ matches sent payload (loopback hit #{rx_count})")

    s.sendall(build_frame('x', call_from=call_from))
    s.close()
    if rx_count:
        print(f"\nLoopback PASS: {rx_count} echo(es) of TX payload received")
        return 0
    else:
        print("\nLoopback FAIL: no matching RX in window")
        return 1


if __name__ == '__main__':
    sys.exit(main())
