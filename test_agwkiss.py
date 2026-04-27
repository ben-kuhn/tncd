import pytest
import argparse
import ax25
import configparser
import re
import struct
from unittest.mock import Mock, MagicMock, patch

from agwkiss import (
    AGWPEServerProtocol, Bridge, Connection, KISSClient,
    AGWPE_HEADER_FORMAT, AGWPE_HEADER_SIZE, load_config,
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
        assert cfg.getint('client', 'baudrate') == 19200


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
        bridge.on_kiss_frame(bytes(frame))
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
        bridge.on_kiss_frame(bytes(frame))
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
        bridge.on_kiss_frame(bytes(frame))
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
        bridge.on_kiss_frame(bytes(frame))
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
        with patch('agwkiss.kiss.SerialKISS') as mock_kiss:
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
        with patch('agwkiss.kiss.TCPKISS') as mock_kiss:
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
        with patch('agwkiss.kiss.SerialKISS') as mock_kiss:
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
        with patch('agwkiss.kiss.SerialKISS') as mock_kiss:
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
        bridge.on_kiss_frame(bytes(sabm))
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
        bridge.on_kiss_frame(bytes(sabm))
        client.send_frame.assert_called_once()
        assert client.send_frame.call_args[0][1] == ord('C')

    def test_incoming_sabm_creates_connected_state(self):
        bridge = self._make_bridge()
        sabm = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                          control=ax25.Control(ax25.FrameType.SABM, poll_final=True))
        bridge.on_kiss_frame(bytes(sabm))
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
        bridge.on_kiss_frame(bytes(ua))
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
        bridge.on_kiss_frame(bytes(ua))
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
        bridge.on_kiss_frame(bytes(dm))
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
        bridge.on_kiss_frame(bytes(dm))
        owner.send_frame.assert_called_once()
        assert owner.send_frame.call_args[0][1] == ord('d')

    def test_received_iframe_sends_rr(self):
        """Received I-frame must be acknowledged with RR."""
        bridge = self._make_bridge()
        owner = Mock()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        iframe = ax25.Frame(dst=ax25.Address('W1ABC'), src=ax25.Address('W2DEF'),
                            control=ax25.Control(ax25.FrameType.I, send_seqno=0, recv_seqno=0),
                            pid=0xF0, data=b'hello')
        bridge.on_kiss_frame(bytes(iframe))
        sent_frames = [ax25.Frame.unpack(c[0][0]) for c in bridge.kiss_client.send.call_args_list]
        rr_frames = [f for f in sent_frames if f.control.frame_type is ax25.FrameType.RR]
        assert len(rr_frames) == 1
        assert rr_frames[0].control.recv_seqno == 1  # N(R) = N(S)+1 = 1

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
        bridge.on_kiss_frame(bytes(iframe))
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
        bridge.on_kiss_frame(bytes(disc))
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
        bridge.on_kiss_frame(bytes(disc))
        owner.send_frame.assert_called_once()
        assert owner.send_frame.call_args[0][1] == ord('d')
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None


if __name__ == '__main__':
    pytest.main([__file__, '-v'])
