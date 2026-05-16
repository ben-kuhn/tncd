import asyncio
import pytest
import argparse
import ax25
import configparser
import re
import socket
import struct
from unittest.mock import Mock, MagicMock, patch

import os
import tempfile

from tncd import (
    AGWPEServerProtocol, BluetoothKISS, Bridge, Connection, KISSClient,
    AGWPE_HEADER_FORMAT, AGWPE_HEADER_SIZE, DEFAULT_MAX_WINDOW,
    DEFAULT_N2_RETRY, load_config, PortConfig,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def make_frame(port, datakind, call_from=b'', call_to=b'', data=b'', pid=0xF0):
    """Build a well-formed AGWPE frame."""
    from_bytes = (call_from + b'\x00' * 10)[:10]
    to_bytes   = (call_to   + b'\x00' * 10)[:10]
    header = struct.pack(
        AGWPE_HEADER_FORMAT,
        port, 0, 0, 0,
        datakind,
        0, pid, 0,
        from_bytes, to_bytes,
        len(data), 0,
    )
    return header + data


def parse_frame(raw):
    """Parse the first frame out of raw bytes; return a dict."""
    values = struct.unpack(AGWPE_HEADER_FORMAT, raw[:AGWPE_HEADER_SIZE])
    data_len = values[10]
    return {
        'port':      values[0],
        'datakind':  values[4],
        'kind':      chr(values[4]),
        'pid':       values[6],
        'call_from': values[8].rstrip(b'\x00'),
        'call_to':   values[9].rstrip(b'\x00'),
        'data':      raw[AGWPE_HEADER_SIZE:AGWPE_HEADER_SIZE + data_len],
    }


def make_protocol():
    """Return (protocol, transport_mock, bridge_mock) ready for use."""
    config = configparser.ConfigParser()
    config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000', 'callsign': 'N0CALL'}
    config['client'] = {'type': 'serial', 'device': '/dev/null', 'baudrate': '9600'}
    config['kiss']   = {'tx_delay': '40', 'persistence': '63', 'slot_time': '20', 'tx_tail': '30'}

    bridge = Mock()
    bridge.config = config
    bridge.verbose = 0
    bridge.get_connection.return_value = None

    protocol  = AGWPEServerProtocol(bridge)
    transport = Mock()
    transport.is_closing.return_value = False
    protocol.connection_made(transport)
    return protocol, transport, bridge


# ---------------------------------------------------------------------------
# load_config
# ---------------------------------------------------------------------------

class TestLoadConfig:
    def test_defaults(self):
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type=None, kiss_device=None, kiss_host=None,
            kiss_port=None, baudrate=None,
        )
        cfg = load_config(args)
        assert cfg.get('server', 'listen_host') == '0.0.0.0'
        assert cfg.getint('server', 'listen_port') == 8000

    def test_cli_overrides(self):
        args = argparse.Namespace(
            config=None, listen_host='192.168.1.1', listen_port=9000,
            callsign='MYCALL', kiss_type='tcp', kiss_device=None,
            kiss_host='kiss.example.com', kiss_port=8001, baudrate=None,
        )
        cfg = load_config(args)
        assert cfg.get('server', 'listen_host') == '192.168.1.1'
        assert cfg.getint('server', 'listen_port') == 9000
        assert cfg.get('client', 'type') == 'tcp'
        assert cfg.get('client', 'host') == 'kiss.example.com'

    def test_baudrate_override(self):
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type='serial', kiss_device='/dev/ttyS0', kiss_host=None,
            kiss_port=None, baudrate=19200,
        )
        cfg = load_config(args)
        assert cfg.getint('client', 'serial_baudrate') == 19200


# ---------------------------------------------------------------------------
# Version response ('R')
# pe reads: major, minor = struct.unpack('H2xH2x', data)  => data must be 8 bytes
# ---------------------------------------------------------------------------

class TestVersionResponse:
    def test_response_kind_is_R(self):
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('R'), b'CLIENT', b'AGWPE'))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['kind'] == 'R'

    def test_response_payload_is_8_bytes(self):
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('R')))
        raw = transport.write.call_args[0][0]
        assert len(raw[AGWPE_HEADER_SIZE:]) == 8

    def test_response_parseable_as_version(self):
        """pe does struct.unpack('H2xH2x', data) — must not raise."""
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('R')))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        major, minor = struct.unpack('H2xH2x', payload)
        assert major == 2


# ---------------------------------------------------------------------------
# Port info response ('G')
# pe parses: data.split(bytearray(1), 1)[0].decode().split(';')
# first element = port count (int), remaining = port descriptions (non-empty)
# ---------------------------------------------------------------------------

class TestPortInfoResponse:
    def test_response_kind_is_G(self):
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('G')))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['kind'] == 'G'

    def test_port_count_and_descriptions_consistent(self):
        """Count in first field must match number of non-empty descriptions."""
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('G')))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        text = payload.split(b'\x00')[0].decode()
        parts = text.split(';')
        port_count = int(parts[0])
        descriptions = [p for p in parts[1:] if p.strip()]
        assert port_count >= 1
        assert len(descriptions) >= port_count


# ---------------------------------------------------------------------------
# Port capabilities response ('g')
# pe: _FRAME_INFO['g'] requires exactly 12 bytes
# pe: PortCaps.unpack() uses struct.unpack('<8BI', data)
# ---------------------------------------------------------------------------

class TestPortCapsResponse:
    def test_response_kind_is_g(self):
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('g')))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['kind'] == 'g'

    def test_response_payload_is_exactly_12_bytes(self):
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('g')))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        assert len(payload) == 12

    def test_payload_parseable_by_pe_portcaps(self):
        """pe uses struct.unpack('<8BI', data) for PortCaps — must not raise."""
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('g')))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        values = struct.unpack('<8BI', payload)
        assert len(values) == 9

    def test_port_number_echoed(self):
        """Response port field should match the requested port."""
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(1, ord('g')))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['port'] == 1


# ---------------------------------------------------------------------------
# Register callsign response ('X')
# pe: data[0] must be nonzero for success; CallFrom echoed back
# ---------------------------------------------------------------------------

class TestRegisterCallsign:
    def test_response_kind_is_X(self):
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('X'), b'N0CALL'))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['kind'] == 'X'

    def test_response_data_nonzero(self):
        """pe treats data[0] == 0 as failure."""
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('X'), b'N0CALL'))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        assert len(payload) >= 1
        assert payload[0] != 0

    def test_response_echoes_callsign_in_call_from(self):
        """pe reads CallFrom from 'X' response to record the registered callsign."""
        protocol, transport, _ = make_protocol()
        protocol.data_received(make_frame(0, ord('X'), b'W1ABC'))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['call_from'] == b'W1ABC'


# ---------------------------------------------------------------------------
# Full initialization sequence: R -> G -> g
# This is exactly what pyham_pe's _InitializingHandler drives.
# All three responses must be correct or Paracon hangs forever.
# ---------------------------------------------------------------------------

