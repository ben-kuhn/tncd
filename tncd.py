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
import logging
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


def _cmd_frame(to_str, from_str, **kw):
    """Build a command AX.25 frame with correct C/R bit (dest H=1, src H=0)."""
    dst = ax25.Address(to_str)
    dst.command_response = True
    return ax25.Frame(dst=dst, src=ax25.Address(from_str), **kw)


def _resp_frame(to_str, from_str, **kw):
    """Build a response AX.25 frame with correct C/R bit (dest H=0, src H=1)."""
    src = ax25.Address(from_str)
    src.command_response = True
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


DEFAULT_MAX_WINDOW = 3   # mod-8 AX.25: max outstanding I-frames (max 7)
AX25_OVERHEAD = 20       # AX.25 header + KISS framing bytes per frame
T2_MULTIPLIER = 1.2      # T2 = multiplier * frame_time (wait for burst to end)


class Connection:
    """State for a single AX.25 connected-mode session."""

    def __init__(self, port, local, remote):
        self.port = port
        self.local = local    # local callsign (our side)
        self.remote = remote  # remote callsign
        self.state = 'DISCONNECTED'  # CONNECTING | CONNECTED | DISCONNECTING
        self.send_seqno = 0   # N(S): next I-frame seq to send (mod 8)
        self.recv_seqno = 0   # N(R): next I-frame seq expected from remote (mod 8)
        self.unacked = 0      # I-frames sent but not yet acked by remote (for AGWPE 'Y' query)
        self.last_acked = 0   # last N(R) received from remote
        self.owner = None     # AGWPEServerProtocol that initiated/accepted this connection
        self.outbound_queue = collections.deque()  # (data_chunk, pid) awaiting window space
        self.retransmit_buf = {}  # N(S) -> raw AX.25 frame bytes for retransmit
        self.t1_handle = None    # asyncio TimerHandle for T1 retransmit timer
        self.t2_handle = None    # asyncio TimerHandle for T2 delayed ACK timer
        self.t2_pending = False  # True when we owe the remote an RR
        self._last_rr_time = 0.0   # monotonic time of last RR F=1 sent
        self._last_rr_nr = -1      # N(R) of last RR F=1 sent


