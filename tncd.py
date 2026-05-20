#!/usr/bin/env python3
"""
AGWPE-to-KISS Translation Bridge

A bridge that allows AGWPE-client applications to communicate with KISS TNCs.
Supports both serial and TCP KISS connections.

Copyright (C) 2024 TNCD Contributors
License: GNU General Public License v3.0 (see COPYING)
"""

import argparse
import asyncio
import collections
import configparser
import functools
import logging
import os
import socket as socket_mod
import struct
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from pathlib import Path

import ax25
import kiss

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)


def _cmd_frame(to_str, from_str, via=None, **kw):
    """Build a command AX.25 frame with correct C/R bit (dest H=1, src H=0)."""
    dst = ax25.Address(to_str)
    dst.command_response = True
    if via:
        kw['via'] = [ax25.Address(v, repeater=True) for v in via]
    return ax25.Frame(dst=dst, src=ax25.Address(from_str), **kw)


def _resp_frame(to_str, from_str, via=None, **kw):
    """Build a response AX.25 frame with correct C/R bit (dest H=0, src H=1)."""
    src = ax25.Address(from_str)
    src.command_response = True
    if via:
        kw['via'] = [ax25.Address(v, repeater=True) for v in via]
    return ax25.Frame(dst=ax25.Address(to_str), src=src, **kw)


def hex_dump(data, prefix="", width=16):
    """Return a hex dump string of binary data."""
    if not data:
        return f"{prefix}(empty)"
    lines = []
    for i in range(0, len(data), width):
        chunk = data[i:i+width]
        hex_part = ' '.join(f'{b:02x}' for b in chunk)
        ascii_part = ''.join(chr(b) if 32 <= b < 127 else '.' for b in chunk)
        lines.append(f"{prefix}{i:04x}: {hex_part:<{width*3}} {ascii_part}")
    return '\n'.join(lines)


# AGWPE header: Port(1) + Reserved(3) + DataKind(1) + Reserved(1) + PID(1) +
#               Reserved(1) + CallFrom(10) + CallTo(10) + DataLen(4) + User(4)
# Equivalent to pe's '_HDR_FMT = BxxxBxBx10s10sIxxxx'
AGWPE_HEADER_FORMAT = '<BBBBBBBB10s10sII'
AGWPE_HEADER_SIZE = 36
MAX_AGWPE_PAYLOAD = 65536  # 64 KB — reject frames claiming larger payloads
MAX_CONNECTIONS = 64       # max simultaneous AX.25 connections
MAX_OUTBOUND_QUEUE = 512   # max queued outbound chunks per connection


DEFAULT_MAX_WINDOW = 3   # mod-8 AX.25: max outstanding I-frames (max 7)
DEFAULT_N2_RETRY = 10    # max T1 retransmissions before disconnect (AX.25 6.3.2)
AX25_OVERHEAD = 20       # AX.25 header + KISS framing bytes per frame
T2_MULTIPLIER = 1.2      # T2 = multiplier * frame_time (wait for burst to end)
DEFAULT_T3_TIMEOUT = 180  # T3 inactive link timer (seconds) — AX.25 v2.0 §6.3.3


class Connection:
    """State for a single AX.25 connected-mode session."""

    def __init__(self, port, local, remote):
        self.port = port
        self.local = local    # local callsign (our side)
        self.remote = remote  # remote callsign
        self.via = []         # digipeater path (list of callsign strings)
        self.state = 'DISCONNECTED'  # CONNECTING | CONNECTED | DISCONNECTING
        self.send_seqno = 0   # N(S): next I-frame seq to send (mod 8)
        self.recv_seqno = 0   # N(R): next I-frame seq expected from remote (mod 8)
        self.unacked = 0      # I-frames sent but not yet acked by remote (for AGWPE 'Y' query)
        self.last_acked = 0   # last N(R) received from remote
        self.owner = None     # AGWPEServerProtocol that initiated/accepted this connection
        self.outbound_queue = collections.deque()  # (data_chunk, pid) awaiting window space
        self.retransmit_buf = {}  # N(S) -> raw AX.25 frame bytes for retransmit
        self.t1_handle = None    # asyncio TimerHandle for T1 retransmit timer
        self.t1_polls = 0        # consecutive T1 polls with no ack response
        self.t2_handle = None    # asyncio TimerHandle for T2 delayed ACK timer
        self.t2_pending = False  # True when we owe the remote an RR
        self.t3_handle = None    # asyncio TimerHandle for T3 inactive link timer
        self.remote_busy = False   # True when remote sent RNR (stop sending I-frames)
        self._last_rr_time = 0.0   # monotonic time of last RR F=1 sent
        self._last_rr_nr = -1      # N(R) of last RR F=1 sent
        # Karn's algorithm: adaptive T1
        self.srtt = 0.0            # smoothed round-trip time (0 = not yet measured)
        self.rttvar = 0.0          # RTT variance
        self.t1_value = 0.0        # current T1 (0 = use port default)
        self._iframe_timestamps = {}  # N(S) -> monotonic send time (first TX only)