class TestInitializationSequence:
    def test_full_init_sequence(self):
        protocol, transport, _ = make_protocol()

        # Step 1: version
        protocol.data_received(make_frame(0, ord('R')))
        r_resp = parse_frame(transport.write.call_args[0][0])
        assert r_resp['kind'] == 'R'
        assert len(transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]) == 8

        # Step 2: port info
        transport.write.reset_mock()
        protocol.data_received(make_frame(0, ord('G')))
        g_resp = parse_frame(transport.write.call_args[0][0])
        assert g_resp['kind'] == 'G'

        # Step 3: port capabilities
        transport.write.reset_mock()
        protocol.data_received(make_frame(0, ord('g')))
        gcaps_resp = parse_frame(transport.write.call_args[0][0])
        assert gcaps_resp['kind'] == 'g'
        caps_payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        assert len(caps_payload) == 12
        struct.unpack('<8BI', caps_payload)  # must not raise


# ---------------------------------------------------------------------------
# Data frames forwarded to KISS
# ---------------------------------------------------------------------------

class TestDataForwarding:
    def test_unproto_M_sends_ax25_ui_frame_to_kiss(self):
        """'M' must build a complete AX.25 UI frame, not send raw AGWPE payload."""
        protocol, transport, bridge = make_protocol()
        data = b'Hello APRS'
        protocol.data_received(make_frame(0, ord('M'), b'W1ABC', b'APRS', data,
                                           pid=0xF0))
        bridge._send_ax25.assert_called_once()
        frame = bridge._send_ax25.call_args[0][0]
        assert str(frame.dst) == 'APRS'
        assert str(frame.src) == 'W1ABC'
        assert frame.control.frame_type is ax25.FrameType.UI
        assert frame.data == data

    def test_unproto_M_pid_preserved(self):
        """PID from AGWPE header is used in the AX.25 frame."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('M'), b'W1ABC', b'APRS',
                                           b'test', pid=0xF0))
        frame = bridge._send_ax25.call_args[0][0]
        assert frame.pid == 0xF0

# ---------------------------------------------------------------------------
# Buffering / reassembly
# ---------------------------------------------------------------------------

class TestLoginFrame:
    def test_P_login_produces_no_response(self):
        """'P' login: spec says accept silently, no response sent."""
        protocol, transport, bridge = make_protocol()
        data = b'\x00' * 510  # userid(255) + password(255)
        protocol.data_received(make_frame(0, ord('P'), b'', b'', data))
        transport.write.assert_not_called()

    def test_P_login_does_not_forward_to_kiss(self):
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('P'), b'', b'', b'\x00' * 510))
        bridge.send_to_kiss.assert_not_called()


class TestUnregisterCallsign:
    def test_x_unregister_produces_no_response(self):
        """'x' unregister: spec says no response."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('x'), b'W1ABC'))
        transport.write.assert_not_called()

    def test_x_unregister_does_not_forward_to_kiss(self):
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('x'), b'W1ABC'))
        bridge.send_to_kiss.assert_not_called()


class TestOutstandingFrames:
    def test_y_port_outstanding_response_kind(self):
        """'y' must respond with 'y' frame."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('y')))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['kind'] == 'y'

    def test_y_port_outstanding_response_is_4_bytes(self):
        """pe reads: (frames,) = struct.unpack('I', data) — must be 4 bytes."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('y')))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        assert len(payload) == 4

    def test_y_port_outstanding_parseable(self):
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('y')))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        (count,) = struct.unpack('<I', payload)
        assert count >= 0

    def test_Y_connection_outstanding_response_kind(self):
        """'Y' must respond with 'Y' frame."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('Y'), b'W1ABC', b'W2DEF'))
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['kind'] == 'Y'

    def test_Y_connection_outstanding_response_is_4_bytes(self):
        """pe reads: (frames,) = struct.unpack('I', data) — must be 4 bytes."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('Y'), b'W1ABC', b'W2DEF'))
        payload = transport.write.call_args[0][0][AGWPE_HEADER_SIZE:]
        assert len(payload) == 4


class TestHeardStations:
    def test_H_query_produces_response(self):
        """'H' heard stations query must produce an 'H' response frame."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(1, ord('H')))
        transport.write.assert_called_once()
        parsed = parse_frame(transport.write.call_args[0][0])
        assert parsed['kind'] == 'H'


class TestRawKissFrame:
    def test_K_raw_frame_strips_pe_prefix_byte(self):
        """'K' frame: pe prepends 0x00 port byte; must be stripped before KISS TX."""
        protocol, transport, bridge = make_protocol()
        raw_ax25 = b'\x82\xa0\xa4\xa6@@`\xaeb\x82\x84\x86@a\x03\xf0hello'
        # pe format: 0x00 + raw_ax25
        protocol.data_received(make_frame(0, ord('K'), b'', b'', b'\x00' + raw_ax25))
        bridge.send_to_kiss.assert_called_once_with(raw_ax25)

    def test_k_raw_mode_toggle_produces_no_response(self):
        """'k' raw mode toggle: no response, no KISS forward."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('k')))
        transport.write.assert_not_called()
        bridge.send_to_kiss.assert_not_called()