class AGWPEServerProtocol(asyncio.Protocol):

    def __init__(self, bridge, traffic_debug=0):
        self.bridge = bridge
        self.transport = None
        self.buffer = b''
        self.traffic_debug = traffic_debug
        self.monitoring = False  # toggled by 'm' frame
        self.registered_calls = set()  # callsigns registered via 'X' frame

    def connection_made(self, transport):
        self.transport = transport
        logger.info(f"AGWPE client connected from {transport.get_extra_info('peername')}")
        self.bridge.add_client(self)

    def connection_lost(self, exc):
        logger.info(f"AGWPE client disconnected: {exc}")
        self.bridge.remove_client(self)

    def data_received(self, data):
        self.buffer += data
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

        if datakind_bytes == b'P':
            logger.debug(f"LOGIN from={from_str!r} (accepted)")

        elif datakind_bytes == b'R':
            logger.debug("VERSION request")
            self.send_version()

        elif datakind_bytes == b'G':
            logger.debug("PORT INFO request")
            self.send_port_info()

        elif datakind_bytes == b'g':
            logger.debug(f"PORT CAPABILITIES request for port {port}")
            # Response must be exactly 12 bytes; pe uses struct.unpack('<8BI', data).
            # Fields: baud, traffic_level, tx_delay, tx_tail, persist,
            #         slot_time, max_frame, active_conns, bytes_rcvd
            cfg = self.bridge.config
            tx_delay    = cfg.getint('kiss', 'tx_delay',    fallback=40)
            persistence = cfg.getint('kiss', 'persistence', fallback=63)
            slot_time   = cfg.getint('kiss', 'slot_time',   fallback=20)
            tx_tail     = cfg.getint('kiss', 'tx_tail',     fallback=30)
            caps = struct.pack('<8BI', 0, 255, tx_delay, tx_tail,
                               persistence, slot_time, 7, 0, 0)
            self.send_frame(port, ord(b'g'), b'', b'', caps)

        elif datakind_bytes == b'X':
            logger.debug(f"REGISTER: from={from_str!r}")
            self.registered_calls.add(from_str)
            # pe reads CallFrom from the response to record registered callsign.
            # data[0] != 0 means success.
            self.send_frame(0, ord(b'X'), call_from, b'', b'\x01')

        elif datakind_bytes == b'x':
            # Unregister callsign: per spec, no response is sent.
            logger.debug(f"UNREGISTER: from={from_str!r}")
            self.registered_calls.discard(from_str)

        elif datakind_bytes == b'm':
            # Toggle monitoring on/off per call (spec: same frame type both ways).
            self.monitoring = not self.monitoring
            logger.debug(f"Monitoring {'enabled' if self.monitoring else 'disabled'}")

        elif datakind_bytes == b'M':
            # Send UNPROTO (UI) frame. Build a complete AX.25 UI frame for KISS.
            logger.debug(f"UNPROTO from {from_str!r} to {to_str!r}")
            self._send_unproto(from_str, to_str, pid, data)

        elif datakind_bytes == b'V':
            # Send UNPROTO via digipeaters. Payload: count(1) + vias(10 each) + info.
            logger.debug(f"UNPROTO VIA from {from_str!r} to {to_str!r}")
            if data:
                n_via = data[0]
                via_data = data[1:1 + n_via * 10]
                info = data[1 + n_via * 10:]
                vias = [via_data[i*10:(i+1)*10].rstrip(b'\x00').decode('ascii', errors='replace')
                        for i in range(n_via)]
                self._send_unproto(from_str, to_str, pid, info, via=vias)
            else:
                self._send_unproto(from_str, to_str, pid, b'')

        elif datakind_bytes == b'C':
            # Initiate AX.25 connection: send SABM to TNC.
            logger.info(f"CONNECT     {from_str} -> {to_str}")
            conn = self.bridge.get_or_create_connection(port, from_str, to_str)
            conn.owner = self
            conn.state = 'CONNECTING'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            try:
                frame = _cmd_frame(to_str, from_str,
                                   control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
                self.bridge._send_ax25(frame)
            except Exception as e:
                logger.error(f"Failed to build SABM: {e}")

        elif datakind_bytes == b'c':
            # Connect with non-standard PID (NET/ROM, etc.) — treated as SABM.
            logger.info(f"CONNECT     {from_str} -> {to_str}  (PID=0x{pid:02X})")
            conn = self.bridge.get_or_create_connection(port, from_str, to_str)
            conn.owner = self
            conn.state = 'CONNECTING'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            try:
                frame = _cmd_frame(to_str, from_str,
                                   control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
                self.bridge._send_ax25(frame)
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
            conn.owner = self
            conn.state = 'CONNECTING'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            try:
                dst = ax25.Address(to_str)
                dst.command_response = True
                frame = ax25.Frame(
                    dst=dst,
                    src=ax25.Address(from_str),
                    via=[ax25.Address(v, repeater=True) for v in vias] if vias else None,
                    control=ax25.Control(ax25.FrameType.SABM, poll_final=True),
                )
                self.bridge._send_ax25(frame)
            except Exception as e:
                logger.error(f"Failed to build SABM via: {e}")

        elif datakind_bytes == b'D':
            # Send data over a connected session as an AX.25 I-frame.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if not conn or conn.state != 'CONNECTED':
                logger.warning(
                    f"'D' from {from_str!r} to {to_str!r}: "
                    f"no active connection (state={conn.state if conn else 'None'})")
            else:
                # I-frame info field is limited to 256 bytes; fragment if needed.
                payload = data if data else b''
                chunks = [payload[i:i+256] for i in range(0, len(payload), 256)] or [b'']
                frame_pid = pid if pid else 0xF0
                for chunk in chunks:
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
            else:
                conn.state = 'DISCONNECTING'
                logger.info(f"DISCONNECT  {from_str} -> {to_str}")
                try:
                    frame = _cmd_frame(to_str, from_str,
                                       control=ax25.Control(ax25.FrameType.DISC, poll_final=True))
                    self.bridge._send_ax25(frame)
                except Exception as e:
                    logger.error(f"Failed to build DISC: {e}")

        elif datakind_bytes == b'K':
            # Raw AX.25 frame. pe prepends a 0x00 byte (port indicator), strip it.
            raw = data[1:] if (data and data[0] == 0x00) else data
            logger.debug(f"Raw KISS frame, {len(raw)} bytes")
            if raw:
                self.bridge.send_to_kiss(raw)

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

    def _send_unproto(self, from_str, to_str, pid, data, via=None):
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
            self.bridge._send_ax25(frame)
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
            logger.debug(f"sent response: kind={datakind}, data={data!r}")
            if self.bridge.verbose >= 3:
                kind_ch = chr(datakind) if 32 <= datakind < 127 else f'\\x{datakind:02x}'
                fr_str = from_bytes.rstrip(b'\x00').decode('ascii', 'replace')
                to_str = to_bytes.rstrip(b'\x00').decode('ascii', 'replace')
                print(f"  [AGWPE TX] '{kind_ch}'  port={port}"
                      f"  {fr_str} -> {to_str}  {len(data)} bytes")

    def send_version(self):
        # pe reads: major, minor = struct.unpack('H2xH2x', data) — 8 bytes total
        data = struct.pack('<II', 2, 0)  # MajorRevision=2, MinorRevision=0
        self.send_frame(0, ord(b'R'), b'', b'', data)

    def send_port_info(self):
        # pe parses: data.split(null,1)[0].decode().split(';')
        # Non-empty fields after the first become the port description list.
        data = b'1;KISS TNC;'
        self.send_frame(0, ord(b'G'), b'', b'', data)


class KISSClient:
    def __init__(self, config, traffic_debug=0):
        self.config = config
        self.connection = None
        self.bridge = None
        self.traffic_debug = traffic_debug
        self._rx_thread = None

    def set_bridge(self, bridge):
        self.bridge = bridge

    def _get_kiss_params(self):
        params = {}
        if 'kiss' in self.config:
            for key in ['TX_DELAY', 'PERSISTENCE', 'SLOT_TIME', 'TX_TAIL', 'FULL_DUPLEX']:
                if self.config.has_option('kiss', key.lower()):
                    params[key] = self.config.getint('kiss', key.lower())
        return params

    async def connect(self):
        conn_type  = self.config.get('client', 'type', fallback='serial')
        kiss_params = self._get_kiss_params()

        loop = asyncio.get_running_loop()

        if conn_type == 'tcp':
            host = self.config.get('client', 'host', fallback='localhost')
            port = self.config.getint('client', 'port', fallback=8001)
            logger.info(f"Connecting to KISS TCP server at {host}:{port}")
            self.connection = kiss.TCPKISS(host=host, port=port)
        else:
            device   = self.config.get('client', 'device', fallback='/dev/ttyUSB0')
            baudrate = self.config.getint('client', 'serial_baudrate',
                                          fallback=self.config.getint('client', 'baudrate', fallback=9600))
            parity   = self.config.get('client', 'parity', fallback='N').upper()
            stopbits = self.config.getfloat('client', 'stopbits', fallback=1)
            rtscts   = self.config.getboolean('client', 'rtscts', fallback=False)
            logger.info(f"Connecting to KISS serial device at {device}, {baudrate} baud")

            init_str = self.config.get('client', 'init_string', fallback=None)
            if init_str:
                import serial as _serial
                init_delay = self.config.getfloat('client', 'init_delay', fallback=1.0)
                init_bytes = init_str.replace('\\r', '\r').replace('\\n', '\n').encode()
                logger.info(f"Sending TNC init string: {init_str!r}")
                with _serial.Serial(device, baudrate, parity=parity, stopbits=stopbits,
                                    rtscts=rtscts, timeout=1) as ser:
                    ser.write(init_bytes)
                time.sleep(init_delay)

            self.connection = kiss.SerialKISS(port=device, speed=baudrate)

        def blocking_start():
            self.connection.start(**kiss_params)
            if conn_type != 'tcp' and (parity != 'N' or stopbits != 1 or rtscts):
                logger.info(f"Reconfiguring serial port: parity={parity}, stopbits={stopbits}, rtscts={rtscts}")
                ser = self.connection.protocol.transport.serial
                ser.parity   = parity
                ser.stopbits = stopbits
                ser.rtscts   = rtscts

        with ThreadPoolExecutor() as executor:
            await loop.run_in_executor(executor, blocking_start)

        logger.info("KISS connection established")

    def start_receive(self, loop):
        """Start a background thread that reads frames from the KISS TNC.

        Each received AX.25 frame is dispatched to bridge.on_kiss_frame()
        on the asyncio event loop thread via call_soon_threadsafe.
        """
        def _on_frame(frame_data):
            loop.call_soon_threadsafe(self.bridge.on_kiss_frame, bytes(frame_data))

        def _read_loop():
            try:
                # min_frames=None blocks until the connection closes.
                self.connection.read(callback=_on_frame, min_frames=None)
            except Exception as e:
                if self.connection is not None:
                    logger.error(f"KISS RX error: {e}")

        self._rx_thread = threading.Thread(target=_read_loop, daemon=True,
                                           name='kiss-rx')
        self._rx_thread.start()
        logger.debug("KISS RX thread started")

    def send(self, data):
        if self.connection:
            if self.traffic_debug:
                print(hex_dump(data, prefix="KISS TX: "))
            self.connection.write(data)

    def close(self):
        if self.connection:
            self.connection.stop()
            self.connection = None


class Bridge:
    def __init__(self, config, traffic_debug=0, verbose=0):
        self.config = config
        self.kiss_client = KISSClient(config, traffic_debug)
        self.clients = []
        self.connections = {}   # (port, local, remote) -> Connection
        self.callsign = config.get('server', 'callsign', fallback='AGWPE')
        self.traffic_debug = traffic_debug
        self.verbose = verbose
        # TX echo detection: some TNCs (e.g. UV-Pro) echo transmitted frames
        # back via KISS. Track recently-sent raw bytes to discard them.
        self._sent_frames = collections.deque(maxlen=20)

        # Window size and T1 timer calculation
        self.max_window = config.getint('ax25', 'max_window', fallback=DEFAULT_MAX_WINDOW)
        self.max_window = max(1, min(7, self.max_window))  # clamp to 1-7

        # Calculate T1 and T2 from OTA baud rate
        ota_baudrate = config.getint('client', 'ota_baudrate', fallback=1200)
        max_frame_bytes = 256 + AX25_OVERHEAD
        frame_time = (max_frame_bytes * 8) / ota_baudrate

        # T1 = 2 * (window_frames * frame_time + turnaround)
        turnaround = 1.0  # processing + channel turnaround
        self.t1_timeout = 2.0 * (self.max_window * frame_time + turnaround)
        self.t1_timeout = max(3.0, self.t1_timeout)  # floor at 3 seconds

        # T2 = slightly longer than one frame time — fires after burst ends
        self.t2_delay = T2_MULTIPLIER * frame_time
        self.t2_delay = max(0.1, self.t2_delay)  # floor at 100ms

        logger.info(f"AX.25 window={self.max_window}, T1={self.t1_timeout:.1f}s, "
                    f"T2={self.t2_delay:.2f}s (ota_baudrate={ota_baudrate})")

    async def start(self):
        await self.kiss_client.connect()
        self.kiss_client.set_bridge(self)

        loop = asyncio.get_running_loop()
        self.kiss_client.start_receive(loop)

        server_cfg = self.config['server']
        host = server_cfg.get('listen_host', '0.0.0.0')
        port = server_cfg.getint('listen_port', 8000)

        logger.info(f"Starting AGWPE server on {host}:{port}")

        server = await loop.create_server(
            lambda: AGWPEServerProtocol(self, self.traffic_debug),
            host, port
        )

        logger.info("Bridge running. Press Ctrl+C to stop.")

        try:
            async with server:
                await server.serve_forever()
        except KeyboardInterrupt:
            logger.info("Shutting down...")

    def add_client(self, client):
        self.clients.append(client)

    def remove_client(self, client):
        if client in self.clients:
            self.clients.remove(client)

    def _conn_key(self, port, local, remote):
        return (port, local.strip().upper(), remote.strip().upper())

    def get_connection(self, port, local, remote):
        return self.connections.get(self._conn_key(port, local, remote))

    def get_or_create_connection(self, port, local, remote):
        key = self._conn_key(port, local, remote)
        if key not in self.connections:
            p, loc, rem = key
            self.connections[key] = Connection(p, loc, rem)
        return self.connections[key]

    def remove_connection(self, port, local, remote):
        conn = self.connections.pop(self._conn_key(port, local, remote), None)
        if conn:
            self._cancel_t1(conn)
            self._cancel_t2(conn)

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

    def _send_ax25(self, frame):
        """Log (at verbose>=1) and send an AX.25 frame to the KISS TNC."""
        self._log_ax25(frame, 'TX')
        self.send_to_kiss(bytes(frame))

    def send_to_kiss(self, data):
        self._sent_frames.append(bytes(data))
        self.kiss_client.send(data)

    def _drain_outbound(self, conn):
        """Send queued I-frames while the outbound window has space.

        Coalesces adjacent queue entries with the same PID into single
        I-frames up to 256 bytes, since AGWPE clients may send small
        'D' payloads (e.g. PAT sends 127-byte chunks).
        """
        sent_any = False
        sent_count = 0
        while conn.outbound_queue and conn.unacked < self.max_window:
            chunk, pid = conn.outbound_queue.popleft()
            # Coalesce adjacent entries with the same PID up to 256 bytes.
            while conn.outbound_queue and len(chunk) < 256:
                next_chunk, next_pid = conn.outbound_queue[0]
                if next_pid != pid or len(chunk) + len(next_chunk) > 256:
                    break
                conn.outbound_queue.popleft()
                chunk = chunk + next_chunk
            try:
                frame = _cmd_frame(conn.remote, conn.local,
                                   control=ax25.Control(ax25.FrameType.I,
                                                        send_seqno=conn.send_seqno,
                                                        recv_seqno=conn.recv_seqno),
                                   pid=pid,
                                   data=chunk)
                ns = conn.send_seqno
                self._send_ax25(frame)
                conn.retransmit_buf[ns] = bytes(frame)
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
            conn.t1_handle = loop.call_later(self.t1_timeout, self._t1_expired, conn)
        except RuntimeError:
            pass  # no event loop (e.g. in tests)

    def _cancel_t1(self, conn):
        """Cancel the T1 timer if running."""
        if conn.t1_handle is not None:
            conn.t1_handle.cancel()
            conn.t1_handle = None

    def _schedule_t2(self, conn, src, dst):
        """Schedule a delayed RR (T2 timer).  Resets the timer on each call
        so a burst of I-frames results in a single RR after the burst."""
        self._cancel_t2(conn)
        conn.t2_pending = True
        conn._t2_src = str(src)
        conn._t2_dst = str(dst)
        try:
            loop = asyncio.get_running_loop()
            conn.t2_handle = loop.call_later(self.t2_delay, self._t2_expired, conn)
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

    def _send_delayed_rr(self, conn):
        """Send RR with current V(R) for delayed acknowledgment."""
        logger.info(f"TX RR(n(r)={conn.recv_seqno}) to {conn._t2_src} [T2 delayed]")
        try:
            rr = _resp_frame(conn._t2_src, conn._t2_dst,
                             control=ax25.Control(ax25.FrameType.RR,
                                                  recv_seqno=conn.recv_seqno))
            self._send_ax25(rr)
        except Exception as e:
            logger.error(f"Failed to send delayed RR: {e}")

    def _t1_expired(self, conn):
        """T1 timer fired: poll the remote with RR P=1 and retransmit unacked frames."""
        conn.t1_handle = None
        if conn.state != 'CONNECTED' or not conn.retransmit_buf:
            return
        logger.debug(f"T1 expired for {conn.local}<->{conn.remote}, "
                     f"{conn.unacked} unacked, retransmitting from {conn.last_acked}")
        try:
            rr = _cmd_frame(conn.remote, conn.local,
                            control=ax25.Control(ax25.FrameType.RR,
                                                 poll_final=True,
                                                 recv_seqno=conn.recv_seqno))
            self._send_ax25(rr)
        except Exception as e:
            logger.error(f"Failed to send T1 poll: {e}")
        self._retransmit_from(conn, conn.last_acked)
        self._start_t1(conn)

    def _ack_frames(self, conn, r_seq):
        """Process cumulative ACK: update unacked, purge retransmit buffer, drain queue."""
        newly_acked = (r_seq - conn.last_acked) % 8
        # Guard against backwards N(R) from retransmitted frames.
        # A retransmit may carry an old N(R) that's behind last_acked;
        # the mod-8 arithmetic would interpret this as a huge forward ACK.
        # Only accept N(R) that ACKs at most max_window frames.
        if newly_acked > self.max_window:
            logger.debug(f"Ignoring backwards N(R)={r_seq} (last_acked={conn.last_acked})")
            return
        if newly_acked:
            logger.info(f"_ack_frames: N(R)={r_seq}, acked {newly_acked} frames, "
                        f"unacked {conn.unacked}->{conn.unacked - newly_acked}, "
                        f"queue={len(conn.outbound_queue)}")
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
                str(orig.dst), str(orig.src),
                control=ax25.Control(
                    ax25.FrameType.I,
                    send_seqno=orig.control.send_seqno,
                    recv_seqno=conn.recv_seqno,
                    poll_final=orig.control.poll_final),
                pid=orig.pid,
                data=orig.data)
            conn.retransmit_buf[seq] = bytes(frame)
            self._send_ax25(frame)
            seq = (seq + 1) % 8

    # ------------------------------------------------------------------
    # KISS receive path
    # ------------------------------------------------------------------

    def on_kiss_frame(self, raw_kiss):
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
        if raw_ax25 in self._sent_frames:
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
            self._dispatch_ui(frame, src, dst)
        elif ft.is_I():
            self._dispatch_i(frame, src, dst)
        elif ft.is_S():
            self._dispatch_s(frame, src, dst)
        elif ft is ax25.FrameType.SABM:
            self._dispatch_sabm(frame, src, dst)
        elif ft is ax25.FrameType.SABME:
            # Reject extended (mod-128) mode — we only support basic (mod-8).
            # Remote should fall back to SABM.
            logger.debug(f"Rejecting SABME from {src} (mod-128 not supported)")
            try:
                dm = _resp_frame(src, dst,
                                 control=ax25.Control(ax25.FrameType.DM, poll_final=True))
                self._send_ax25(dm)
            except Exception as e:
                logger.error(f"Failed to send DM for SABME: {e}")
        elif ft is ax25.FrameType.UA:
            self._dispatch_ua(frame, src, dst)
        elif ft is ax25.FrameType.DM:
            self._dispatch_dm(frame, src, dst)
        elif ft is ax25.FrameType.DISC:
            self._dispatch_disc(frame, src, dst)
        else:
            logger.debug(f"Received AX.25 {ft.name} frame, not forwarded")

    def _monitor_text(self, frame_type_str, src, dst, pid, data_len):
        """Format AGWPE monitor header: 'Fm SRC To DST <TYPE pid=XX Len=N >[HH:MM:SS]'"""
        ts = datetime.now().strftime('%H:%M:%S')
        return (
            f"Fm {src} To {dst}"
            f" <{frame_type_str} pid={pid:02X} Len={data_len} >"
            f"[{ts}]\r"
        ).encode()

    def _dispatch_ui(self, frame, src, dst):
        """Forward a UI (UNPROTO) frame to monitoring clients as 'U'."""
        pid  = frame.pid
        data = frame.data or b''
        payload = self._monitor_text('UI', src, dst, pid, len(data)) + data
        for client in self.clients:
            if client.monitoring:
                try:
                    client.send_frame(0, ord('U'), src.encode(), dst.encode(),
                                      payload, pid=pid)
                except Exception as e:
                    logger.error(f"Error sending 'U' to client: {e}")

    def _dispatch_i(self, frame, src, dst):
        """Handle received I-frame: deliver data to connection owner and monitoring clients."""
        pid  = frame.pid
        data = frame.data or b''
        logger.info(f"RX I-frame {src}->{dst} N(S)={frame.control.send_seqno} "
                    f"N(R)={frame.control.recv_seqno} P={frame.control.poll_final} "
                    f"{len(data)}B")

        # Find connection: local=dst (frame addressed to us), remote=src
        conn = self.get_connection(0, dst, src)
        if not conn or conn.state != 'CONNECTED':
            # No active connection — send DM so the remote knows to disconnect.
            if frame.control.poll_final:
                try:
                    dm = _resp_frame(src, dst,
                                     control=ax25.Control(ax25.FrameType.DM,
                                                          poll_final=True))
                    self._send_ax25(dm)
                    logger.info(f"TX DM to {src} (no connection for I-frame)")
                except Exception as e:
                    logger.error(f"Failed to send DM: {e}")
        elif conn.state == 'CONNECTED':
            # I-frames carry N(R) which implicitly ACKs our sent frames.
            self._ack_frames(conn, frame.control.recv_seqno)

            expected_ns = conn.recv_seqno
            if frame.control.send_seqno != expected_ns:
                # Duplicate or out-of-order frame — discard data but still
                # respond to a poll so the remote knows our current V(R).
                logger.info(f"Discarding duplicate I frame N(S)={frame.control.send_seqno}"
                            f" (expected {expected_ns})")
                if frame.control.poll_final:
                    self._cancel_t2(conn)
                    self._send_rr_guarded(conn, src, dst, 'dup poll')
                else:
                    # Re-send our V(R) so the remote learns the ACK it missed.
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
                        conn.owner.send_frame(0, ord('D'), src.encode(), dst.encode(),
                                              data, pid=pid)
                    except Exception as e:
                        logger.error(f"Error delivering 'D' to client: {e}")

        # Monitoring clients get 'I' monitor frame regardless
        payload = self._monitor_text('I', src, dst, pid, len(data)) + data
        for client in self.clients:
            if client.monitoring:
                try:
                    client.send_frame(0, ord('I'), src.encode(), dst.encode(),
                                      payload, pid=pid)
                except Exception as e:
                    logger.error(f"Error sending 'I' to client: {e}")

    def _dispatch_sabm(self, frame, src, dst):
        """Handle incoming SABM/SABME: accept connection, notify AGWPE clients."""
        logger.info(f"CONNECT     {src} -> {dst}  (incoming)")
        try:
            ua = _resp_frame(str(frame.src), str(frame.dst),
                             control=ax25.Control(ax25.FrameType.UA, poll_final=True))
            self._send_ax25(ua)
        except Exception as e:
            logger.error(f"Failed to send UA for incoming SABM: {e}")
            return

        conn = self.get_or_create_connection(0, dst, src)
        conn.state = 'CONNECTED'
        conn.send_seqno = 0
        conn.recv_seqno = 0
        conn.unacked = 0
        conn.last_acked = 0

        # Assign owner to the client that registered this callsign.
        for client in self.clients:
            try:
                if dst in client.registered_calls:
                    conn.owner = client
                    break
            except TypeError:
                pass

        msg = f'*** CONNECTED To Station {src}\r'.encode()
        for client in self.clients:
            try:
                client.send_frame(0, ord('C'), src.encode(), dst.encode(), msg)
            except Exception as e:
                logger.error(f"Error sending 'C' notification: {e}")

    def _dispatch_ua(self, frame, src, dst):
        """Handle UA: outgoing connection established, or disconnect confirmed."""
        # src=remote sent UA, dst=local received it
        conn = self.get_connection(0, dst, src)
        if conn is None:
            logger.debug(f"UA from {src}: no pending connection")
            return

        if conn.state == 'CONNECTING':
            conn.state = 'CONNECTED'
            conn.send_seqno = 0
            conn.recv_seqno = 0
            logger.info(f"CONNECTED   {dst} <-> {src}")
            msg = f'*** CONNECTED With {src}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(0, ord('C'), src.encode(), dst.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'C' to owner: {e}")

        elif conn.state == 'DISCONNECTING':
            logger.info(f"DISCONNECTED {dst} <-> {src}")
            msg = f'*** DISCONNECTED From {src}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(0, ord('d'), src.encode(), dst.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'd' to owner: {e}")
            self.remove_connection(0, dst, src)

    def _dispatch_dm(self, frame, src, dst):
        """Handle DM: connection refused or remote forced disconnect."""
        conn = self.get_connection(0, dst, src)
        if conn is None:
            logger.debug(f"DM from {src}: no active connection")
            return
        logger.info(f"REJECTED    {dst} <-> {src}  (DM, was {conn.state})")
        msg = (f'*** CONNECTED With {src} failed\r'.encode()
               if conn.state == 'CONNECTING'
               else f'*** DISCONNECTED From {src}\r'.encode())
        if conn.owner:
            try:
                conn.owner.send_frame(0, ord('d'), src.encode(), dst.encode(), msg)
            except Exception as e:
                logger.error(f"Error sending 'd' for DM: {e}")
        self.remove_connection(0, dst, src)

    def _dispatch_disc(self, frame, src, dst):
        """Handle remote DISC: send UA, notify AGWPE client of disconnection."""
        logger.info(f"DISCONNECT  {src} -> {dst}  (remote)")
        try:
            ua = _resp_frame(str(frame.src), str(frame.dst),
                             control=ax25.Control(ax25.FrameType.UA, poll_final=True))
            self._send_ax25(ua)
        except Exception as e:
            logger.error(f"Failed to send UA for DISC: {e}")

        conn = self.get_connection(0, dst, src)
        if conn:
            msg = f'*** DISCONNECTED From {src}\r'.encode()
            if conn.owner:
                try:
                    conn.owner.send_frame(0, ord('d'), src.encode(), dst.encode(), msg)
                except Exception as e:
                    logger.error(f"Error sending 'd' for DISC: {e}")
            self.remove_connection(0, dst, src)

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
            rr = _resp_frame(src, dst,
                             control=ax25.Control(ax25.FrameType.RR,
                                                  poll_final=True,
                                                  recv_seqno=nr))
            self._send_ax25(rr)
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

    def _dispatch_s(self, frame, src, dst):
        """Handle received S (supervisory) frame: respond to polls, forward to monitors."""
        ft = frame.control.frame_type
        ft_name = ft.name
        try:
            r_seq = frame.control.recv_seqno
        except TypeError:
            r_seq = 0

        conn = self.get_connection(0, dst, src)
        logger.info(f"RX {ft_name} {src}->{dst} N(R)={r_seq} "
                    f"P={frame.control.poll_final} "
                    f"recv_seqno={conn.recv_seqno if conn else '?'}")

        # Update unacked count and purge retransmit buffer.
        if conn and conn.state == 'CONNECTED':
            self._ack_frames(conn, r_seq)

            # REJ: retransmit from the requested sequence number.
            if ft is ax25.FrameType.REJ:
                self._retransmit_from(conn, r_seq)

        # If the remote set P=1 (poll), we must respond with RR F=1.
        # Defer via call_soon so that any I-frames already queued on the
        # event loop (from the same KISS burst) are processed first,
        # advancing recv_seqno before we build the response.
        if frame.control.poll_final:
            conn = self.get_connection(0, dst, src)
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
                    client.send_frame(0, ord('S'), src.encode(), dst.encode(), payload)
                except Exception as e:
                    logger.error(f"Error sending 'S' to client: {e}")


def load_config(args):
    config = configparser.ConfigParser()
    config.add_section("server")
    config.add_section("client")
    config.add_section("kiss")
    config.add_section("ax25")
    config["server"]["listen_host"] = "0.0.0.0"
    config["server"]["listen_port"] = "8000"
    config["server"]["callsign"]    = "AGWPE"
    config["client"]["type"]        = "serial"
    config["client"]["device"]      = "/dev/ttyUSB0"
    config["client"]["serial_baudrate"] = "9600"
    config["client"]["ota_baudrate"]    = "1200"
    config["kiss"]["tx_delay"]      = "40"
    config["kiss"]["persistence"]   = "63"
    config["kiss"]["slot_time"]     = "20"
    config["kiss"]["tx_tail"]       = "30"
    config["kiss"]["full_duplex"]   = "0"

    if args.config:
        config_file = Path(args.config)
        if config_file.exists():
            config.read(config_file)
            logger.info(f"Loaded config from {args.config}")

    if args.listen_host:
        config["server"]["listen_host"] = args.listen_host
    if args.listen_port:
        config["server"]["listen_port"] = str(args.listen_port)
    if args.callsign:
        config["server"]["callsign"] = args.callsign
    if args.kiss_type:
        config["client"]["type"] = args.kiss_type
    if args.kiss_device:
        config["client"]["device"] = args.kiss_device
    if args.kiss_host:
        config["client"]["host"] = args.kiss_host
    if args.kiss_port:
        config["client"]["port"] = str(args.kiss_port)
    if args.baudrate:
        config["client"]["serial_baudrate"] = str(args.baudrate)
    if getattr(args, 'ota_baudrate', None):
        config["client"]["ota_baudrate"] = str(args.ota_baudrate)

    return config


def main():
    parser = argparse.ArgumentParser(
        description='AGWPE-to-KISS Translation Bridge')

    parser.add_argument(
        '-c', '--config', metavar='FILE',
        help='Configuration file (INI format)')

    server_group = parser.add_argument_group('Server options')
    server_group.add_argument(
        '--listen-host', metavar='HOST',
        help='AGWPE server listen address (default: 0.0.0.0)')
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