class AGWPEServerProtocol(asyncio.Protocol):

    def __init__(self, bridge, traffic_debug=0):
        self.bridge = bridge
        self.transport = None
        self.buffer = b''
        self.traffic_debug = traffic_debug
        self.monitoring = False  # toggled by 'm' frame
        self.registered_calls = set()  # callsigns registered via 'X' frame
        self.last_activity = time.monotonic()

    def connection_made(self, transport):
        if len(self.bridge.clients) >= self.bridge.max_clients:
            logger.warning(f"Client limit ({self.bridge.max_clients}) reached, "
                           f"rejecting {transport.get_extra_info('peername')}")
            transport.close()
            return
        self.transport = transport
        logger.info(f"AGWPE client connected from {transport.get_extra_info('peername')}")
        self.bridge.add_client(self)

    def connection_lost(self, exc):
        logger.info(f"AGWPE client disconnected: {exc}")
        self.bridge.remove_client(self)

    def data_received(self, data):
        self.buffer += data
        self.last_activity = time.monotonic()
        if self.traffic_debug:
            print(hex_dump(data, prefix="AGWPE RX: "))
        logger.debug(f"data_received: {len(data)} bytes, buffer now {len(self.buffer)}")

        while len(self.buffer) >= AGWPE_HEADER_SIZE:
            header = self.buffer[:AGWPE_HEADER_SIZE]
            try:
                values = struct.unpack(AGWPE_HEADER_FORMAT, header)
            except struct.error as e:
                logger.error(f"unpack failed: {e}, header={header.hex()}")
                self.buffer = b''
                break

            port      = values[0]
            datakind  = values[4]   # byte 4 is DataKind
            pid       = values[6]   # byte 6 is PID
            call_from = values[8]   # bytes 8-17 are CallFrom
            call_to   = values[9]   # bytes 18-27 are CallTo
            data_len  = values[10]  # bytes 28-31 are DataLen (little-endian)

            if data_len > MAX_AGWPE_PAYLOAD:
                logger.warning(
                    f"Oversized AGWPE payload ({data_len} bytes) from "
                    f"{self.transport.get_extra_info('peername')}, closing")
                self.transport.close()
                return

            if len(self.buffer) < AGWPE_HEADER_SIZE + data_len:
                logger.debug(
                    f"Waiting for payload: have {len(self.buffer) - AGWPE_HEADER_SIZE}"
                    f" of {data_len} bytes (kind={bytes([datakind])!r})")
                break

            frame_data = self.buffer[AGWPE_HEADER_SIZE:AGWPE_HEADER_SIZE + data_len]
            self.buffer = self.buffer[AGWPE_HEADER_SIZE + data_len:]

            self.handle_frame(port, datakind, pid, call_from, call_to, frame_data)

    def handle_frame(self, port, datakind, pid, call_from, call_to, data):
        datakind_bytes = bytes([datakind])

        from_str = (call_from.rstrip(b'\x00').decode('ascii', errors='replace')
                    if isinstance(call_from, bytes) else (call_from or ''))
        to_str   = (call_to.rstrip(b'\x00').decode('ascii', errors='replace')
                    if isinstance(call_to, bytes) else (call_to or ''))

        logger.debug(
            f"handle_frame: port={port}, kind={datakind_bytes!r} ({datakind:02x}),"
            f" from={from_str!r}, to={to_str!r}, len={len(data)}")
        if self.bridge.verbose >= 3:
            kind_ch = chr(datakind) if 32 <= datakind < 127 else f'\\x{datakind:02x}'
            print(f"  [AGWPE RX] '{kind_ch}'  port={port}"
                  f"  {from_str} -> {to_str}  {len(data)} bytes")

        # Validate port number for frame types that route to a specific port
        _routed_kinds = {b'M', b'V', b'C', b'c', b'v', b'D', b'd', b'K', b'g', b'H', b'y', b'Y'}
        if datakind_bytes in _routed_kinds:
            if port >= self.bridge.config.port_count:
                logger.debug(f"Ignoring frame for invalid port {port}")
                return

        if datakind_bytes == b'P':
            logger.debug(f"LOGIN from={from_str!r} (accepted)")

        elif datakind_bytes == b'R':
            logger.debug("VERSION request")
            self.send_version(port)

        elif datakind_bytes == b'G':
            logger.debug("PORT INFO request")
            self.send_port_info(port)

        elif datakind_bytes == b'g':
            logger.debug(f"PORT CAPABILITIES request for port {port}")
            # Response must be exactly 12 bytes; pe uses struct.unpack('<8BI', data).
            # Fields: baud, traffic_level, tx_delay, tx_tail, persist,
            #         slot_time, max_frame, active_conns, bytes_rcvd
            kiss_cfg = self.bridge.config.kiss_config(port)
            tx_delay    = int(kiss_cfg.get('tx_delay', '40'))
            persistence = int(kiss_cfg.get('persistence', '63'))
            slot_time   = int(kiss_cfg.get('slot_time', '20'))
            tx_tail     = int(kiss_cfg.get('tx_tail', '30'))
            caps = struct.pack('<8BI', 0, 255, tx_delay, tx_tail,
                               persistence, slot_time, 7, 0, 0)
            self.send_frame(port, ord(b'g'), b'', b'', caps)

        elif datakind_bytes == b'X':
            call = from_str.upper()
            logger.debug(f"REGISTER: from={call!r}")
            for other in self.bridge.clients:
                if other is not self and call in other.registered_calls:
                    logger.warning(f"Rejecting duplicate registration of {call}")
                    self.send_frame(port, ord(b'X'), call_from, b'', b'\x00')
                    return
            self.registered_calls.add(call)
            # pe reads CallFrom from the response to record registered callsign.
            # data[0] != 0 means success.  Echo the port from the request.
            self.send_frame(port, ord(b'X'), call_from, b'', b'\x01')

        elif datakind_bytes == b'x':
            # Unregister callsign: per spec, no response is sent.
            call = from_str.upper()
            logger.debug(f"UNREGISTER: from={call!r}")
            self.registered_calls.discard(call)

        elif datakind_bytes == b'm':
            # Toggle monitoring on/off per call (spec: same frame type both ways).
            self.monitoring = not self.monitoring
            logger.debug(f"Monitoring {'enabled' if self.monitoring else 'disabled'}")

        elif datakind_bytes == b'M':
            # Send UNPROTO (UI) frame. Build a complete AX.25 UI frame for KISS.
            logger.debug(f"UNPROTO from {from_str!r} to {to_str!r}")
            self._send_unproto(from_str, to_str, pid, data, port=port)

        elif datakind_bytes == b'V':
            # Send UNPROTO via digipeaters. Payload: count(1) + vias(10 each) + info.
            logger.debug(f"UNPROTO VIA from {from_str!r} to {to_str!r}")
            if data:
                n_via = data[0]
                via_data = data[1:1 + n_via * 10]
                info = data[1 + n_via * 10:]
                vias = [via_data[i*10:(i+1)*10].rstrip(b'\x00').decode('ascii', errors='replace')
                        for i in range(n_via)]
                self._send_unproto(from_str, to_str, pid, info, via=vias, port=port)
            else:
                self._send_unproto(from_str, to_str, pid, b'', port=port)

        elif datakind_bytes == b'C':
            # Initiate AX.25 connection: send SABM to TNC.
            if not self.bridge.kiss_clients[port].online:
                logger.info(f"CONNECT on offline port {port}: BUSY")
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
            logger.info(f"CONNECT     {from_str} -> {to_str}")
            conn = self.bridge.get_or_create_connection(port, from_str, to_str)
            if not conn:
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
            if conn.state != 'DISCONNECTED' and conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'C' for active {from_str}->{to_str}")
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
            conn.owner = self
            conn.state = 'CONNECTING'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            conn.t1_polls = 0
            try:
                frame = _cmd_frame(to_str, from_str,
                                   control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
                self.bridge._send_ax25(frame, port)
                self.bridge._start_t1(conn)
            except Exception as e:
                logger.error(f"Failed to build SABM: {e}")

        elif datakind_bytes == b'c':
            # Connect with non-standard PID (NET/ROM, etc.) — treated as SABM.
            logger.info(f"CONNECT     {from_str} -> {to_str}  (PID=0x{pid:02X})")
            conn = self.bridge.get_or_create_connection(port, from_str, to_str)
            if not conn:
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
            if conn.state != 'DISCONNECTED' and conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'c' for active {from_str}->{to_str}")
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
            conn.owner = self
            conn.state = 'CONNECTING'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            conn.t1_polls = 0
            try:
                frame = _cmd_frame(to_str, from_str,
                                   control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
                self.bridge._send_ax25(frame, port)
                self.bridge._start_t1(conn)
            except Exception as e:
                logger.error(f"Failed to build SABM: {e}")

        elif datakind_bytes == b'v':
            # Connect via digipeaters. Payload: count(1) + vias(10 bytes each).
            vias = []
            if data:
                n_via = data[0]
                via_data = data[1:1 + n_via * 10]
                vias = [
                    via_data[i*10:(i+1)*10].rstrip(b'\x00').decode('ascii', errors='replace')
                    for i in range(n_via)
                ]
            logger.info(f"CONNECT     {from_str} -> {to_str}  via {vias!r}")
            conn = self.bridge.get_or_create_connection(port, from_str, to_str)
            if not conn:
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
            if conn.state != 'DISCONNECTED' and conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'v' for active {from_str}->{to_str}")
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
            conn.owner = self
            conn.via = vias
            conn.state = 'CONNECTING'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            conn.t1_polls = 0
            try:
                frame = _cmd_frame(to_str, from_str, via=vias,
                    control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
                self.bridge._send_ax25(frame, port)
                self.bridge._start_t1(conn)
            except Exception as e:
                logger.error(f"Failed to build SABM via: {e}")

        elif datakind_bytes == b'D':
            # Send data over a connected session as an AX.25 I-frame.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if not conn or conn.state != 'CONNECTED':
                logger.warning(
                    f"'D' from {from_str!r} to {to_str!r}: "
                    f"no active connection (state={conn.state if conn else 'None'})")
            elif conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'D' for {from_str}->{to_str}")
            else:
                # I-frame info field is limited to 256 bytes; fragment if needed.
                payload = data if data else b''
                chunks = [payload[i:i+256] for i in range(0, len(payload), 256)] or [b'']
                frame_pid = pid if pid else 0xF0
                for chunk in chunks:
                    if len(conn.outbound_queue) >= MAX_OUTBOUND_QUEUE:
                        logger.warning(f"Outbound queue full ({MAX_OUTBOUND_QUEUE}), "
                                       f"dropping data for {from_str}->{to_str}")
                        break
                    conn.outbound_queue.append((chunk, frame_pid))
                logger.info(f"'D' frame: {len(payload)}B payload -> {len(chunks)} chunks, "
                            f"queue={len(conn.outbound_queue)}, unacked={conn.unacked}, "
                            f"window={self.bridge.max_window}")
                self.bridge._drain_outbound(conn)

        elif datakind_bytes == b'd':
            # Disconnect: send DISC to TNC.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if not conn:
                logger.warning(f"'d' disconnect with no connection {from_str!r} -> {to_str!r}")
            elif conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'd' for {from_str}->{to_str}")
            else:
                conn.state = 'DISCONNECTING'
                logger.info(f"DISCONNECT  {from_str} -> {to_str}")
                try:
                    frame = _cmd_frame(to_str, from_str, via=conn.via,
                                       control=ax25.Control(ax25.FrameType.DISC, poll_final=True))
                    self.bridge._send_ax25(frame, port)
                except Exception as e:
                    logger.error(f"Failed to build DISC: {e}")

        elif datakind_bytes == b'K':
            # Raw AX.25 frame. pe prepends a 0x00 byte (port indicator), strip it.
            if not self.registered_calls:
                logger.warning("Rejecting 'K' from unregistered client")
                return
            raw = data[1:] if (data and data[0] == 0x00) else data
            logger.debug(f"Raw KISS frame, {len(raw)} bytes")
            if raw:
                conn = self.bridge.get_connection(port, from_str, to_str)
                if conn and conn.owner is not self:
                    logger.warning(f"Rejecting non-owner 'K' for {from_str}->{to_str}")
                    return
                self.bridge.send_to_kiss(port, raw)

        elif datakind_bytes == b'k':
            # Toggle raw frame reception mode (K frames to client).
            # We don't buffer raw frames for clients; log and ignore.
            logger.debug("Raw KISS mode toggle received (not supported)")

        elif datakind_bytes == b'y':
            # Outstanding frames waiting on a port. We have no TX queue; reply 0.
            logger.debug(f"Outstanding frames query for port {port}")
            self.send_frame(port, ord(b'y'), b'', b'', struct.pack('<I', 0))

        elif datakind_bytes == b'Y':
            # Outstanding frames for a connection: queued + unacked.
            # Matches Direwolf behavior (i_frame_queue + txdata_by_ns).
            # PAT's Flush() has a 60s timeout waiting for outstanding==0.
            # Including queued frames makes PAT's Write() flow control
            # throttle sends (blocking at outstanding > maxFrame), so
            # Flush() only waits for the last batch — not the whole transfer.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if conn and conn.owner is not self:
                unacked = 0
                queued = 0
                count = 0
            else:
                unacked = conn.unacked if conn else 0
                queued = len(conn.outbound_queue) if conn else 0
                count = unacked + queued
            logger.info(f"'Y' query {from_str!r}<->{to_str!r}: unacked={unacked}, queue={queued}, reported={count}")
            self.send_frame(port, ord(b'Y'), call_from, call_to, struct.pack('<I', count))

        elif datakind_bytes == b'H':
            # Heard stations on a port. We don't track heard stations; reply empty.
            logger.debug(f"Heard stations query for port {port}")
            self.send_frame(port, ord(b'H'), b'', b'', b'')

        else:
            logger.info(f"Unknown frame type: {datakind_bytes!r} ({datakind})")

    def _send_unproto(self, from_str, to_str, pid, data, via=None, port=0):
        """Build a complete AX.25 UI frame and send it to KISS."""
        via_str = f" via {','.join(via)}" if via else ""
        logger.info(f"UNPROTO     {from_str} -> {to_str}{via_str}  ({len(data)} bytes)")
        try:
            dst = ax25.Address(to_str)
            dst.command_response = True
            frame = ax25.Frame(
                dst=dst,
                src=ax25.Address(from_str),
                via=[ax25.Address(v, repeater=True) for v in via] if via else None,
                control=ax25.Control(ax25.FrameType.UI),
                pid=pid if pid else 0xF0,
                data=data if data else b'',
            )
            self.bridge._send_ax25(frame, port)
        except Exception as e:
            logger.error(f"Failed to build AX.25 UI frame: {e}")

    def send_frame(self, port, datakind, call_from, call_to, data=b'', pid=0):
        if isinstance(call_from, bytes):
            from_bytes = call_from
        else:
            from_bytes = call_from.encode().ljust(10, b'\x00') if call_from else b'\x00' * 10
        if isinstance(call_to, bytes):
            to_bytes = call_to
        else:
            to_bytes = call_to.encode().ljust(10, b'\x00') if call_to else b'\x00' * 10

        from_bytes = (from_bytes + b'\x00' * 10)[:10]
        to_bytes   = (to_bytes   + b'\x00' * 10)[:10]
        header = struct.pack(
            AGWPE_HEADER_FORMAT,
            port,
            0, 0, 0,
            datakind,
            0, pid, 0,
            from_bytes,
            to_bytes,
            len(data),
            0
        )
        if self.transport and not self.transport.is_closing():
            if self.traffic_debug:
                print(hex_dump(header + data, prefix="AGWPE TX: "))
            self.transport.write(header + data)
            logger.debug(f"sent response: port={port}, kind={chr(datakind) if 32 <= datakind < 127 else datakind}, len={len(data)}")
            if self.bridge.verbose >= 3:
                kind_ch = chr(datakind) if 32 <= datakind < 127 else f'\\x{datakind:02x}'
                fr_str = from_bytes.rstrip(b'\x00').decode('ascii', 'replace')
                to_str = to_bytes.rstrip(b'\x00').decode('ascii', 'replace')
                print(f"  [AGWPE TX] '{kind_ch}'  port={port}"
                      f"  {fr_str} -> {to_str}  {len(data)} bytes")

    def send_version(self, port=0):
        # pe reads: major, minor = struct.unpack('H2xH2x', data) — 8 bytes total
        data = struct.pack('<II', 2, 0)  # MajorRevision=2, MinorRevision=0
        self.send_frame(port, ord(b'R'), b'', b'', data)

    def send_port_info(self, port=0):
        # pe parses: data.split(null,1)[0].decode().split(';')
        # Non-empty fields after the first become the port description list.
        count = self.bridge.config.port_count
        names = [self.bridge.config.port_name(i) for i in range(count)]
        payload = f"{count};{';'.join(names)};"
        self.send_frame(port, ord(b'G'), b'', b'', payload.encode())


class KISSClient:
    def __init__(self, port_num, port_section, kiss_section, raw_config, traffic_debug=0):
        self.port_num = port_num
        self.port_section = port_section
        self.kiss_section = kiss_section
        self.config = raw_config   # raw ConfigParser
        self.online = False
        self.name = raw_config.get(port_section, 'name', fallback=f'Port {port_num}')
        self.connection = None
        self.bridge = None
        self.traffic_debug = traffic_debug
        self._rx_thread = None

    def set_bridge(self, bridge):
        self.bridge = bridge

    def _get_kiss_params(self):
        params = {}
        section = self.kiss_section
        if section and self.config.has_section(section):
            for key in ['TX_DELAY', 'PERSISTENCE', 'SLOT_TIME', 'TX_TAIL', 'FULL_DUPLEX']:
                if self.config.has_option(section, key.lower()):
                    params[key] = self.config.getint(section, key.lower())
        return params

    async def connect(self):
        ps = self.port_section
        conn_type  = self.config.get(ps, 'type', fallback='serial')
        kiss_params = self._get_kiss_params()

        loop = asyncio.get_running_loop()

        init_str = self.config.get(ps, 'init_string', fallback=None)
        init_delay = self.config.getfloat(ps, 'init_delay', fallback=1.0)

        if conn_type == 'tcp':
            host = self.config.get(ps, 'host', fallback='localhost')
            port = self.config.getint(ps, 'port', fallback=8001)
            logger.info(f"Connecting to KISS TCP server at {host}:{port}")
            self.connection = kiss.TCPKISS(host=host, port=port)
        elif conn_type == 'bluetooth':
            try:
                import dbus
                import dbus.service
                import dbus.mainloop.glib
                from gi.repository import GLib
            except ImportError:
                raise RuntimeError(
                    "Bluetooth support requires dbus-python and PyGObject: "
                    "pip install dbus-python PyGObject"
                )

            bdaddr = self.config.get(ps, 'bdaddr', fallback=None)
            if not bdaddr:
                raise ValueError("Bluetooth connection requires 'bdaddr' in [client] config")

            channel = self.config.get(ps, 'channel', fallback=None)
            reconnect = self.config.getboolean(ps, 'reconnect', fallback=True)
            reconnect_delay = self.config.getfloat(ps, 'reconnect_delay', fallback=5.0)
            reconnect_max_delay = self.config.getfloat(ps, 'reconnect_max_delay', fallback=60.0)

            logger.info(f"Connecting to Bluetooth TNC at {bdaddr}"
                        f"{f' channel {channel}' if channel else ' (SDP auto-detect)'}")

            # Store reconnect params before connect attempt so they're
            # available if the initial connect fails and we enter the
            # reconnect loop.
            self._bt_dbus = dbus
            self._bt_glib = GLib
            self._bt_bdaddr = bdaddr
            self._bt_channel = channel
            self._bt_reconnect = reconnect
            self._bt_reconnect_delay = reconnect_delay
            self._bt_reconnect_max_delay = reconnect_max_delay

            sock = await self._bluetooth_connect(
                dbus, GLib, bdaddr, channel, loop)
            self.connection = BluetoothKISS(sock)
        else:
            device   = self.config.get(ps, 'device', fallback='/dev/ttyUSB0')
            baudrate = self.config.getint(ps, 'serial_baudrate',
                                          fallback=self.config.getint(ps, 'baudrate', fallback=9600))
            parity   = self.config.get(ps, 'parity', fallback='N').upper()
            stopbits = self.config.getfloat(ps, 'stopbits', fallback=1)
            rtscts   = self.config.getboolean(ps, 'rtscts', fallback=False)
            logger.info(f"Connecting to KISS serial device at {device}, {baudrate} baud")
            self.connection = kiss.SerialKISS(port=device, speed=baudrate)

        def _tnc_in_command_mode(ser):
            """Probe whether the TNC is in command mode by sending CR.

            Most TNCs respond with a text prompt or error (e.g. 'Eh?', '?',
            'cmd:') when they receive a bare CR in command mode.  A TNC
            already in KISS mode will not send any printable text back.
            """
            ser.reset_input_buffer()
            ser.write(b'\r')
            time.sleep(1.0)
            resp = ser.read(ser.in_waiting or 0)
            logger.debug(f"TNC probe raw response: {resp!r}")
            if resp:
                # Strip KISS FENDs / NULs — a TNC in KISS mode may echo
                # framing bytes but not printable ASCII.
                text = resp.replace(b'\xc0', b'').replace(b'\x00', b'').strip()
                if text and all(0x20 <= b < 0x7f or b in (0x0a, 0x0d) for b in text):
                    logger.info(f"TNC probe: command-mode response {text!r}")
                    return True
            logger.info("TNC probe: no command-mode response, assuming KISS mode")
            return False

        def blocking_start():
            # kiss3 uses a shared class-level event loop (SyncFrameDecode._loop).
            # Reset it so each KISSClient gets a fresh loop, avoiding
            # "This event loop is already running" when connecting multiple ports.
            from kiss.classes import SyncFrameDecode
            SyncFrameDecode._loop = None

            if init_str and conn_type != 'tcp':
                # Open serial via kiss3 without sending KISS config yet,
                # then send init commands through the SAME serial port so
                # there is no close/reopen cycle that could reset the TNC.
                self.connection.start_no_config()
                transport = self.connection.protocol.transport
                ser = transport.serial
                # Pause asyncio reader so it doesn't steal probe bytes
                transport.pause_reading()
                try:
                    if _tnc_in_command_mode(ser):
                        for line in init_str.split('\\n'):
                            cmd = line.replace('\\r', '\r').replace('\\n', '\n').encode()
                            logger.info(f"TNC init: {line!r}")
                            ser.write(cmd)
                            time.sleep(init_delay)
                        # Verify the TNC actually entered KISS mode
                        if _tnc_in_command_mode(ser):
                            raise RuntimeError(
                                "TNC still in command mode after init_string — "
                                "check that the init commands are correct for this TNC"
                            )
                        logger.info("TNC confirmed in KISS mode after init")
                finally:
                    transport.resume_reading()
                self.connection._write_defaults(**kiss_params)
            else:
                self.connection.start(**kiss_params)
            if conn_type == 'serial' and (parity != 'N' or stopbits != 1 or rtscts):
                logger.info(f"Reconfiguring serial port: parity={parity}, stopbits={stopbits}, rtscts={rtscts}")
                ser = self.connection.protocol.transport.serial
                ser.parity   = parity
                ser.stopbits = stopbits
                ser.rtscts   = rtscts

        if conn_type == 'bluetooth':
            # Bluetooth uses a per-instance event loop.  Defer start() to
            # _start_and_receive() so start() and read() run on the same
            # thread — asyncio loops aren't safely usable across threads.
            self._deferred_kiss_params = kiss_params
        else:
            with ThreadPoolExecutor() as executor:
                await loop.run_in_executor(executor, blocking_start)
            logger.info("KISS connection established")

    def _on_bt_connection_lost(self, exc):
        """Called when the Bluetooth connection drops."""
        logger.warning(f"Bluetooth connection lost: {exc}")
        if getattr(self, '_bt_reconnect', False):
            logger.info("Scheduling Bluetooth reconnection...")
            try:
                loop = asyncio.get_running_loop()
            except RuntimeError:
                loop = asyncio.get_event_loop()
            asyncio.ensure_future(self._bt_reconnect_loop(), loop=loop)
        else:
            logger.info("Bluetooth reconnect disabled, not reconnecting")

    async def _bt_reconnect_loop(self):
        """Reconnect to Bluetooth TNC with exponential backoff."""
        delay = self._bt_reconnect_delay
        max_delay = self._bt_reconnect_max_delay
        loop = asyncio.get_running_loop()
        kiss_params = self._get_kiss_params()

        while True:
            logger.info(f"Bluetooth reconnect in {delay:.1f}s...")
            await asyncio.sleep(delay)
            try:
                sock = await self._bluetooth_connect(
                    self._bt_dbus, self._bt_glib,
                    self._bt_bdaddr, self._bt_channel, loop)
                conn = BluetoothKISS(sock)
                with ThreadPoolExecutor() as executor:
                    await loop.run_in_executor(
                        executor, functools.partial(conn.start, **kiss_params))
                self.connection = conn

                # Re-hook connection_lost
                def _on_connection_lost(exc):
                    if hasattr(self, '_on_bt_connection_lost'):
                        self._on_bt_connection_lost(exc)
                self.connection.protocol._on_connection_lost = _on_connection_lost

                self.start_receive(loop)
                self.online = True
                logger.info(f"Port {self.port_num} ({self.name}) online — Bluetooth reconnected")
                return
            except Exception as e:
                logger.warning(f"Bluetooth reconnect failed: {e}")
                delay = min(delay * 2, max_delay)

    async def _bluetooth_connect(self, dbus_mod, GLib, bdaddr, channel, loop):
        """Connect to a Bluetooth SPP device via D-Bus Profile API.

        Registers a shared SPP profile (once per process), calls
        ConnectProfile on the target device, and waits for BlueZ to
        deliver a connected fd via NewConnection.
        Returns a socket wrapping the fd.
        """
        global _bt_profile, _bt_pending

        dbus_mod.mainloop.glib.DBusGMainLoop(set_as_default=True)
        bus = dbus_mod.SystemBus()

        # Register the shared SPP profile once for all Bluetooth ports
        if _bt_profile is None:
            profile_path = '/org/tncd/spp'
            ProfileClass = _make_spp_profile(dbus_mod, _bt_pending, loop)
            _bt_profile = ProfileClass(bus, profile_path)

            manager = dbus_mod.Interface(
                bus.get_object('org.bluez', '/org/bluez'),
                'org.bluez.ProfileManager1')
            opts = dbus_mod.Dictionary({
                'Role': dbus_mod.String('client'),
            }, signature='sv')
            manager.RegisterProfile(profile_path, SPP_UUID, opts)
            logger.info("Bluetooth SPP profile registered")

            glib_loop = GLib.MainLoop()
            glib_thread = threading.Thread(target=glib_loop.run, daemon=True,
                                           name='glib-mainloop')
            glib_thread.start()
            self._glib_loop = glib_loop

        device_path = f'/org/bluez/hci0/dev_{bdaddr.upper().replace(":", "_")}'
        device = dbus_mod.Interface(
            bus.get_object('org.bluez', device_path),
            'org.bluez.Device1')

        # Disconnect any existing connection (e.g. auto-connected BLE) so that
        # ConnectProfile can establish a fresh BR/EDR link for SPP.
        # We must call ConnectProfile immediately after disconnect — if the
        # device is trusted, BlueZ will auto-reconnect via BLE within ~1s,
        # which blocks BR/EDR paging.
        props = dbus_mod.Interface(
            bus.get_object('org.bluez', device_path),
            'org.freedesktop.DBus.Properties')
        connected = props.Get('org.bluez.Device1', 'Connected')
        if connected:
            logger.info(f"Disconnecting existing connection to {bdaddr}")
            device.Disconnect()

        # Register a future for this device so NewConnection can route to us
        fd_future = loop.create_future()
        _bt_pending[device_path] = fd_future

        logger.info(f"Calling ConnectProfile on {device_path}")

        def _connect_profile():
            try:
                device.ConnectProfile(SPP_UUID)
            except dbus_mod.exceptions.DBusException as e:
                # NoReply is expected — BlueZ delivers the fd via our profile's
                # NewConnection callback instead of replying to ConnectProfile.
                if 'NoReply' not in str(e) and 'Did not receive a reply' not in str(e):
                    if not fd_future.done():
                        loop.call_soon_threadsafe(fd_future.set_exception, e)

        threading.Thread(target=_connect_profile, daemon=True,
                         name='bt-connect').start()

        try:
            fd = await asyncio.wait_for(fd_future, timeout=30.0)
        except asyncio.TimeoutError:
            _bt_pending.pop(device_path, None)
            raise TimeoutError(f"Bluetooth connection to {bdaddr} timed out (30s)")
        sock = socket_mod.fromfd(fd, socket_mod.AF_UNIX, socket_mod.SOCK_STREAM)
        os.close(fd)
        logger.info(f"Bluetooth SPP socket ready (fd={sock.fileno()})")
        return sock

    def start_receive(self, loop):
        """Start a background thread that reads frames from the KISS TNC.

        Each received AX.25 frame is dispatched to bridge.on_kiss_frame()
        on the asyncio event loop thread via call_soon_threadsafe.
        """
        def _on_frame(frame_data):
            loop.call_soon_threadsafe(self.bridge.on_kiss_frame, self.port_num, bytes(frame_data))

        def _read_loop():
            logger.info(f"KISS RX thread started for port {self.port_num}")
            try:
                # min_frames=None blocks until the connection closes.
                self.connection.read(callback=_on_frame, min_frames=None)
            except Exception as e:
                if self.connection is not None:
                    logger.error(f"KISS RX error (port {self.port_num}): {e}")
            logger.info(f"KISS RX thread exited for port {self.port_num}")

        self._rx_thread = threading.Thread(target=_read_loop, daemon=True,
                                           name=f'kiss-rx-{self.port_num}')
        self._rx_thread.start()

    def _start_and_receive(self, loop, kiss_params):
        """Start connection AND read loop on the same thread.

        BluetoothKISS uses a per-instance asyncio event loop.  Both start()
        and read() call run_until_complete() on it, and asyncio loops are
        not safely usable across threads.  Running both on the same thread
        avoids cross-thread selector issues.
        """
        def _on_frame(frame_data):
            loop.call_soon_threadsafe(self.bridge.on_kiss_frame, self.port_num, bytes(frame_data))

        def _start_and_read():
            logger.info(f"KISS start+RX thread started for port {self.port_num}")
            try:
                self.connection.start(**kiss_params)
                logger.info("KISS connection established")
                # Hook connection_lost for bluetooth reconnection
                if hasattr(self, '_bt_reconnect'):
                    def _on_connection_lost(exc):
                        if hasattr(self, '_on_bt_connection_lost'):
                            self._on_bt_connection_lost(exc)
                    self.connection.protocol._on_connection_lost = _on_connection_lost
                self.online = True
                logger.info(f"Port {self.port_num} ({self.name}) online")
                self.connection.read(callback=_on_frame, min_frames=None)
            except Exception as e:
                if self.connection is not None:
                    logger.error(f"KISS RX error (port {self.port_num}): {e}")
            logger.info(f"KISS start+RX thread exited for port {self.port_num}")

        self._rx_thread = threading.Thread(target=_start_and_read, daemon=True,
                                           name=f'kiss-rx-{self.port_num}')
        self._rx_thread.start()

    def send(self, data):
        if self.connection:
            if self.traffic_debug:
                print(hex_dump(data, prefix="KISS TX: "))
            self.connection.write(data)

    def close(self):
        if self.connection:
            self.connection.stop()
            self.connection = None


SPP_UUID = '00001101-0000-1000-8000-00805f9b34fb'


def _make_spp_profile(dbus_mod, pending_connections, loop):
    """Create an SPP Profile1 D-Bus service object class.

    A single profile is shared across all Bluetooth ports.  BlueZ calls
    NewConnection() with a connected fd; we route to the correct
    KISSClient via pending_connections keyed by device path.
    """
    class SPPProfile(dbus_mod.service.Object):
        @dbus_mod.service.method("org.bluez.Profile1",
                                 in_signature="oha{sv}", out_signature="")
        def NewConnection(self, path, fd, properties):
            fd_val = fd.take()
            logger.info(f"Bluetooth SPP connected: path={path}, fd={fd_val}")
            future = pending_connections.pop(path, None)
            if future and not future.done():
                loop.call_soon_threadsafe(future.set_result, fd_val)
            else:
                logger.warning(f"Unexpected Bluetooth connection from {path}, closing fd")
                os.close(fd_val)

        @dbus_mod.service.method("org.bluez.Profile1",
                                 in_signature="o", out_signature="")
        def RequestDisconnection(self, path):
            logger.info(f"Bluetooth SPP disconnection requested: {path}")

        @dbus_mod.service.method("org.bluez.Profile1",
                                 in_signature="", out_signature="")
        def Release(self):
            logger.info("Bluetooth SPP profile released")

    return SPPProfile


# Shared Bluetooth SPP profile state (one registration per process)
_bt_profile = None
_bt_pending = {}   # device_path -> asyncio.Future


class _BluetoothKISSProtocol(kiss.kiss.KISSProtocol):
    """KISSProtocol subclass that supports a connection_lost callback."""

    _on_connection_lost = None

    def connection_lost(self, exc):
        super().connection_lost(exc)
        if self._on_connection_lost is not None:
            self._on_connection_lost(exc)


class BluetoothKISS(kiss.classes.KISS):
    """KISS connection over a pre-connected socket (e.g. Bluetooth SPP fd)."""

    def __init__(self, sock, strip_df_start=False):
        super().__init__(strip_df_start)
        self._sock = sock
        # Per-instance event loop so multiple BluetoothKISS instances
        # don't share the class-level SyncFrameDecode._loop.
        self._bt_loop = asyncio.new_event_loop()

    @property
    def loop(self):
        return self._bt_loop

    @loop.setter
    def loop(self, value):
        self._bt_loop = value

    def stop(self):
        if self.protocol and self.protocol.transport:
            self.protocol.transport.close()
        # Don't close the loop in stop() — it may still be needed by the
        # RX thread.  It will be cleaned up when the instance is GC'd.

    def start(self, **kwargs):
        _, self.protocol = self.loop.run_until_complete(
            self.loop.create_connection(
                functools.partial(_BluetoothKISSProtocol, decoder=self.decoder),
                sock=self._sock,
            )
        )
        self.loop.run_until_complete(self.protocol.connection_future)
        self._write_defaults(**kwargs)

    def write(self, frame):
        """Thread-safe write: schedule on the instance's event loop.

        The per-instance loop is pumped by the kiss-rx thread's
        run_until_complete(read), so call_soon_threadsafe writes get
        processed during the read loop.
        """
        self.loop.call_soon_threadsafe(self.protocol.write, frame)


class Bridge:
    def __init__(self, config, traffic_debug=0, verbose=0):
        self.config = config
        self.clients = []
        self.connections = {}   # (port, local, remote) -> Connection
        self.callsign = config.get('server', 'callsign', fallback='AGWPE')
        self.traffic_debug = traffic_debug
        self.verbose = verbose
        self.max_clients = config.getint('server', 'max_clients', fallback=8)
        self.idle_timeout = config.getint('server', 'idle_timeout', fallback=300)
        # TX echo detection: some TNCs (e.g. UV-Pro) echo transmitted frames
        # back via KISS. Track recently-sent raw bytes to discard them.
        self._sent_frames = collections.deque(maxlen=20)

        # Create one KISSClient per configured port
        self.kiss_clients = []
        for i in range(config.port_count):
            kc = KISSClient(
                port_num=i,
                port_section=config._ports[i],
                kiss_section=config._kiss.get(i),
                raw_config=config._raw,
                traffic_debug=traffic_debug,
            )
            kc.set_bridge(self)
            self.kiss_clients.append(kc)
        # Backward compat for tests that reference kiss_client (singular)
        self.kiss_client = self.kiss_clients[0] if self.kiss_clients else None

        # Window size and retry limit (global defaults, overridden per-port below)
        max_window = config.getint('ax25', 'max_window', fallback=DEFAULT_MAX_WINDOW)
        max_window = max(1, min(7, max_window))  # clamp to 1-7
        n2_retry = config.getint('ax25', 'n2_retry', fallback=DEFAULT_N2_RETRY)

        # Per-port T1/T2/window parameters derived from each port's ota_baudrate
        self._port_params = []
        for i in range(config.port_count):
            port_section = config._ports[i]
            ota_baudrate = config._raw.getint(port_section, 'ota_baudrate', fallback=1200)
            max_frame_bytes = 256 + AX25_OVERHEAD
            frame_time = (max_frame_bytes * 8) / ota_baudrate
            turnaround = 1.0
            t1_timeout = max(3.0, 2.0 * (max_window * frame_time + turnaround))
            t2_delay = max(0.1, T2_MULTIPLIER * frame_time)
            t3_timeout = config.getint('ax25', 't3_timeout', fallback=DEFAULT_T3_TIMEOUT)
            self._port_params.append({
                'max_window': max_window,
                'n2_retry': n2_retry,
                't1_timeout': t1_timeout,
                't2_delay': t2_delay,
                't3_timeout': t3_timeout,
            })
            logger.info(f"Port {i}: window={max_window}, T1={t1_timeout:.1f}s, "
                        f"T2={t2_delay:.2f}s, T3={t3_timeout}s (ota_baudrate={ota_baudrate})")

        # Backward compat aliases for port 0 (used by existing tests)
        if self._port_params:
            self.max_window = self._port_params[0]['max_window']
            self.n2_retry = self._port_params[0]['n2_retry']
            self.t1_timeout = self._port_params[0]['t1_timeout']
            self.t2_delay = self._port_params[0]['t2_delay']
        else:
            self.max_window = max_window
            self.n2_retry = n2_retry
            self.t1_timeout = 3.0
            self.t2_delay = 0.1

    def _get_port_param(self, port, key):
        if port < len(self._port_params):
            return self._port_params[port][key]
        return self._port_params[0][key]

    async def start(self):
        loop = asyncio.get_running_loop()

        # Connect ports sequentially; kiss3's blocking start() uses its own
        # event loop internally and doesn't play well with parallel execution.
        # Failures log and leave port offline (don't crash the bridge).
        for kc in self.kiss_clients:
            try:
                await kc.connect()
                if hasattr(kc, '_deferred_kiss_params'):
                    # Bluetooth: start() + read() on same thread
                    kc._start_and_receive(loop, kc._deferred_kiss_params)
                    # online is set by _start_and_receive thread
                else:
                    kc.start_receive(loop)
                    kc.online = True
                    logger.info(f"Port {kc.port_num} ({kc.name}) online")
            except Exception as e:
                logger.error(f"Port {kc.port_num} ({kc.name}) failed to connect: {e}")
                kc.online = False
                # Schedule reconnect loop for Bluetooth ports
                if getattr(kc, '_bt_reconnect', False):
                    asyncio.ensure_future(kc._bt_reconnect_loop())

        server_cfg = self.config['server']
        host = server_cfg.get('listen_host', '0.0.0.0')
        port = server_cfg.getint('listen_port', 8000)

        logger.info(f"Starting AGWPE server on {host}:{port}")

        server = await loop.create_server(
            lambda: AGWPEServerProtocol(self, self.traffic_debug),
            host, port
        )

        logger.info("Bridge running. Press Ctrl+C to stop.")
        loop.call_later(30, self._sweep_idle_clients)

        try:
            async with server:
                await server.serve_forever()
        except KeyboardInterrupt:
            logger.info("Shutting down...")

    def add_client(self, client):
        self.clients.append(client)

    def _sweep_idle_clients(self):
        """Close AGWPE clients that have been idle beyond idle_timeout."""
        if self.idle_timeout <= 0:
            return
        now = time.monotonic()
        for client in list(self.clients):
            if now - client.last_activity > self.idle_timeout:
                logger.info(f"Closing idle AGWPE client "
                            f"{client.transport.get_extra_info('peername')}")
                client.transport.close()
        loop = asyncio.get_running_loop()
        loop.call_later(30, self._sweep_idle_clients)

    def remove_client(self, client):
        if client in self.clients:
            self.clients.remove(client)
        # Clean up AX.25 connections owned by the departing client
        to_remove = [(k, c) for k, c in self.connections.items() if c.owner is client]
        for key, conn in to_remove:
            self._cancel_t1(conn)
            self._cancel_t2(conn)
            self._cancel_t3(conn)
            if conn.state in ('CONNECTED', 'CONNECTING'):
                try:
                    frame = _cmd_frame(conn.remote, conn.local, via=conn.via,
                                       control=ax25.Control(ax25.FrameType.DISC, poll_final=True))
                    self._send_ax25(frame, conn.port)
                except Exception:
                    pass
            del self.connections[key]
        # Free registered callsigns
        client.registered_calls.clear()

    def _conn_key(self, port, local, remote):
        return (port, local.strip().upper(), remote.strip().upper())

    def get_connection(self, port, local, remote):
        return self.connections.get(self._conn_key(port, local, remote))

    def get_or_create_connection(self, port, local, remote):
        key = self._conn_key(port, local, remote)
        if key not in self.connections:
            if len(self.connections) >= MAX_CONNECTIONS:
                logger.warning(f"Connection limit ({MAX_CONNECTIONS}) reached, "
                               f"rejecting {local}->{remote}")
                return None
            p, loc, rem = key
            self.connections[key] = Connection(p, loc, rem)
        return self.connections[key]

    def remove_connection(self, port, local, remote):
        conn = self.connections.pop(self._conn_key(port, local, remote), None)
        if conn:
            self._cancel_t1(conn)
            self._cancel_t2(conn)
            self._cancel_t3(conn)

    def _port_went_offline(self, port_num):
        """Called when a KISSClient loses its connection."""
        logger.warning(f"Port {port_num} went offline")
        self.kiss_clients[port_num].online = False
        # Notify and remove active connections on this port
        to_remove = [(k, conn) for k, conn in self.connections.items()
                     if conn.port == port_num and conn.state in ('CONNECTED', 'CONNECTING')]
        for key, conn in to_remove:
            self._cancel_t1(conn)
            self._cancel_t2(conn)
            self._cancel_t3(conn)
            if conn.owner:
                msg = f'*** DISCONNECTED From {conn.remote}\r'.encode()
                try:
                    conn.owner.send_frame(port_num, ord('d'),
                                          conn.remote.encode(), conn.local.encode(), msg)
                except Exception:
                    pass
            del self.connections[key]

    def _log_ax25(self, frame, direction):
        """Print per-frame AX.25 info when -v or -vv is active.

        -v  : frame type, source, destination, byte count
        -vv : same + decoded data content (up to 3 lines)
        """
        if self.verbose < 1:
            return
        ft = frame.control.frame_type
        src = str(frame.src)
        dst = str(frame.dst)

        if ft.is_I():
            ns, nr = frame.control.send_seqno, frame.control.recv_seqno
            type_str = f"I[{ns}/{nr}]"
        elif ft.is_S():
            type_str = f"{ft.name}[{frame.control.recv_seqno}]"
        else:
            type_str = ft.name

        via_str = (' via ' + ','.join(str(v) for v in frame.via)) if frame.via else ''
        data = frame.data if frame.data else b''
        size_str = f"  {len(data)} bytes" if data else ''
        ts = datetime.now().strftime('%H:%M:%S.%f')[:-3]
        print(f"  {ts} [{direction}] {type_str:<12} {src} -> {dst}{via_str}{size_str}")

        if self.verbose >= 2 and data:
            try:
                lines = data.decode('utf-8', errors='replace').rstrip('\r\n').split('\n')
                for line in lines[:3]:
                    print(f"      {line!r}")
                if len(lines) > 3:
                    print(f"      ... ({len(lines) - 3} more lines)")
            except Exception:
                print(f"      {data[:64].hex()}")

    def _send_ax25(self, frame, port=0):
        """Log (at verbose>=1) and send an AX.25 frame to the KISS TNC."""
        self._log_ax25(frame, 'TX')
        self.send_to_kiss(port, bytes(frame))

    @staticmethod
    def _normalize_hbits(raw_ax25):
        """Clear H-bits in via addresses for TX echo comparison.

        When a frame is repeated by digipeaters, they set the H-bit (bit 7
        of the SSID byte) on their address.  This makes the echoed raw bytes
        differ from what we originally sent, defeating exact-match echo
        suppression.  Normalizing clears all via H-bits so both versions
        compare equal.
        """
        data = bytearray(raw_ax25)
        # Address field: dst (7 bytes) + src (7 bytes) + vias (7 bytes each).
        # Extension bit (bit 0 of SSID byte) marks the last address.
        if len(data) > 13 and not (data[13] & 0x01):
            # src lacks extension bit → via addresses follow
            i = 14
            while i + 6 < len(data):
                data[i + 6] &= 0x7F  # clear H-bit
                if data[i + 6] & 0x01:  # last address
                    break
                i += 7
        return bytes(data)

    def send_to_kiss(self, port, data):
        if port >= len(self.kiss_clients):
            return
        kc = self.kiss_clients[port]
        if not kc.online:
            return
        self._sent_frames.append(self._normalize_hbits(bytes(data)))
        kc.send(data)

    def _drain_outbound(self, conn):
        """Send queued I-frames while the outbound window has space.

        Coalesces adjacent queue entries with the same PID into single
        I-frames up to 256 bytes, since AGWPE clients may send small
        'D' payloads (e.g. PAT sends 127-byte chunks).
        """
        sent_any = False
        sent_count = 0
        if conn.remote_busy:
            return
        max_window = self._get_port_param(conn.port, 'max_window')
        while conn.outbound_queue and conn.unacked < max_window:
            chunk, pid = conn.outbound_queue.popleft()
            # Coalesce adjacent entries with the same PID up to 256 bytes.
            while conn.outbound_queue and len(chunk) < 256:
                next_chunk, next_pid = conn.outbound_queue[0]
                if next_pid != pid or len(chunk) + len(next_chunk) > 256:
                    break
                conn.outbound_queue.popleft()
                chunk = chunk + next_chunk
            try:
                frame = _cmd_frame(conn.remote, conn.local, via=conn.via,
                                   control=ax25.Control(ax25.FrameType.I,
                                                        send_seqno=conn.send_seqno,
                                                        recv_seqno=conn.recv_seqno),
                                   pid=pid,
                                   data=chunk)
                ns = conn.send_seqno
                self._send_ax25(frame, conn.port)
                conn.retransmit_buf[ns] = bytes(frame)
                conn._iframe_timestamps[ns] = time.monotonic()
                conn.send_seqno = (ns + 1) % 8
                conn.unacked += 1
                sent_any = True
                sent_count += 1
            except Exception as e:
                logger.error(f"Failed to send queued I-frame: {e}")
                break
        if sent_any:
            logger.info(f"_drain_outbound: sent {sent_count} I-frames, "
                        f"unacked now={conn.unacked}, queue remaining={len(conn.outbound_queue)}")
            # Outgoing I-frames piggyback N(R), so cancel any pending T2
            # delayed ACK — the remote will see the acknowledgment in our
            # I-frames' N(R) field.
            self._cancel_t2(conn)
            self._start_t1(conn)

    def _start_t1(self, conn):
        """Start (or restart) the T1 retransmit timer for a connection."""
        self._cancel_t1(conn)
        try:
            loop = asyncio.get_running_loop()
            t1 = conn.t1_value if conn.t1_value > 0 else \
                self._get_port_param(conn.port, 't1_timeout')
            conn.t1_handle = loop.call_later(t1, self._t1_expired, conn)
        except RuntimeError:
            pass  # no event loop (e.g. in tests)

    def _cancel_t1(self, conn):
        """Cancel the T1 timer if running."""
        if conn.t1_handle is not None:
            conn.t1_handle.cancel()
            conn.t1_handle = None

    def _update_srtt(self, conn, rtt):
        """Update smoothed RTT and variance per Karn's algorithm (RFC 2988).

        On the first sample, initialize SRTT and RTTVAR directly.
        On subsequent samples, apply exponential weighted moving average."""
        t1_floor = 3.0
        t1_ceil = 60.0
        if conn.srtt == 0.0:
            # First measurement
            conn.srtt = rtt
            conn.rttvar = rtt / 2.0
        else:
            # α = 7/8, β = 3/4 (RFC 2988 / Jacobson/Karels)
            conn.rttvar = 0.75 * conn.rttvar + 0.25 * abs(conn.srtt - rtt)
            conn.srtt = 0.875 * conn.srtt + 0.125 * rtt
        conn.t1_value = max(t1_floor, min(t1_ceil, conn.srtt + 4 * conn.rttvar))
        logger.debug(f"Karn's T1: SRTT={conn.srtt:.2f}s RTTVAR={conn.rttvar:.2f}s "
                     f"T1={conn.t1_value:.2f}s (measured RTT={rtt:.2f}s)")

    def _schedule_t2(self, conn, src, dst):
        """Schedule a delayed RR (T2 timer).  Resets the timer on each call
        so a burst of I-frames results in a single RR after the burst."""
        self._cancel_t2(conn)
        conn.t2_pending = True
        conn._t2_src = str(src)
        conn._t2_dst = str(dst)
        try:
            loop = asyncio.get_running_loop()
            t2 = self._get_port_param(conn.port, 't2_delay')
            conn.t2_handle = loop.call_later(t2, self._t2_expired, conn)
        except RuntimeError:
            # No event loop (tests) — send immediately
            self._send_delayed_rr(conn)

    def _cancel_t2(self, conn):
        """Cancel the T2 delayed ACK timer."""
        if conn.t2_handle is not None:
            conn.t2_handle.cancel()
            conn.t2_handle = None
        conn.t2_pending = False

    def _t2_expired(self, conn):
        """T2 fired: send the delayed RR acknowledging all frames received so far."""
        conn.t2_handle = None
        conn.t2_pending = False
        if conn.state != 'CONNECTED':
            return
        self._send_delayed_rr(conn)

    def _start_t3(self, conn):
        """Start (or restart) the T3 inactive link timer."""
        self._cancel_t3(conn)
        t3 = self._get_port_param(conn.port, 't3_timeout')
        if t3 <= 0:
            return
        try:
            loop = asyncio.get_running_loop()
            conn.t3_handle = loop.call_later(t3, self._t3_expired, conn)
        except RuntimeError:
            pass

    def _cancel_t3(self, conn):
        """Cancel the T3 inactive link timer."""
        if conn.t3_handle is not None:
            conn.t3_handle.cancel()
            conn.t3_handle = None

    def _t3_expired(self, conn):
        """T3 fired: idle link — poll the remote with RR P=1 to check liveness.

        If the remote doesn't respond, T1 retries will eventually disconnect."""
        conn.t3_handle = None
        if conn.state != 'CONNECTED':
            return
        logger.info(f"T3 expired for {conn.remote}, sending liveness poll")
        try:
            rr = _cmd_frame(conn.remote, conn.local, via=conn.via,
                            control=ax25.Control(ax25.FrameType.RR,
                                                 poll_final=True,
                                                 recv_seqno=conn.recv_seqno))
            self._send_ax25(rr, conn.port)
            self._start_t1(conn)
        except Exception as e:
            logger.error(f"Failed to send T3 poll: {e}")

    def _send_delayed_rr(self, conn):
        """Send RR with current V(R) for delayed acknowledgment."""
        logger.info(f"TX RR(n(r)={conn.recv_seqno}) to {conn._t2_src} [T2 delayed]")
        try:
            rr = _resp_frame(conn._t2_src, conn._t2_dst, via=conn.via,
                             control=ax25.Control(ax25.FrameType.RR,
                                                  recv_seqno=conn.recv_seqno))
            self._send_ax25(rr, conn.port)
        except Exception as e:
            logger.error(f"Failed to send delayed RR: {e}")

    def _t1_expired(self, conn):
        """T1 timer fired: poll the remote with RR P=1.

        First expiry sends poll only — gives the remote a chance to
        respond without flooding slow TNC buffers (e.g. Bluetooth).
        Second consecutive expiry (no ack received) also retransmits
        unacked I-frames, since the originals were likely lost OTA.
        After N2 consecutive retransmissions, disconnect (AX.25 6.3.2).
        """
        conn.t1_handle = None
        n2_retry = self._get_port_param(conn.port, 'n2_retry')

        # SABM retransmission while waiting for UA (AX.25 6.3.1)
        if conn.state == 'CONNECTING':
            conn.t1_polls += 1
            if conn.t1_polls > n2_retry:
                logger.warning(f"N2 retry limit ({n2_retry}) exceeded for "
                               f"SABM to {conn.remote}, giving up")
                conn.state = 'DISCONNECTED'
                msg = f'*** BUSY From {conn.remote}\r'.encode()
                if conn.owner:
                    try:
                        conn.owner.send_frame(conn.port, ord('d'),
                                              conn.remote.encode(), conn.local.encode(), msg)
                    except Exception as e:
                        logger.error(f"Error sending 'd' for SABM timeout: {e}")
                self.remove_connection(conn.port, conn.local, conn.remote)
                return
            logger.info(f"T1 expired, retransmitting SABM to {conn.remote} "
                        f"(attempt {conn.t1_polls}/{n2_retry})")
            try:
                frame = _cmd_frame(conn.remote, conn.local, via=conn.via,
                                   control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
                self._send_ax25(frame, conn.port)
            except Exception as e:
                logger.error(f"Failed to retransmit SABM: {e}")
            if conn.t1_value > 0:
                conn.t1_value = min(60.0, conn.t1_value * 2)
            self._start_t1(conn)
            return

        if conn.state != 'CONNECTED' or not conn.retransmit_buf:
            return
        conn.t1_polls += 1

        # N2 retry limit: disconnect after too many unanswered polls.
        if conn.t1_polls > n2_retry:
            logger.warning(f"N2 retry limit ({n2_retry}) exceeded for "
                           f"{conn.local}<->{conn.remote}, disconnecting")
            conn.state = 'DISCONNECTED'
            msg = f'*** DISCONNECTED From {conn.remote}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(conn.port, ord('d'),
                                          conn.remote.encode(), conn.local.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'd' for N2 timeout: {e}")
            self.remove_connection(conn.port, conn.local, conn.remote)
            return

        retransmit = conn.t1_polls > 1
        logger.debug(f"T1 expired for {conn.local}<->{conn.remote}, "
                     f"{conn.unacked} unacked, poll #{conn.t1_polls}/{n2_retry}"
                     f"{' + retransmit' if retransmit else ''}")
        try:
            rr = _cmd_frame(conn.remote, conn.local, via=conn.via,
                            control=ax25.Control(ax25.FrameType.RR,
                                                 poll_final=True,
                                                 recv_seqno=conn.recv_seqno))
            self._send_ax25(rr, conn.port)
        except Exception as e:
            logger.error(f"Failed to send T1 poll: {e}")
        if retransmit:
            self._retransmit_from(conn, conn.last_acked)
        # Karn's algorithm: exponential backoff on timeout (don't update SRTT)
        if conn.t1_value > 0:
            conn.t1_value = min(60.0, conn.t1_value * 2)
            logger.debug(f"T1 backoff: {conn.t1_value:.2f}s")
        self._start_t1(conn)

    def _ack_frames(self, conn, r_seq):
        """Process cumulative ACK: update unacked, purge retransmit buffer, drain queue."""
        newly_acked = (r_seq - conn.last_acked) % 8
        # Guard against backwards N(R) from retransmitted frames.
        # A retransmit may carry an old N(R) that's behind last_acked;
        # the mod-8 arithmetic would interpret this as a huge forward ACK.
        # Only accept N(R) that ACKs at most max_window frames.
        max_window = self._get_port_param(conn.port, 'max_window')
        if newly_acked > max_window:
            logger.debug(f"Ignoring backwards N(R)={r_seq} (last_acked={conn.last_acked})")
            return
        if newly_acked:
            conn.t1_polls = 0  # remote responded — reset consecutive poll counter
            logger.info(f"_ack_frames: N(R)={r_seq}, acked {newly_acked} frames, "
                        f"unacked {conn.unacked}->{conn.unacked - newly_acked}, "
                        f"queue={len(conn.outbound_queue)}")
            # Karn's algorithm: compute RTT from first-transmission timestamps.
            now = time.monotonic()
            seq = conn.last_acked
            for _ in range(newly_acked):
                send_time = conn._iframe_timestamps.pop(seq, None)
                if send_time is not None:
                    self._update_srtt(conn, now - send_time)
                seq = (seq + 1) % 8
            # Purge retransmit buffer for ACKed sequence numbers.
            seq = conn.last_acked
            for _ in range(newly_acked):
                conn.retransmit_buf.pop(seq, None)
                seq = (seq + 1) % 8
            conn.last_acked = r_seq
            conn.unacked = max(0, conn.unacked - newly_acked)
            if conn.retransmit_buf:
                self._start_t1(conn)
            else:
                self._cancel_t1(conn)
            self._drain_outbound(conn)

    def _retransmit_from(self, conn, from_seq):
        """Retransmit all buffered I-frames from from_seq onward,
        updating N(R) to the current receive sequence number."""
        seq = from_seq
        while seq in conn.retransmit_buf:
            frame_bytes = conn.retransmit_buf[seq]
            orig = ax25.Frame.unpack(frame_bytes)
            # Rebuild the frame with current N(R) to piggyback-acknowledge
            # any frames received since this I-frame was originally built.
            frame = _cmd_frame(
                str(orig.dst), str(orig.src), via=conn.via,
                control=ax25.Control(
                    ax25.FrameType.I,
                    send_seqno=orig.control.send_seqno,
                    recv_seqno=conn.recv_seqno,
                    poll_final=orig.control.poll_final),
                pid=orig.pid,
                data=orig.data)
            conn.retransmit_buf[seq] = bytes(frame)
            # Karn's algorithm: discard RTT sample for retransmitted frames
            conn._iframe_timestamps.pop(seq, None)
            self._send_ax25(frame, conn.port)
            seq = (seq + 1) % 8

    # ------------------------------------------------------------------
    # KISS receive path
    # ------------------------------------------------------------------

    def on_kiss_frame(self, port_num, raw_kiss):
        """Called on the asyncio thread when a frame arrives from the KISS TNC."""
        # kiss3 with strip_df_start=False (the default) includes the KISS command byte
        # as the first byte: high nibble = port, low nibble = command (0 = data frame).
        # Strip it, and ignore non-data-frame commands.
        if not raw_kiss:
            return
        kiss_cmd = raw_kiss[0]
        if (kiss_cmd & 0x0F) != 0:
            logger.debug(f"Ignoring non-data KISS command 0x{kiss_cmd:02x}")
            return
        raw_ax25 = raw_kiss[1:]
        # Discard frames that are echoes of our own transmissions.
        # Normalize H-bits so digipeated echoes match the original.
        if self._normalize_hbits(raw_ax25) in self._sent_frames:
            logger.debug("Ignoring echoed TX frame")
            return
        if self.traffic_debug:
            print(hex_dump(raw_ax25, prefix="KISS RX: "))
        try:
            frame = ax25.Frame.unpack(raw_ax25)
        except Exception as e:
            logger.warning(f"Failed to parse AX.25 frame from KISS: {e} | "
                           f"len={len(raw_ax25)} raw={raw_ax25[:32].hex()}")
            return

        self._log_ax25(frame, 'RX')

        ft  = frame.control.frame_type
        src = str(frame.src)
        dst = str(frame.dst)

        if ft is ax25.FrameType.UI:
            self._dispatch_ui(frame, src, dst, port_num)
        elif ft.is_I():
            self._dispatch_i(frame, src, dst, port_num)
        elif ft.is_S():
            self._dispatch_s(frame, src, dst, port_num)
        elif ft is ax25.FrameType.SABM:
            self._dispatch_sabm(frame, src, dst, port_num)
        elif ft is ax25.FrameType.SABME:
            # Reject extended (mod-128) mode — we only support basic (mod-8).
            # Remote should fall back to SABM.
            logger.debug(f"Rejecting SABME from {src} (mod-128 not supported)")
            try:
                dm = _resp_frame(src, dst,
                                 control=ax25.Control(ax25.FrameType.DM, poll_final=True))
                self._send_ax25(dm, port_num)
            except Exception as e:
                logger.error(f"Failed to send DM for SABME: {e}")
        elif ft is ax25.FrameType.UA:
            self._dispatch_ua(frame, src, dst, port_num)
        elif ft is ax25.FrameType.DM:
            self._dispatch_dm(frame, src, dst, port_num)
        elif ft is ax25.FrameType.DISC:
            self._dispatch_disc(frame, src, dst, port_num)
        elif ft is ax25.FrameType.FRMR:
            self._dispatch_frmr(frame, src, dst, port_num)
        else:
            logger.debug(f"Received AX.25 {ft.name} frame, not forwarded")

        # Reset T3 inactive link timer on any received frame for an active
        # connection.  T3 detects dead peers on idle links (AX.25 v2.0 §6.3.3).
        conn = self.get_connection(port_num, dst, src)
        if conn and conn.state == 'CONNECTED':
            self._start_t3(conn)

    def _monitor_text(self, frame_type_str, src, dst, pid, data_len):
        """Format AGWPE monitor header: 'Fm SRC To DST <TYPE pid=XX Len=N >[HH:MM:SS]'"""
        ts = datetime.now().strftime('%H:%M:%S')
        return (
            f"Fm {src} To {dst}"
            f" <{frame_type_str} pid={pid:02X} Len={data_len} >"
            f"[{ts}]\r"
        ).encode()

    def _dispatch_ui(self, frame, src, dst, port=0):
        """Forward a UI (UNPROTO) frame to monitoring clients as 'U'."""
        pid  = frame.pid
        data = frame.data or b''
        payload = self._monitor_text('UI', src, dst, pid, len(data)) + data
        for client in self.clients:
            if client.monitoring:
                try:
                    client.send_frame(port, ord('U'), src.encode(), dst.encode(),
                                      payload, pid=pid)
                except Exception as e:
                    logger.error(f"Error sending 'U' to client: {e}")

    def _dispatch_i(self, frame, src, dst, port=0):
        """Handle received I-frame: deliver data to connection owner and monitoring clients."""
        pid  = frame.pid
        data = frame.data or b''
        logger.info(f"RX I-frame {src}->{dst} N(S)={frame.control.send_seqno} "
                    f"N(R)={frame.control.recv_seqno} P={frame.control.poll_final} "
                    f"{len(data)}B")

        # Find connection: local=dst (frame addressed to us), remote=src
        conn = self.get_connection(port, dst, src)

        # I-frame while CONNECTING: the UA was lost OTA but the remote
        # clearly accepted our SABM.  Promote to CONNECTED so the
        # I-frame is processed normally below.
        if conn and conn.state == 'CONNECTING':
            conn.state = 'CONNECTED'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            logger.info(f"CONNECTED   {dst} <-> {src}  (implicit, UA lost)")
            msg = f'*** CONNECTED With {src}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(port, ord('C'), src.encode(), dst.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'C' to owner: {e}")

        if not conn or conn.state != 'CONNECTED':
            # Check if this connection exists on a different port (overheard
            # frame from a TNC on the same frequency).  If so, silently drop.
            for other_port in range(self.config.port_count):
                if other_port != port:
                    other = self.get_connection(other_port, dst, src)
                    if other and other.state in ('CONNECTING', 'CONNECTED'):
                        logger.debug(f"Dropping overheard I-frame from {src} "
                                     f"on port {port} (conn on port {other_port})")
                        return
            # No active connection on any port — send DM so the remote
            # knows to disconnect.
            if frame.control.poll_final:
                try:
                    dm = _resp_frame(src, dst,
                                     control=ax25.Control(ax25.FrameType.DM,
                                                          poll_final=True))
                    self._send_ax25(dm, port)
                    logger.info(f"TX DM to {src} (no connection for I-frame)")
                except Exception as e:
                    logger.error(f"Failed to send DM: {e}")
        else:
            # I-frames carry N(R) which implicitly ACKs our sent frames.
            self._ack_frames(conn, frame.control.recv_seqno)

            expected_ns = conn.recv_seqno
            if frame.control.send_seqno != expected_ns:
                actual_ns = frame.control.send_seqno
                # Gap (future frame) vs true duplicate
                gap = (actual_ns - expected_ns) % 8
                if 0 < gap <= self._port_params[port]['max_window']:
                    # Future frame: a gap was detected.  Send REJ to request
                    # retransmission from V(R) per AX.25 v2.0 §2.4.4.
                    logger.info(f"Out-of-sequence I frame N(S)={actual_ns}"
                                f" (expected {expected_ns}), sending REJ")
                    self._cancel_t2(conn)
                    try:
                        rej = _resp_frame(src, dst, via=conn.via,
                                          control=ax25.Control(
                                              ax25.FrameType.REJ,
                                              poll_final=frame.control.poll_final,
                                              recv_seqno=expected_ns))
                        self._send_ax25(rej, port)
                    except Exception as e:
                        logger.error(f"Failed to send REJ to {src}: {e}")
                else:
                    # True duplicate (N(S) < V(R)) — discard data but still
                    # respond to a poll so the remote knows our current V(R).
                    logger.info(f"Discarding duplicate I frame N(S)={actual_ns}"
                                f" (expected {expected_ns})")
                    if frame.control.poll_final:
                        self._cancel_t2(conn)
                        self._send_rr_guarded(conn, src, dst, 'dup poll')
                    else:
                        self._schedule_t2(conn, src, dst)
            else:
                # In-sequence frame: advance V(R) and schedule delayed ACK.
                conn.recv_seqno = (frame.control.send_seqno + 1) % 8
                if frame.control.poll_final:
                    # Poll requires immediate response
                    self._cancel_t2(conn)
                    self._send_rr_guarded(conn, src, dst, 'I-frame poll')
                else:
                    # Delay the RR to batch-acknowledge a burst of I-frames
                    self._schedule_t2(conn, src, dst)
                # Deliver data to connection owner as 'D' frame
                if conn.owner:
                    try:
                        conn.owner.send_frame(port, ord('D'), src.encode(), dst.encode(),
                                              data, pid=pid)
                    except Exception as e:
                        logger.error(f"Error delivering 'D' to client: {e}")

        # Monitoring clients get 'I' monitor frame regardless
        payload = self._monitor_text('I', src, dst, pid, len(data)) + data
        for client in self.clients:
            if client.monitoring:
                try:
                    client.send_frame(port, ord('I'), src.encode(), dst.encode(),
                                      payload, pid=pid)
                except Exception as e:
                    logger.error(f"Error sending 'I' to client: {e}")

    def _dispatch_sabm(self, frame, src, dst, port=0):
        """Handle incoming SABM/SABME: accept connection, notify AGWPE clients."""
        # If this callsign pair already has an active connection on another
        # port, this is an overheard frame (both TNCs on the same frequency).
        # Silently drop it to avoid creating phantom connections and sending
        # spurious UA responses.
        for other_port in range(self.config.port_count):
            if other_port != port:
                other = self.get_connection(other_port, dst, src)
                if other and other.state in ('CONNECTING', 'CONNECTED'):
                    logger.debug(f"Dropping overheard SABM from {src} on port {port} "
                                 f"(conn on port {other_port})")
                    return
        # Capture digipeater path (reversed for return direction).
        incoming_via = [str(v) for v in frame.via] if frame.via else []
        return_via = list(reversed(incoming_via))
        logger.info(f"CONNECT     {src} -> {dst}  (incoming)"
                     + (f"  via {incoming_via!r}" if incoming_via else ""))

        conn = self.get_or_create_connection(port, dst, src)
        if not conn:
            # Connection limit reached — reject with DM (not UA).
            try:
                dm = _resp_frame(str(frame.src), str(frame.dst), via=return_via,
                                 control=ax25.Control(ax25.FrameType.DM,
                                                      poll_final=frame.control.poll_final))
                self._send_ax25(dm, port)
            except Exception as e:
                logger.error(f"Failed to send DM for connection limit: {e}")
            return

        try:
            ua = _resp_frame(str(frame.src), str(frame.dst), via=return_via,
                             control=ax25.Control(ax25.FrameType.UA,
                                                  poll_final=frame.control.poll_final))
            self._send_ax25(ua, port)
        except Exception as e:
            logger.error(f"Failed to send UA for incoming SABM: {e}")
            self.remove_connection(port, dst, src)
            return
        conn.via = return_via
        conn.state = 'CONNECTED'
        conn.send_seqno = 0
        conn.recv_seqno = 0
        conn.unacked = 0
        conn.last_acked = 0
        conn.retransmit_buf.clear()
        conn._iframe_timestamps.clear()
        conn.outbound_queue.clear()
        conn.remote_busy = False
        self._cancel_t1(conn)
        self._cancel_t2(conn)
        self._cancel_t3(conn)

        # Assign owner to the client that registered this callsign.
        for client in self.clients:
            try:
                if dst in client.registered_calls:
                    conn.owner = client
                    break
            except TypeError:
                pass

        msg = f'*** CONNECTED To Station {src}\r'.encode()
        if conn.owner:
            try:
                conn.owner.send_frame(port, ord('C'), src.encode(), dst.encode(), msg)
            except Exception as e:
                logger.error(f"Error sending 'C' notification: {e}")

    def _dispatch_ua(self, frame, src, dst, port=0):
        """Handle UA: outgoing connection established, or disconnect confirmed."""
        # src=remote sent UA, dst=local received it
        conn = self.get_connection(port, dst, src)
        if conn is None:
            logger.debug(f"UA from {src}: no pending connection")
            return

        if conn.state == 'CONNECTING':
            self._cancel_t1(conn)
            conn.state = 'CONNECTED'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            conn.t1_polls = 0
            logger.info(f"CONNECTED   {dst} <-> {src}")
            msg = f'*** CONNECTED With {src}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(port, ord('C'), src.encode(), dst.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'C' to owner: {e}")

        elif conn.state == 'DISCONNECTING':
            logger.info(f"DISCONNECTED {dst} <-> {src}")
            msg = f'*** DISCONNECTED From {src}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(port, ord('d'), src.encode(), dst.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'd' to owner: {e}")
            self.remove_connection(port, dst, src)

    def _dispatch_dm(self, frame, src, dst, port=0):
        """Handle DM: connection refused or remote forced disconnect."""
        conn = self.get_connection(port, dst, src)
        if conn is None:
            logger.debug(f"DM from {src}: no active connection")
            return
        logger.info(f"REJECTED    {dst} <-> {src}  (DM, was {conn.state})")
        msg = (f'*** CONNECTED With {src} failed\r'.encode()
               if conn.state == 'CONNECTING'
               else f'*** DISCONNECTED From {src}\r'.encode())
        if conn.owner:
            try:
                conn.owner.send_frame(port, ord('d'), src.encode(), dst.encode(), msg)
            except Exception as e:
                logger.error(f"Error sending 'd' for DM: {e}")
        self.remove_connection(port, dst, src)

    def _dispatch_disc(self, frame, src, dst, port=0):
        """Handle remote DISC: send UA, notify AGWPE client of disconnection."""
        # Suppress overheard DISC from TNCs on the same frequency.
        conn = self.get_connection(port, dst, src)
        if not conn:
            for other_port in range(self.config.port_count):
                if other_port != port:
                    other = self.get_connection(other_port, dst, src)
                    if other and other.state in ('CONNECTING', 'CONNECTED', 'DISCONNECTING'):
                        logger.debug(f"Dropping overheard DISC from {src} on port {port} "
                                     f"(conn on port {other_port})")
                        return
        logger.info(f"DISCONNECT  {src} -> {dst}  (remote)")
        conn = self.get_connection(port, dst, src)
        if conn:
            # Connection exists: respond with UA per AX.25 v2.0 §2.4.5
            try:
                ua = _resp_frame(str(frame.src), str(frame.dst), via=conn.via,
                                 control=ax25.Control(ax25.FrameType.UA,
                                                      poll_final=frame.control.poll_final))
                self._send_ax25(ua, port)
            except Exception as e:
                logger.error(f"Failed to send UA for DISC: {e}")
        else:
            # No connection: respond with DM per AX.25 v2.0 §2.4.5
            try:
                dm = _resp_frame(str(frame.src), str(frame.dst),
                                 control=ax25.Control(ax25.FrameType.DM,
                                                      poll_final=frame.control.poll_final))
                self._send_ax25(dm, port)
            except Exception as e:
                logger.error(f"Failed to send DM for DISC: {e}")
            return

        if conn:
            msg = f'*** DISCONNECTED From {src}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(port, ord('d'), src.encode(), dst.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'd' for DISC: {e}")
            self.remove_connection(port, dst, src)

    def _dispatch_frmr(self, frame, src, dst, port=0):
        """Handle incoming FRMR: reset the connection (AX.25 2.4.5).

        FRMR indicates a protocol error that cannot be recovered by
        retransmission.  Re-establish the link by sending SABM.
        """
        logger.warning(f"FRMR from {src} -> {dst}, resetting connection")
        conn = self.get_connection(port, dst, src)
        if conn and conn.state == 'CONNECTED':
            conn.state = 'CONNECTING'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            conn.unacked = 0
            conn.last_acked = 0
            conn.retransmit_buf.clear()
            conn._iframe_timestamps.clear()
            conn.outbound_queue.clear()
            self._cancel_t1(conn)
            self._cancel_t2(conn)
            self._cancel_t3(conn)
            try:
                frame = _cmd_frame(src, dst, via=conn.via,
                                   control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
                self._send_ax25(frame, port)
                self._start_t1(conn)
            except Exception as e:
                logger.error(f"Failed to send SABM after FRMR: {e}")

    def _send_rr_guarded(self, conn, src, dst, tag):
        """Send RR F=1 with conn.recv_seqno, suppressing duplicates within 1s.

        Prevents flooding the KISS TX buffer with identical RR responses when
        the remote sends rapid retransmit bursts on a half-duplex channel.
        """
        now = time.monotonic()
        nr = conn.recv_seqno
        if nr == conn._last_rr_nr and (now - conn._last_rr_time) < 3.0:
            logger.debug(f"Suppressing duplicate RR(n(r)={nr}, f=1) to {src} [{tag}]")
            return
        conn._last_rr_time = now
        conn._last_rr_nr = nr
        logger.info(f"TX RR(n(r)={nr}, f=1) to {src} [{tag}]")
        try:
            rr = _resp_frame(src, dst, via=conn.via,
                             control=ax25.Control(ax25.FrameType.RR,
                                                  poll_final=True,
                                                  recv_seqno=nr))
            self._send_ax25(rr, conn.port)
        except Exception as e:
            logger.error(f"Failed to send RR F=1 to {src}: {e}")

    def _send_poll_response(self, conn, src_str, dst_str):
        """Deferred RR F=1 poll response — called via call_soon so that
        I-frames queued before us on the event loop are processed first."""
        if conn.state != 'CONNECTED':
            return
        self._send_rr_guarded(conn, src_str, dst_str, 'poll response')
        # The remote is polling us — if we have unacked I-frames,
        # retransmit from where the remote left off.
        if conn.retransmit_buf:
            logger.info(f"Retransmitting {len(conn.retransmit_buf)} I-frames "
                        f"from seq {conn.last_acked}")
            self._retransmit_from(conn, conn.last_acked)

    def _dispatch_s(self, frame, src, dst, port=0):
        """Handle received S (supervisory) frame: respond to polls, forward to monitors."""
        ft = frame.control.frame_type
        ft_name = ft.name
        try:
            r_seq = frame.control.recv_seqno
        except TypeError:
            r_seq = 0

        conn = self.get_connection(port, dst, src)
        logger.info(f"RX {ft_name} {src}->{dst} N(R)={r_seq} "
                    f"P={frame.control.poll_final} "
                    f"recv_seqno={conn.recv_seqno if conn else '?'}")

        # Update unacked count and purge retransmit buffer.
        if conn and conn.state == 'CONNECTED':
            self._ack_frames(conn, r_seq)

            # RNR: remote is busy, stop sending I-frames (AX.25 6.4.9).
            if ft is ax25.FrameType.RNR:
                if not conn.remote_busy:
                    logger.info(f"Remote {src} busy (RNR)")
                conn.remote_busy = True
            elif ft is ax25.FrameType.RR:
                if conn.remote_busy:
                    logger.info(f"Remote {src} no longer busy (RR)")
                    conn.remote_busy = False
                    self._drain_outbound(conn)

            # REJ: retransmit from the requested sequence number.
            if ft is ax25.FrameType.REJ:
                conn.remote_busy = False
                self._retransmit_from(conn, r_seq)

        # If the remote set P=1 (poll), we must respond with RR F=1.
        # Defer via call_soon so that any I-frames already queued on the
        # event loop (from the same KISS burst) are processed first,
        # advancing recv_seqno before we build the response.
        if frame.control.poll_final:
            conn = self.get_connection(port, dst, src)
            if conn and conn.state == 'CONNECTED':
                loop = asyncio.get_running_loop()
                loop.call_soon(self._send_poll_response, conn,
                               str(src), str(dst))

        # Forward to monitoring clients as 'S'
        ts = datetime.now().strftime('%H:%M:%S')
        payload = (
            f"Fm {src} To {dst} <{ft_name} R{r_seq} >[{ts}]\r"
        ).encode()
        for client in self.clients:
            if client.monitoring:
                try:
                    client.send_frame(port, ord('S'), src.encode(), dst.encode(), payload)
                except Exception as e:
                    logger.error(f"Error sending 'S' to client: {e}")


class PortConfig:
    """Wrapper around ConfigParser providing multi-port access.

    Numbered sections [client.0], [client.1], ... define ports.
    Backward-compatible: config['client'] and config.get('client', ...) map
    to port 0's section so existing Bridge/KISSClient code continues to work.
    """

    def __init__(self, raw, ports=None, kiss=None):
        self._raw = raw
        # Ordered list of section names for each port index (e.g. 'client.0')
        # If ports/kiss are provided explicitly they take precedence over scanning.
        if ports is not None:
            # ports is a list like ['client.0', 'client.1']
            self._ports = {i: s for i, s in enumerate(ports)}
        else:
            self._ports = {}
        if kiss is not None:
            # kiss is a dict like {0: 'kiss.0', 1: 'kiss.1'}
            self._kiss = dict(kiss)
        else:
            self._kiss = {}

        if ports is None or kiss is None:
            for section in raw.sections():
                if ports is None and section.startswith('client.'):
                    try:
                        idx = int(section[len('client.'):])
                        self._ports[idx] = section
                    except ValueError:
                        pass
                elif kiss is None and section.startswith('kiss.'):
                    try:
                        idx = int(section[len('kiss.'):])
                        self._kiss[idx] = section
                    except ValueError:
                        pass

    # ------------------------------------------------------------------
    # Multi-port API
    # ------------------------------------------------------------------

    @property
    def port_count(self):
        return len(self._ports)

    def port_config(self, n):
        """Return dict of config values for port N (from [client.N])."""
        section = self._ports[n]
        return {k: v for k, v in self._raw.items(section)
                if k not in self._raw.defaults()}

    def kiss_config(self, n):
        """Return dict of KISS config values for port N, or {} if none."""
        if n not in self._kiss:
            return {}
        section = self._kiss[n]
        return {k: v for k, v in self._raw.items(section)
                if k not in self._raw.defaults()}

    def port_name(self, n):
        """Human-readable name for port N."""
        section = self._ports.get(n)
        if section and self._raw.has_option(section, 'name'):
            return self._raw.get(section, 'name')
        return f"Port {n}"

    # ------------------------------------------------------------------
    # Backward-compatible delegation (map 'client'/'kiss' to port 0)
    # ------------------------------------------------------------------

    def _resolve_section(self, section):
        if section == 'client' and self._ports:
            return self._ports[0]
        if section == 'kiss' and 0 in self._kiss:
            return self._kiss[0]
        return section

    def get(self, section, key, **kwargs):
        return self._raw.get(self._resolve_section(section), key, **kwargs)

    def getint(self, section, key, **kwargs):
        return self._raw.getint(self._resolve_section(section), key, **kwargs)

    def getfloat(self, section, key, **kwargs):
        return self._raw.getfloat(self._resolve_section(section), key, **kwargs)

    def getboolean(self, section, key, **kwargs):
        return self._raw.getboolean(self._resolve_section(section), key, **kwargs)

    def has_option(self, section, key):
        return self._raw.has_option(self._resolve_section(section), key)

    def has_section(self, section):
        return self._raw.has_section(self._resolve_section(section))

    def __getitem__(self, key):
        return self._raw[self._resolve_section(key)]

    def __contains__(self, key):
        return self._resolve_section(key) in self._raw


def load_config(args):
    config = configparser.ConfigParser()
    config.add_section("server")
    config["server"]["listen_host"] = "127.0.0.1"
    config["server"]["listen_port"] = "8000"
    config["server"]["callsign"]    = "AGWPE"

    if args.config:
        config_file = Path(args.config)
        if config_file.exists():
            config.read(config_file)
            logger.info(f"Loaded config from {args.config}")

    # --- Detect bare [client] / [kiss] sections and migrate to numbered form ---
    if config.has_section("client") and not any(
            s.startswith("client.") for s in config.sections()):
        logger.warning(
            "[client] section is deprecated; rename to [client.0]")
        config.add_section("client.0")
        for k, v in config.items("client"):
            if k not in config.defaults():
                config.set("client.0", k, v)
        config.remove_section("client")

    if config.has_section("kiss") and not any(
            s.startswith("kiss.") for s in config.sections()):
        logger.warning(
            "[kiss] section is deprecated; rename to [kiss.0]")
        config.add_section("kiss.0")
        for k, v in config.items("kiss"):
            if k not in config.defaults():
                config.set("kiss.0", k, v)
        config.remove_section("kiss")

    # --- Apply CLI overrides to port 0 ---
    # Ensure [client.0] exists if no port sections exist yet (CLI-only usage)
    if not any(s.startswith("client.") for s in config.sections()):
        config.add_section("client.0")
        # Set defaults for port 0
        config["client.0"]["type"]             = "serial"
        config["client.0"]["device"]           = "/dev/ttyUSB0"
        config["client.0"]["serial_baudrate"]  = "9600"
        config["client.0"]["ota_baudrate"]     = "1200"

    if args.listen_host:
        config["server"]["listen_host"] = args.listen_host
    if args.listen_port:
        config["server"]["listen_port"] = str(args.listen_port)
    if args.callsign:
        config["server"]["callsign"] = args.callsign
    if args.kiss_type:
        config["client.0"]["type"] = args.kiss_type
    if args.kiss_device:
        config["client.0"]["device"] = args.kiss_device
    if args.kiss_host:
        config["client.0"]["host"] = args.kiss_host
    if args.kiss_port:
        config["client.0"]["port"] = str(args.kiss_port)
    if args.baudrate:
        config["client.0"]["serial_baudrate"] = str(args.baudrate)
    if getattr(args, 'ota_baudrate', None):
        config["client.0"]["ota_baudrate"] = str(args.ota_baudrate)

    # --- Validate ---
    def _parse_port_idx(s, prefix):
        try:
            return int(s[len(prefix):])
        except ValueError:
            return None

    port_sections = sorted(
        (idx, s)
        for s in config.sections()
        if s.startswith("client.")
        for idx in [_parse_port_idx(s, "client.")]
        if idx is not None
    )

    if not port_sections:
        logger.error("No [client.N] sections found in config. "
                     "Define at least [client.0].")
        sys.exit(1)

    # Contiguous numbering from 0
    expected = list(range(len(port_sections)))
    actual   = [idx for idx, _ in port_sections]
    if actual != expected:
        logger.error(
            f"Port numbering must be contiguous starting from 0. "
            f"Found ports: {actual}")
        sys.exit(1)

    # Required fields per connection type
    _REQUIRED = {
        "bluetooth": ["bdaddr"],
        "serial":    ["device"],
        "tcp":       ["host", "port"],
    }
    for idx, section in port_sections:
        ctype = config.get(section, "type", fallback=None)
        if not ctype:
            logger.error(f"[{section}] missing required 'type' field")
            sys.exit(1)
        if ctype not in _REQUIRED:
            logger.error(
                f"[{section}] invalid type '{ctype}'. "
                f"Must be one of: {', '.join(sorted(_REQUIRED))}"
            )
            sys.exit(1)
        required = _REQUIRED.get(ctype, [])
        missing = [f for f in required if not config.has_option(section, f)]
        if missing:
            logger.error(
                f"[{section}] type={ctype} is missing required field(s): "
                f"{', '.join(missing)}")
            sys.exit(1)

    return PortConfig(config)


def main():
    parser = argparse.ArgumentParser(
        description='AGWPE-to-KISS Translation Bridge')

    parser.add_argument(
        '-c', '--config', metavar='FILE',
        help='Configuration file (INI format)')

    server_group = parser.add_argument_group('Server options')
    server_group.add_argument(
        '--listen-host', metavar='HOST',
        help='AGWPE server listen address (default: 127.0.0.1)')
    server_group.add_argument(
        '--listen-port', metavar='PORT', type=int,
        help='AGWPE server listen port (default: 8000)')
    server_group.add_argument(
        '--callsign', metavar='CALL',
        help='Callsign for AGWPE responses (default: AGWPE)')

    client_group = parser.add_argument_group('KISS client options')
    client_group.add_argument(
        '--kiss-type', choices=['serial', 'tcp'],
        help='KISS connection type')
    client_group.add_argument(
        '--kiss-device', metavar='DEVICE',
        help='Serial device (e.g., /dev/ttyUSB0)')
    client_group.add_argument(
        '--kiss-host', metavar='HOST',
        help='KISS TCP host')
    client_group.add_argument(
        '--kiss-port', metavar='PORT', type=int,
        help='KISS TCP port')
    client_group.add_argument(
        '-b', '--baudrate', metavar='BAUD', type=int,
        help='Serial baud rate (default: 9600)')
    client_group.add_argument(
        '--ota-baudrate', metavar='BAUD', type=int,
        help='Over-the-air baud rate for T1 calculation (default: 1200)')

    parser.add_argument(
        '-v', '--verbose', action='count', default=0,
        help=(
            '-v: show AX.25 frame type/src/dst for every frame; '
            '-vv: also show frame data content; '
            '-vvv: also show AGWPE protocol detail'
        ))
    parser.add_argument(
        '-t', '--traffic-debug', action='count', default=0,
        help='Enable raw hex dumps (use -tt for more detail)')

    args = parser.parse_args()

    if args.verbose >= 3:
        logging.getLogger().setLevel(logging.DEBUG)

    try:
        return asyncio.run(main_async(args))
    except KeyboardInterrupt:
        return 0


async def main_async(args):
    config = load_config(args)

    traffic_debug = args.traffic_debug
    if traffic_debug:
        print(f"Traffic debugging enabled (level {traffic_debug})")

    bridge = Bridge(config, traffic_debug, verbose=args.verbose)
    await bridge.start()


if __name__ == '__main__':
    sys.exit(main())