class TestMonitoring:
    def test_m_toggles_monitoring_on(self):
        """First 'm' enables monitoring."""
        protocol, transport, bridge = make_protocol()
        assert protocol.monitoring is False
        protocol.data_received(make_frame(0, ord('m')))
        assert protocol.monitoring is True

    def test_m_toggles_monitoring_off(self):
        """Second 'm' disables monitoring."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('m')))
        protocol.data_received(make_frame(0, ord('m')))
        assert protocol.monitoring is False

    def test_m_produces_no_response(self):
        """'m' monitoring toggle: no response frame."""
        protocol, transport, bridge = make_protocol()
        protocol.data_received(make_frame(0, ord('m')))
        transport.write.assert_not_called()


class TestKISSReceivePath:
    """Verify frames received from KISS are forwarded to monitoring clients."""

    def _make_bridge_with_client(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                             'callsign': 'N0CALL'}
        config['client'] = {'type': 'serial', 'device': '/dev/null', 'baudrate': '9600'}
        config['kiss']   = {'tx_delay': '40', 'persistence': '63',
                             'slot_time': '20', 'tx_tail': '30'}
        bridge = Bridge(config)
        # Mock out the kiss_client so we don't actually connect
        bridge.kiss_client = Mock()
        # Add a mock monitoring client
        client = Mock()
        client.monitoring = True
        bridge.add_client(client)
        return bridge, client

    def test_ui_frame_dispatched_as_U(self):
        """A received UI frame must be forwarded to monitoring clients as 'U'."""
        bridge, client = self._make_bridge_with_client()
        frame = ax25.Frame(
            dst=ax25.Address('APRS'),
            src=ax25.Address('W1ABC'),
            control=ax25.Control(ax25.FrameType.UI),
            pid=0xF0,
            data=b'Hello',
        )
        bridge.on_kiss_frame(b'\x00' + bytes(frame))
        client.send_frame.assert_called_once()
        args = client.send_frame.call_args[0]
        assert args[1] == ord('U')

    def test_ui_frame_payload_contains_cr_separated_monitor_text(self):
        """Monitor payload must be 'header\\rdata' — pe splits on \\r."""
        bridge, client = self._make_bridge_with_client()
        info = b'test data'
        frame = ax25.Frame(
            dst=ax25.Address('APRS'),
            src=ax25.Address('W1ABC'),
            control=ax25.Control(ax25.FrameType.UI),
            pid=0xF0,
            data=info,
        )
        bridge.on_kiss_frame(b'\x00' + bytes(frame))
        payload = client.send_frame.call_args[0][4]  # data arg
        assert b'\r' in payload
        header, data = payload.split(b'\r', 1)
        assert b'W1ABC' in header
        assert b'APRS' in header
        assert data == info

    def test_ui_frame_monitor_text_has_len_field(self):
        """Monitor header must contain 'Len=N ' so pe can extract data length."""
        bridge, client = self._make_bridge_with_client()
        info = b'hello world'
        frame = ax25.Frame(
            dst=ax25.Address('APRS'),
            src=ax25.Address('W1ABC'),
            control=ax25.Control(ax25.FrameType.UI),
            pid=0xF0,
            data=info,
        )
        bridge.on_kiss_frame(b'\x00' + bytes(frame))
        payload = client.send_frame.call_args[0][4]
        header = payload.split(b'\r', 1)[0].decode()
        m = re.search(r' Len=(\d+) ', header)
        assert m is not None
        assert int(m.group(1)) == len(info)

    def test_non_monitoring_client_does_not_receive_frame(self):
        """Frames must only go to clients that have monitoring enabled."""
        bridge, client = self._make_bridge_with_client()
        client.monitoring = False
        frame = ax25.Frame(
            dst=ax25.Address('APRS'),
            src=ax25.Address('W1ABC'),
            control=ax25.Control(ax25.FrameType.UI),
            pid=0xF0,
            data=b'test',
        )
        bridge.on_kiss_frame(b'\x00' + bytes(frame))
        client.send_frame.assert_not_called()

    def test_invalid_ax25_frame_does_not_crash(self):
        """Garbage from KISS must be logged and discarded without exception."""
        bridge, client = self._make_bridge_with_client()
        bridge.on_kiss_frame(b'\xff\xff\xff')  # must not raise


class TestBufferingAndReassembly:
    def test_partial_header_is_buffered(self):
        protocol, transport, bridge = make_protocol()
        partial = make_frame(0, ord('R'))[:20]
        protocol.data_received(partial)
        transport.write.assert_not_called()

    def test_partial_payload_is_buffered(self):
        protocol, transport, bridge = make_protocol()
        frame = make_frame(0, ord('M'), b'W1ABC', b'APRS', b'Hello World')
        protocol.data_received(frame[:AGWPE_HEADER_SIZE + 3])
        transport.write.assert_not_called()
        bridge.send_to_kiss.assert_not_called()

    def test_frame_reassembled_across_two_chunks(self):
        protocol, transport, bridge = make_protocol()
        frame = make_frame(0, ord('R'))
        half = len(frame) // 2
        protocol.data_received(frame[:half])
        transport.write.assert_not_called()
        protocol.data_received(frame[half:])
        transport.write.assert_called_once()

    def test_two_frames_in_one_chunk(self):
        protocol, transport, bridge = make_protocol()
        frame1 = make_frame(0, ord('R'))
        frame2 = make_frame(0, ord('G'))
        protocol.data_received(frame1 + frame2)
        assert transport.write.call_count == 2


# ---------------------------------------------------------------------------
# KISSClient
# ---------------------------------------------------------------------------

class TestKISSClient:
    @pytest.mark.asyncio
    async def test_connect_serial(self):
        config = configparser.ConfigParser()
        config['client'] = {'type': 'serial', 'device': '/dev/ttyUSB0', 'baudrate': '9600'}
        with patch('tncd.kiss.SerialKISS') as mock_kiss:
            mock_instance = MagicMock()
            mock_kiss.return_value = mock_instance
            client = KISSClient(config)
            await client.connect()
            mock_kiss.assert_called_once_with(port='/dev/ttyUSB0', speed=9600)
            mock_instance.start.assert_called_once()

    @pytest.mark.asyncio
    async def test_connect_tcp(self):
        config = configparser.ConfigParser()
        config['client'] = {'type': 'tcp', 'host': 'kiss.example.com', 'port': '8001'}
        with patch('tncd.kiss.TCPKISS') as mock_kiss:
            mock_instance = MagicMock()
            mock_kiss.return_value = mock_instance
            client = KISSClient(config)
            await client.connect()
            mock_kiss.assert_called_once_with(host='kiss.example.com', port=8001)
            mock_instance.start.assert_called_once()

    @pytest.mark.asyncio
    async def test_send_data(self):
        config = configparser.ConfigParser()
        config['client'] = {'type': 'serial', 'device': '/dev/ttyUSB0', 'baudrate': '9600'}
        with patch('tncd.kiss.SerialKISS') as mock_kiss:
            mock_instance = MagicMock()
            mock_kiss.return_value = mock_instance
            client = KISSClient(config)
            await client.connect()
            client.send(b'\x00\x01\x02')
            mock_instance.write.assert_called_once_with(b'\x00\x01\x02')

    @pytest.mark.asyncio
    async def test_close(self):
        config = configparser.ConfigParser()
        config['client'] = {'type': 'serial', 'device': '/dev/ttyUSB0', 'baudrate': '9600'}
        with patch('tncd.kiss.SerialKISS') as mock_kiss:
            mock_instance = MagicMock()
            mock_kiss.return_value = mock_instance
            client = KISSClient(config)
            await client.connect()
            client.close()
            mock_instance.stop.assert_called_once()
            assert client.connection is None

    def test_send_no_connection_is_noop(self):
        config = configparser.ConfigParser()
        config['client'] = {'type': 'serial', 'device': '/dev/ttyUSB0', 'baudrate': '9600'}
        client = KISSClient(config)
        client.send(b'\x00\x01\x02')  # must not raise

    def test_kiss_params_extracted(self):
        config = configparser.ConfigParser()
        config.read_string('[kiss]\ntx_delay = 50\npersistence = 64\n')
        client = KISSClient(config)
        params = client._get_kiss_params()
        assert params['TX_DELAY'] == 50
        assert params['PERSISTENCE'] == 64

    def test_kiss_params_empty_section(self):
        config = configparser.ConfigParser()
        config.add_section('kiss')
        client = KISSClient(config)
        assert client._get_kiss_params() == {}


# ---------------------------------------------------------------------------
# Bridge
# ---------------------------------------------------------------------------

class TestBridge:
    def _make_bridge(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000', 'callsign': 'MYCALL'}
        config['client'] = {'type': 'serial', 'device': '/dev/ttyUSB0', 'baudrate': '9600'}
        return Bridge(config)

    def test_init(self):
        bridge = self._make_bridge()
        assert bridge.kiss_client is not None
        assert bridge.clients == []
        assert bridge.callsign == 'MYCALL'

    def test_add_client(self):
        bridge = self._make_bridge()
        client = Mock()
        bridge.add_client(client)
        assert client in bridge.clients

    def test_remove_client(self):
        bridge = self._make_bridge()
        client = Mock()
        bridge.add_client(client)
        bridge.remove_client(client)
        assert client not in bridge.clients

    def test_remove_missing_client_is_noop(self):
        bridge = self._make_bridge()
        bridge.remove_client(Mock())  # must not raise

    def test_send_to_kiss(self):
        bridge = self._make_bridge()
        mock_conn = MagicMock()
        bridge.kiss_client.connection = mock_conn
        bridge.send_to_kiss(b'\x00\x01\x02')
        mock_conn.write.assert_called_once_with(b'\x00\x01\x02')


# ---------------------------------------------------------------------------
# Connected mode helpers
# ---------------------------------------------------------------------------

def make_real_protocol():
    """Return (protocol, transport, bridge) with a real Bridge and mocked KISS."""
    config = configparser.ConfigParser()
    config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000', 'callsign': 'N0CALL'}
    config['client'] = {'type': 'serial', 'device': '/dev/null', 'baudrate': '9600'}
    config['kiss']   = {'tx_delay': '40', 'persistence': '63', 'slot_time': '20', 'tx_tail': '30'}
    bridge = Bridge(config)
    bridge.kiss_client = Mock()
    protocol = AGWPEServerProtocol(bridge)
    transport = Mock()
    transport.is_closing.return_value = False
    protocol.connection_made(transport)
    return protocol, transport, bridge


# ---------------------------------------------------------------------------
# Connected mode: client→server TX path (C / c / v / D / d)
# ---------------------------------------------------------------------------

class TestConnectedMode:

    def test_C_sends_sabm(self):
        """'C' must send an AX.25 SABM frame to the TNC."""
        protocol, _, bridge = make_real_protocol()
        protocol.data_received(make_frame(0, ord('C'), b'W1ABC', b'W2DEF'))
        bridge.kiss_client.send.assert_called_once()
        frame = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert frame.control.frame_type is ax25.FrameType.SABM
        assert str(frame.dst) == 'W2DEF'
        assert str(frame.src) == 'W1ABC'

    def test_C_sets_state_connecting(self):
        protocol, _, bridge = make_real_protocol()
        protocol.data_received(make_frame(0, ord('C'), b'W1ABC', b'W2DEF'))
        conn = bridge.get_connection(0, 'W1ABC', 'W2DEF')
        assert conn is not None
        assert conn.state == 'CONNECTING'
        assert conn.owner is protocol

    def test_D_sends_iframe_when_connected(self):
        """'D' on an established connection must build an AX.25 I-frame."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'hello'))
        bridge.kiss_client.send.assert_called_once()
        frame = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert frame.control.frame_type is ax25.FrameType.I
        assert frame.data == b'hello'

    def test_D_increments_send_seqno(self):
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'pkt1'))
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'pkt2'))
        assert conn.send_seqno == 2

    def test_D_no_connection_does_not_forward(self):
        """'D' with no established connection must not send to KISS."""
        protocol, _, bridge = make_real_protocol()
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'hello'))
        bridge.kiss_client.send.assert_not_called()

    def test_D_fragments_large_payload(self):
        """Data > 256 bytes must be split into multiple I-frames."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'x' * 300))
        assert bridge.kiss_client.send.call_count == 2

    def test_D_window_limits_sends(self):
        """Must not send more than max_window I-frames without receiving ACKs."""
        protocol, _, bridge = make_real_protocol()
        max_win = bridge.max_window
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        # Send more D-frames than window allows — only max_window should be transmitted
        total = max_win + 3
        for i in range(total):
            protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', f'pkt{i}'.encode()))
        assert bridge.kiss_client.send.call_count == max_win
        assert conn.unacked == max_win
        assert len(conn.outbound_queue) == 3

    def test_D_window_drains_on_ack(self):
        """Queued frames must be sent when ACKs open the window."""
        protocol, _, bridge = make_real_protocol()
        max_win = bridge.max_window
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        # Fill window + queue 3 more (use 200-byte payloads to prevent coalescing)
        total = max_win + 3
        for i in range(total):
            protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', bytes(200)))
        assert bridge.kiss_client.send.call_count == max_win
        # Remote ACKs all with RR N(R)=max_win
        rr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                         control=ax25.Control(ax25.FrameType.RR, recv_seqno=max_win))
        bridge.on_kiss_frame(b'\x00' + bytes(rr))
        # The 3 queued frames should now be drained
        assert bridge.kiss_client.send.call_count == max_win + 3
        assert conn.unacked == 3
        assert len(conn.outbound_queue) == 0

    def test_D_retains_sent_frames_in_retransmit_buffer(self):
        """Sent I-frames must be retained in retransmit buffer for replay."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'hello'))
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'world'))
        assert len(conn.retransmit_buf) == 2
        assert 0 in conn.retransmit_buf
        assert 1 in conn.retransmit_buf

    def test_D_retransmit_buffer_purged_on_ack(self):
        """ACKs must purge retransmit buffer entries up to N(R)."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        for i in range(3):
            protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', f'pkt{i}'.encode()))
        assert len(conn.retransmit_buf) == 3
        # Remote ACKs first 2 with RR N(R)=2
        rr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                         control=ax25.Control(ax25.FrameType.RR, recv_seqno=2))
        bridge.on_kiss_frame(b'\x00' + bytes(rr))
        assert len(conn.retransmit_buf) == 1
        assert 2 in conn.retransmit_buf  # only the unacked one remains

    def test_rej_retransmits_from_requested_seqno(self):
        """REJ N(R)=X must retransmit all frames from N(S)=X onward."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        for i in range(3):
            protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', f'pkt{i}'.encode()))
        assert bridge.kiss_client.send.call_count == 3
        # Remote sends REJ N(R)=1 (frame 0 was lost, retransmit from 1)
        rej = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.REJ, recv_seqno=1))
        bridge.on_kiss_frame(b'\x00' + bytes(rej))
        # Frames with N(S)=1 and N(S)=2 should be retransmitted
        assert bridge.kiss_client.send.call_count == 3 + 2

    async def test_rr_poll_retransmits_unacked_frames(self):
        """RR with P=1 from remote must trigger retransmit of unacked I-frames."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        for i in range(3):
            protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', f'pkt{i}'.encode()))
        assert bridge.kiss_client.send.call_count == 3
        # Remote ACKs first frame only, then polls with RR P=1 N(R)=1
        # (simulating: remote got frame 0 but frames 1 and 2 were lost)
        rr_poll = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                             control=ax25.Control(ax25.FrameType.RR, recv_seqno=1,
                                                  poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(rr_poll))
        # Poll response is deferred via call_soon — yield to let it run.
        await asyncio.sleep(0)
        # Should send: RR F=1 response + retransmit of frames 1 and 2 = 3 more
        assert bridge.kiss_client.send.call_count == 3 + 3

    async def test_t1_timer_polls_then_retransmits(self):
        """First T1 expiry sends poll only; second adds retransmit."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'hello'))
        assert bridge.kiss_client.send.call_count == 1
        assert conn.t1_handle is not None
        # First T1 expiry: poll only (no retransmit)
        bridge._t1_expired(conn)
        assert bridge.kiss_client.send.call_count == 2
        assert conn.t1_polls == 1
        poll_bytes = bridge.kiss_client.send.call_args_list[1][0][0]
        poll_frame = ax25.Frame.unpack(poll_bytes)
        assert poll_frame.control.frame_type is ax25.FrameType.RR
        assert poll_frame.control.poll_final is True
        # Second T1 expiry: poll + retransmit
        bridge._t1_expired(conn)
        assert bridge.kiss_client.send.call_count == 4  # poll + 1 retransmit
        assert conn.t1_polls == 2

    async def test_t1_timer_cancelled_on_full_ack(self):
        """T1 timer must be cancelled when all frames are ACKed."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'hello'))
        assert conn.t1_handle is not None
        # ACK everything
        rr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                         control=ax25.Control(ax25.FrameType.RR, recv_seqno=1))
        bridge.on_kiss_frame(b'\x00' + bytes(rr))
        assert conn.t1_handle is None

    def test_d_sends_disc(self):
        """'d' must send AX.25 DISC to the TNC."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('d'), b'W1ABC', b'W2DEF'))
        bridge.kiss_client.send.assert_called_once()
        frame = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert frame.control.frame_type is ax25.FrameType.DISC

    def test_d_sets_state_disconnecting(self):
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.data_received(make_frame(0, ord('d'), b'W1ABC', b'W2DEF'))
        assert conn.state == 'DISCONNECTING'

    def test_c_sends_sabm(self):
        """'c' (connect with PID) must also send SABM."""
        protocol, _, bridge = make_real_protocol()
        protocol.data_received(make_frame(0, ord('c'), b'W1ABC', b'W2DEF'))
        bridge.kiss_client.send.assert_called_once()
        frame = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert frame.control.frame_type is ax25.FrameType.SABM

    def test_v_sends_sabm_with_via(self):
        """'v' (connect via) must send SABM with digipeater path."""
        protocol, _, bridge = make_real_protocol()
        via_payload = bytes([1]) + b'RELAY\x00\x00\x00\x00\x00'
        protocol.data_received(make_frame(0, ord('v'), b'W1ABC', b'W2DEF', via_payload))
        bridge.kiss_client.send.assert_called_once()
        frame = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert frame.control.frame_type is ax25.FrameType.SABM
        assert frame.via is not None and len(frame.via) == 1


# ---------------------------------------------------------------------------
# Connected mode: KISS→AGWPE RX path
# ---------------------------------------------------------------------------

class TestConnectedModeReceivePath:

    def _make_bridge(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000', 'callsign': 'N0CALL'}
        config['client'] = {'type': 'serial', 'device': '/dev/null', 'baudrate': '9600'}
        config['kiss']   = {'tx_delay': '40', 'persistence': '63', 'slot_time': '20', 'tx_tail': '30'}
        bridge = Bridge(config)
        bridge.kiss_client = Mock()
        return bridge

    def test_incoming_sabm_sends_ua(self):
        """Incoming SABM must be replied to with UA addressed to the caller."""
        bridge = self._make_bridge()
        sabm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(sabm))
        bridge.kiss_client.send.assert_called_once()
        ua = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert ua.control.frame_type is ax25.FrameType.UA
        assert str(ua.dst) == 'W2DEF'
        assert str(ua.src) == 'W1ABC'

    def test_incoming_sabm_notifies_clients_with_C(self):
        """Incoming SABM must send 'C' notification to all AGWPE clients."""
        bridge = self._make_bridge()
        client = Mock()
        bridge.add_client(client)
        sabm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(sabm))
        client.send_frame.assert_called_once()
        assert client.send_frame.call_args[0][1] == ord('C')

    def test_incoming_sabm_creates_connected_state(self):
        bridge = self._make_bridge()
        sabm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(sabm))
        conn = bridge.get_connection(0, 'W1ABC', 'W2DEF')
        assert conn is not None
        assert conn.state == 'CONNECTED'

    def test_ua_after_connecting_notifies_owner(self):
        """UA received while CONNECTING sends 'C' to owner, sets CONNECTED."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTING'
        conn.owner = owner
        ua = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                        control=ax25.Control(ax25.FrameType.UA, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(ua))
        owner.send_frame.assert_called_once()
        assert owner.send_frame.call_args[0][1] == ord('C')
        assert conn.state == 'CONNECTED'

    def test_ua_after_disconnecting_notifies_owner(self):
        """UA received while DISCONNECTING sends 'd' to owner, removes connection."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'DISCONNECTING'
        conn.owner = owner
        ua = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                        control=ax25.Control(ax25.FrameType.UA, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(ua))
        owner.send_frame.assert_called_once()
        assert owner.send_frame.call_args[0][1] == ord('d')
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None

    def test_dm_while_connecting_notifies_owner(self):
        """DM while CONNECTING sends 'd' (connect failed) to owner."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTING'
        conn.owner = owner
        dm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                        control=ax25.Control(ax25.FrameType.DM, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(dm))
        owner.send_frame.assert_called_once()
        assert owner.send_frame.call_args[0][1] == ord('d')
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None

    def test_dm_while_connected_notifies_owner(self):
        """DM while CONNECTED (forced disconnect) sends 'd' to owner."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        dm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                        control=ax25.Control(ax25.FrameType.DM, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(dm))
        owner.send_frame.assert_called_once()
        assert owner.send_frame.call_args[0][1] == ord('d')

    def test_received_iframe_sends_rr(self):
        """Polled I-frame (P=1) must be immediately acknowledged with RR."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        iframe = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                            control=ax25.Control(ax25.FrameType.I, send_seqno=0,
                                                 recv_seqno=0, poll_final=True),
                            pid=0xF0, data=b'hello')
        bridge.on_kiss_frame(b'\x00' + bytes(iframe))
        sent_frames = [ax25.Frame.unpack(c[0][0]) for c in bridge.kiss_client.send.call_args_list]
        rr_frames = [f for f in sent_frames if f.control.frame_type is ax25.FrameType.RR]
        assert len(rr_frames) == 1
        assert rr_frames[0].control.recv_seqno == 1  # N(R) = N(S)+1 = 1

    def test_received_iframe_nonpoll_sends_rr(self):
        """Non-polled I-frame (P=0) must still be acknowledged with RR."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        iframe = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                            control=ax25.Control(ax25.FrameType.I, send_seqno=0,
                                                 recv_seqno=0, poll_final=False),
                            pid=0xF0, data=b'hello')
        bridge.on_kiss_frame(b'\x00' + bytes(iframe))
        sent_frames = [ax25.Frame.unpack(c[0][0]) for c in bridge.kiss_client.send.call_args_list]
        rr_frames = [f for f in sent_frames if f.control.frame_type is ax25.FrameType.RR]
        assert len(rr_frames) == 1
        assert rr_frames[0].control.recv_seqno == 1
        assert rr_frames[0].control.poll_final is False  # P/F must echo the I-frame

    def test_received_iframe_window_sends_rr_for_each(self):
        """A window of P=0 I-frames must each get an RR, advancing N(R)."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        for ns in range(4):
            iframe = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                                control=ax25.Control(ax25.FrameType.I, send_seqno=ns,
                                                     recv_seqno=0, poll_final=False),
                                pid=0xF0, data=f'data{ns}'.encode())
            bridge.on_kiss_frame(b'\x00' + bytes(iframe))
        sent_frames = [ax25.Frame.unpack(c[0][0]) for c in bridge.kiss_client.send.call_args_list]
        rr_frames = [f for f in sent_frames if f.control.frame_type is ax25.FrameType.RR]
        assert len(rr_frames) == 4
        for i, rr in enumerate(rr_frames):
            assert rr.control.recv_seqno == (i + 1) % 8

    def test_received_iframe_delivers_data_to_owner(self):
        """Received I-frame must deliver data to connection owner as 'D'."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        iframe = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                            control=ax25.Control(ax25.FrameType.I, send_seqno=0, recv_seqno=0),
                            pid=0xF0, data=b'hello')
        bridge.on_kiss_frame(b'\x00' + bytes(iframe))
        owner.send_frame.assert_called_once()
        args = owner.send_frame.call_args[0]
        assert args[1] == ord('D')
        assert args[4] == b'hello'

    def test_remote_disc_sends_ua(self):
        """Remote DISC must be answered with UA."""
        bridge = self._make_bridge()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        disc = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.DISC, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(disc))
        bridge.kiss_client.send.assert_called_once()
        ua = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert ua.control.frame_type is ax25.FrameType.UA

    def test_remote_disc_notifies_owner_and_removes_connection(self):
        """Remote DISC must notify owner with 'd' and clean up connection state."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        disc = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.DISC, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(disc))
        owner.send_frame.assert_called_once()
        assert owner.send_frame.call_args[0][1] == ord('d')
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None


    def test_sabme_rejected_with_dm(self):
        """SABME (extended mode) must be rejected with DM, not accepted."""
        bridge = self._make_bridge()
        sabme = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                           control=ax25.Control(ax25.FrameType.SABME, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(sabme))
        bridge.kiss_client.send.assert_called_once()
        dm = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert dm.control.frame_type is ax25.FrameType.DM
        # No connection should be created
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None

    def test_incoming_sabm_sets_owner_for_registered_client(self):
        """Incoming SABM must set conn.owner to the client that registered the callsign."""
        bridge = self._make_bridge()
        protocol = AGWPEServerProtocol(bridge)
        protocol.registered_calls = {'W1ABC'}
        protocol.send_frame = Mock()
        bridge.add_client(protocol)
        sabm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(sabm))
        conn = bridge.get_connection(0, 'W1ABC', 'W2DEF')
        assert conn is not None
        assert conn.owner is protocol

    def test_incoming_sabm_message_text(self):
        """Incoming SABM 'C' notification must use 'CONNECTED To' (not 'With')."""
        bridge = self._make_bridge()
        client = Mock()
        client.registered_calls = set()
        bridge.add_client(client)
        sabm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
        bridge.on_kiss_frame(b'\x00' + bytes(sabm))
        msg = client.send_frame.call_args[0][4]
        assert b'CONNECTED To' in msg


class TestRNRHandling:
    """AX.25 6.4.9: Receive Not Ready (RNR) flow control."""

    def test_rnr_stops_iframe_sending(self):
        """RNR from remote must set remote_busy and prevent I-frame sending."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        assert not conn.remote_busy

        # Receive RNR from remote
        rnr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                         control=ax25.Control(ax25.FrameType.RNR, recv_seqno=0))
        bridge.on_kiss_frame(b'\x00' + bytes(rnr))
        assert conn.remote_busy

        # Queue data — should NOT be sent while remote is busy
        bridge.kiss_client.send.reset_mock()
        protocol.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'hello'))
        # The data is queued but _drain_outbound returns early due to remote_busy
        assert len(conn.outbound_queue) > 0 or conn.unacked == 0
        # No I-frame should have been sent (only the RR poll response may have been sent)
        for call in bridge.kiss_client.send.call_args_list:
            frame = ax25.Frame.unpack(call[0][0])
            assert frame.control.frame_type is not ax25.FrameType.I

    def test_rr_clears_remote_busy(self):
        """RR from remote must clear remote_busy and drain queued frames."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol

        # Set remote_busy
        conn.remote_busy = True
        # Queue some data
        conn.outbound_queue.append((b'hello', 0xF0))

        # Receive RR from remote (clears busy)
        rr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                        control=ax25.Control(ax25.FrameType.RR, recv_seqno=0))
        bridge.on_kiss_frame(b'\x00' + bytes(rr))
        assert not conn.remote_busy
        # Queued frame should have been drained
        assert len(conn.outbound_queue) == 0

    def test_rej_clears_remote_busy(self):
        """REJ must clear remote_busy (AX.25 6.4.9)."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        conn.remote_busy = True

        rej = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                         control=ax25.Control(ax25.FrameType.REJ, recv_seqno=0))
        bridge.on_kiss_frame(b'\x00' + bytes(rej))
        assert not conn.remote_busy


class TestN2RetryLimit:
    """AX.25 6.3.2: N2 retry limit — disconnect after too many unanswered polls."""

    async def test_n2_disconnect_after_exceeded(self):
        """After N2+1 T1 expiries, connection must be torn down."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        # Put a valid AX.25 I-frame in retransmit buffer
        iframe = ax25.Frame(dst=ax25.Address('W2DEF'), src=ax25.Address('W1ABC'),
                            control=ax25.Control(ax25.FrameType.I, send_seqno=0,
                                                 recv_seqno=0),
                            data=b'test')
        conn.retransmit_buf[0] = bytes(iframe)

        # Set t1_polls so after _t1_expired increments it, it exceeds n2_retry
        conn.t1_polls = bridge.n2_retry
        bridge._t1_expired(conn)

        # Connection should be removed
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None

    async def test_n2_does_not_disconnect_before_limit(self):
        """At exactly N2 polls, connection must still be alive."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        iframe = ax25.Frame(dst=ax25.Address('W2DEF'), src=ax25.Address('W1ABC'),
                            control=ax25.Control(ax25.FrameType.I, send_seqno=0,
                                                 recv_seqno=0),
                            data=b'test')
        conn.retransmit_buf[0] = bytes(iframe)

        # _t1_expired increments t1_polls before checking, so set to n2_retry - 1
        # so after increment it will be exactly n2_retry (not exceeding)
        conn.t1_polls = bridge.n2_retry - 1
        bridge._t1_expired(conn)

        # Should still exist (n2_retry == n2_retry is not >)
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is not None
        assert conn.state == 'CONNECTED'

    async def test_n2_sends_disconnect_notification(self):
        """N2 exceeded must send 'd' disconnect notification to AGWPE client."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        protocol.send_frame = Mock()
        iframe = ax25.Frame(dst=ax25.Address('W2DEF'), src=ax25.Address('W1ABC'),
                            control=ax25.Control(ax25.FrameType.I, send_seqno=0,
                                                 recv_seqno=0),
                            data=b'test')
        conn.retransmit_buf[0] = bytes(iframe)
        conn.t1_polls = bridge.n2_retry

        bridge._t1_expired(conn)

        # Should have sent a 'd' frame with disconnect message
        protocol.send_frame.assert_called()
        args = protocol.send_frame.call_args[0]
        assert args[1] == ord('d')
        assert b'DISCONNECTED' in args[4]

    def test_n2_default_value(self):
        """DEFAULT_N2_RETRY must be 10 per AX.25 recommended default."""
        assert DEFAULT_N2_RETRY == 10


class TestFRMRHandling:
    """AX.25 2.4.5: Frame Reject (FRMR) — unrecoverable protocol error."""

    def test_frmr_resets_connection(self):
        """FRMR must reset connection state and send SABM."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol
        conn.send_seqno = 5
        conn.recv_seqno = 3
        conn.unacked = 2
        conn.retransmit_buf = {3: b'x', 4: b'y'}
        conn.outbound_queue.append((b'data', 0xF0))

        frmr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.FRMR))
        bridge.on_kiss_frame(b'\x00' + bytes(frmr))

        # Connection should be reset to CONNECTING
        conn = bridge.get_connection(0, 'W1ABC', 'W2DEF')
        assert conn.state == 'CONNECTING'
        assert conn.send_seqno == 0
        assert conn.recv_seqno == 0
        assert conn.unacked == 0
        assert len(conn.retransmit_buf) == 0
        assert len(conn.outbound_queue) == 0

    def test_frmr_sends_sabm(self):
        """FRMR must trigger a SABM to re-establish the link."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = protocol

        bridge.kiss_client.send.reset_mock()
        frmr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.FRMR))
        bridge.on_kiss_frame(b'\x00' + bytes(frmr))

        bridge.kiss_client.send.assert_called()
        sent = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert sent.control.frame_type is ax25.FrameType.SABM

    def test_frmr_ignored_when_not_connected(self):
        """FRMR on a non-CONNECTED session must be ignored."""
        protocol, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTING'

        bridge.kiss_client.send.reset_mock()
        frmr = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.FRMR))
        bridge.on_kiss_frame(b'\x00' + bytes(frmr))

        # Should not send SABM since not in CONNECTED state
        for call in bridge.kiss_client.send.call_args_list:
            frame = ax25.Frame.unpack(call[0][0])
            assert frame.control.frame_type is not ax25.FrameType.SABM


class TestBluetoothKISS:
    """Test BluetoothKISS over a socketpair (no real Bluetooth needed)."""

    def test_start_creates_protocol(self):
        s1, s2 = socket.socketpair()
        try:
            bt = BluetoothKISS(s1)
            bt.start()
            assert bt.protocol is not None
            assert bt.protocol.transport is not None
            bt.stop()
        finally:
            s2.close()

    def test_write_sends_kiss_framed_data(self):
        s1, s2 = socket.socketpair()
        try:
            bt = BluetoothKISS(s1)
            bt.start()
            test_data = b'\x01\x02\x03'
            bt.write(test_data)
            # Give the event loop a chance to flush the write
            import time
            time.sleep(0.05)
            received = s2.recv(1024)
            assert received[0:1] == b'\xc0'   # FEND
            assert received[-1:] == b'\xc0'    # FEND
            bt.stop()
        finally:
            s2.close()

    def test_stop_closes_transport(self):
        s1, s2 = socket.socketpair()
        try:
            bt = BluetoothKISS(s1)
            bt.start()
            transport = bt.protocol.transport
            bt.stop()
            assert transport.is_closing()
        finally:
            s2.close()


class TestBluetoothConfig:
    """Test Bluetooth config parsing in KISSClient.connect()."""

    async def test_bluetooth_missing_bdaddr_raises(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'ota_baudrate': '1200'}
        config['kiss'] = {}
        client = KISSClient(config)
        with pytest.raises(Exception):
            await client.connect()

    async def test_bluetooth_missing_dbus_raises_runtime_error(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        client = KISSClient(config)
        with patch.dict('sys.modules', {'dbus': None, 'dbus.service': None,
                                         'dbus.mainloop': None,
                                         'dbus.mainloop.glib': None}):
            with pytest.raises(RuntimeError, match='Bluetooth support requires'):
                await client.connect()

    async def test_bluetooth_channel_optional_defaults_none(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        assert config.get('client', 'channel', fallback=None) is None

    async def test_bluetooth_reconnect_defaults(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        assert config.getboolean('client', 'reconnect', fallback=True) is True
        assert config.getfloat('client', 'reconnect_delay', fallback=5.0) == 5.0
        assert config.getfloat('client', 'reconnect_max_delay', fallback=60.0) == 60.0


class TestBluetoothConnect:
    async def test_connect_calls_register_and_connect_profile(self):
        """Verify D-Bus wiring: profile registered, ConnectProfile called."""
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        client = KISSClient(config)

        mock_dbus = MagicMock()
        mock_glib = MagicMock()
        mock_bus = MagicMock()
        mock_dbus.SystemBus.return_value = mock_bus

        # Provide a base class that accepts (bus, path) args
        class FakeDbusObject:
            def __init__(self, bus, path):
                pass
        mock_dbus.service.Object = FakeDbusObject
        mock_dbus.service.method = lambda *a, **kw: lambda f: f  # no-op decorator
        mock_dbus.Dictionary = dict
        mock_dbus.String = str

        loop = asyncio.get_running_loop()

        # _bluetooth_connect will block on fd_future; timeout verifies
        # that all D-Bus setup calls happen before the await.
        with pytest.raises((asyncio.TimeoutError, Exception)):
            await asyncio.wait_for(
                client._bluetooth_connect(
                    mock_dbus, mock_glib, 'AA:BB:CC:DD:EE:FF', None, loop),
                timeout=0.5)

        # Verify D-Bus interactions happened in the right order
        mock_bus.get_object.assert_any_call('org.bluez', '/org/bluez')
        mock_bus.get_object.assert_any_call(
            'org.bluez',
            '/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF')
        # Properties.Get should have been called to check Connected status
        mock_dbus.Interface.assert_any_call(
            mock_bus.get_object.return_value,
            'org.freedesktop.DBus.Properties')


class TestBluetoothFullFlow:
    def test_bluetooth_send_and_receive(self):
        """End-to-end: BluetoothKISS over socketpair sends KISS-framed data."""
        s1, s2 = socket.socketpair()
        try:
            bt = BluetoothKISS(s1)
            bt.start()

            # Send a frame
            test_frame = b'\x01\x02\x03\x04'
            bt.write(test_frame)

            # Small delay for async transport to flush
            import time
            time.sleep(0.05)

            raw = s2.recv(1024)
            assert raw[0:1] == b'\xc0'   # FEND
            assert raw[-1:] == b'\xc0'   # FEND

            bt.stop()
        finally:
            s2.close()

    def test_bluetooth_connection_lost_hook(self):
        """connection_lost hook fires when socket closes."""
        s1, s2 = socket.socketpair()
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        client = KISSClient(config)
        client.connection = BluetoothKISS(s1)
        client.connection.start()

        # Install the connection_lost hook (simulating what connect() does)
        hook_called = []
        client.connection.protocol._on_connection_lost = lambda exc: hook_called.append(exc)

        # Trigger connection loss
        client.connection.protocol.connection_lost(Exception("test"))
        assert len(hook_called) == 1
        s2.close()


class TestBluetoothReconnect:
    async def test_connection_lost_triggers_reconnect(self):
        """When bluetooth socket closes and reconnect=true, reconnect loop starts."""
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200', 'reconnect': 'true',
                            'reconnect_delay': '0.1', 'reconnect_max_delay': '0.2'}
        config['kiss'] = {}
        client = KISSClient(config)
        client._bt_reconnect = True
        client._bt_reconnect_delay = 0.1
        client._bt_reconnect_max_delay = 0.2

        reconnect_called = asyncio.Event()

        async def mock_reconnect():
            reconnect_called.set()

        client._bt_reconnect_loop = mock_reconnect
        client._on_bt_connection_lost(Exception("connection lost"))

        await asyncio.wait_for(reconnect_called.wait(), timeout=1.0)
        assert reconnect_called.is_set()

    async def test_reconnect_disabled_does_not_reconnect(self):
        """When reconnect=false, connection loss does not trigger reconnect."""
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200', 'reconnect': 'false'}
        config['kiss'] = {}
        client = KISSClient(config)
        client._bt_reconnect = False

        reconnect_called = False

        async def mock_reconnect():
            nonlocal reconnect_called
            reconnect_called = True

        client._bt_reconnect_loop = mock_reconnect
        client._on_bt_connection_lost(Exception("connection lost"))
        await asyncio.sleep(0.2)
        assert not reconnect_called

    async def test_reconnect_loop_exponential_backoff(self):
        """Reconnect delay doubles on each failure up to max."""
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        client = KISSClient(config)
        client._bt_reconnect = True
        client._bt_reconnect_delay = 0.05
        client._bt_reconnect_max_delay = 0.15
        client._bt_dbus = MagicMock()
        client._bt_glib = MagicMock()
        client._bt_bdaddr = 'AA:BB:CC:DD:EE:FF'
        client._bt_channel = None

        attempts = []

        async def mock_bt_connect(dbus_mod, GLib, bdaddr, channel, loop):
            attempts.append(1)
            if len(attempts) < 3:
                raise ConnectionError("not ready")
            # On 3rd attempt, return a real socket
            s1, s2 = socket.socketpair()
            client._test_s2 = s2  # keep reference to close later
            return s1

        client._bluetooth_connect = mock_bt_connect
        client.start_receive = MagicMock()  # don't actually start RX thread

        await asyncio.wait_for(client._bt_reconnect_loop(), timeout=2.0)
        assert len(attempts) == 3
        client.connection.stop()
        client._test_s2.close()


# ---------------------------------------------------------------------------
# Multi-port config
# ---------------------------------------------------------------------------

def _write_ini(content):
    """Write content to a NamedTemporaryFile, return path. Caller must unlink."""
    f = tempfile.NamedTemporaryFile(mode='w', suffix='.ini', delete=False)
    f.write(content)
    f.close()
    return f.name


class TestMultiPortConfig:
    def test_numbered_sections_parsed(self):
        path = _write_ini("""
[server]
listen_port = 8000

[client.0]
type = serial
device = /dev/ttyUSB0

[client.1]
type = tcp
host = 192.168.1.10
port = 8001
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            cfg = load_config(args)
            assert cfg.port_count == 2
            p0 = cfg.port_config(0)
            assert p0['type'] == 'serial'
            assert p0['device'] == '/dev/ttyUSB0'
            p1 = cfg.port_config(1)
            assert p1['type'] == 'tcp'
            assert p1['host'] == '192.168.1.10'
        finally:
            os.unlink(path)

    def test_bare_client_section_treated_as_port0_with_warning(self):
        path = _write_ini("""
[client]
type = serial
device = /dev/ttyUSB0
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            with patch('tncd.logger') as mock_logger:
                cfg = load_config(args)
                mock_logger.warning.assert_called()
                warning_msg = mock_logger.warning.call_args[0][0]
                assert '[client]' in warning_msg
            assert cfg.port_count == 1
            assert cfg.port_config(0)['type'] == 'serial'
        finally:
            os.unlink(path)

    def test_kiss_n_sections_provide_per_port_kiss_params(self):
        path = _write_ini("""
[client.0]
type = serial
device = /dev/ttyUSB0

[kiss.0]
tx_delay = 55
persistence = 50

[client.1]
type = tcp
host = 10.0.0.1
port = 8100
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            cfg = load_config(args)
            k0 = cfg.kiss_config(0)
            assert k0['tx_delay'] == '55'
            assert k0['persistence'] == '50'
            # Port 1 has no kiss section
            assert cfg.kiss_config(1) == {}
        finally:
            os.unlink(path)

    def test_port_name_configured(self):
        path = _write_ini("""
[client.0]
type = serial
device = /dev/ttyUSB0
name = VHF Modem
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            cfg = load_config(args)
            assert cfg.port_name(0) == 'VHF Modem'
        finally:
            os.unlink(path)

    def test_port_name_default(self):
        path = _write_ini("""
[client.0]
type = serial
device = /dev/ttyUSB0
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            cfg = load_config(args)
            assert cfg.port_name(0) == 'Port 0'
        finally:
            os.unlink(path)

    def test_non_contiguous_ports_exits(self):
        path = _write_ini("""
[client.0]
type = serial
device = /dev/ttyUSB0

[client.2]
type = serial
device = /dev/ttyUSB1
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            with pytest.raises(SystemExit):
                load_config(args)
        finally:
            os.unlink(path)

    def test_no_client_sections_exits(self):
        """A config file with [client] sections that have no valid int suffix
        leaves port_sections empty after migration, triggering sys.exit(1).
        We simulate this by writing a config with a [client.notanumber] section
        which load_config cannot interpret as a port index."""
        path = _write_ini("""
[server]
listen_port = 8000

[client.notanumber]
type = serial
device = /dev/ttyUSB0
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            with pytest.raises(SystemExit):
                load_config(args)
        finally:
            os.unlink(path)

    def test_invalid_type_exits(self):
        """Unknown type value exits with error."""
        path = _write_ini("""
[client.0]
type = foobar
device = /dev/ttyUSB0
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            with pytest.raises(SystemExit):
                load_config(args)
        finally:
            os.unlink(path)

    def test_missing_type_exits(self):
        """Section with no type field exits with error."""
        path = _write_ini("""
[client.0]
device = /dev/ttyUSB0
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            with pytest.raises(SystemExit):
                load_config(args)
        finally:
            os.unlink(path)

    def test_backward_compat_client_access(self):
        """config['client'] and config.get('client', ...) map to port 0."""
        path = _write_ini("""
[client.0]
type = tcp
host = 10.0.0.1
port = 8001
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            cfg = load_config(args)
            assert cfg.get('client', 'type') == 'tcp'
            assert cfg['client']['host'] == '10.0.0.1'
        finally:
            os.unlink(path)

    def test_backward_compat_kiss_access(self):
        """config['kiss'] and config.get('kiss', ...) map to kiss.0."""
        path = _write_ini("""
[client.0]
type = serial
device = /dev/ttyUSB0

[kiss.0]
tx_delay = 77
""")
        try:
            args = argparse.Namespace(
                config=path, listen_host=None, listen_port=None, callsign=None,
                kiss_type=None, kiss_device=None, kiss_host=None,
                kiss_port=None, baudrate=None,
            )
            cfg = load_config(args)
            assert cfg.get('kiss', 'tx_delay') == '77'
            assert cfg['kiss']['tx_delay'] == '77'
        finally:
            os.unlink(path)


if __name__ == '__main__':
    pytest.main([__file__, '-v'])
