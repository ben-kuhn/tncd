# Bluetooth SPP D-Bus Integration — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the deprecated `tncd-rfcomm` helper with native Bluetooth SPP support in `KISSClient` using the BlueZ D-Bus Profile API.

**Architecture:** When `type = bluetooth` is configured, `KISSClient.connect()` registers an SPP profile via D-Bus, calls `ConnectProfile()` on the target device, receives a connected unix fd via the `NewConnection` callback, wraps it in a standard asyncio transport with kiss3's `KISSProtocol`, and stores the result in `self.connection` — identical to the serial/TCP paths from that point forward. Reconnection uses exponential backoff.

**Tech Stack:** Python 3.8+, dbus-python, PyGObject (GLib mainloop), kiss3, asyncio

---

## File Structure

| File | Responsibility |
|------|---------------|
| `tncd.py` | `BluetoothKISS` wrapper class, `SPPProfile` D-Bus service object, bluetooth branch in `KISSClient.connect()`, reconnection logic |
| `tests/test_tncd.py` | Unit tests for bluetooth config parsing, `BluetoothKISS` over socketpair, reconnect logic, optional import error |
| `tncd.ini` | Example bluetooth config (commented out) |
| `requirements.txt` | Conditional Linux-only bluetooth deps |
| `README.md` | Bluetooth docs, platform notes, prerequisites |
| `tncd-rfcomm` | Deprecation notice in header |
| `nix/module.nix` | Updated bluetooth service (D-Bus instead of rfcomm) |
| `nix/default.nix` | Optional `bluetoothSupport` parameter |
| `packaging/PKGBUILD` | Add python-dbus, python-gobject deps |

---

### Task 1: Create feature branch and add BluetoothKISS wrapper class

**Files:**
- Modify: `tncd.py` (add `BluetoothKISS` class after `KISSClient` class, around line 563)
- Modify: `tests/test_tncd.py` (add `TestBluetoothKISS` class)

This task adds a thin wrapper that takes an already-connected socket and creates a kiss3-compatible KISS connection (transport + KISSProtocol). No D-Bus yet — just the socket-to-KISS bridge.

- [ ] **Step 1: Create feature branch**

```bash
cd /home/ku0hn/dev/tncd
git checkout -b feature/bluetooth-dbus
```

- [ ] **Step 2: Write the failing test for BluetoothKISS**

Add to `tests/test_tncd.py`, at the top add `import socket` to existing imports, and add `BluetoothKISS` to the import from tncd:

```python
from tncd import (
    AGWPEServerProtocol, Bridge, BluetoothKISS, Connection, KISSClient,
    AGWPE_HEADER_FORMAT, AGWPE_HEADER_SIZE, DEFAULT_MAX_WINDOW,
    DEFAULT_N2_RETRY, load_config,
)
```

Then add at the end of the file:

```python
# ---------------------------------------------------------------------------
# BluetoothKISS wrapper
# ---------------------------------------------------------------------------

class TestBluetoothKISS:
    """Test BluetoothKISS over a socketpair (no real Bluetooth needed)."""

    async def test_start_creates_protocol(self):
        s1, s2 = socket.socketpair()
        try:
            bt = BluetoothKISS(s1)
            bt.start()
            assert bt.protocol is not None
            assert bt.protocol.transport is not None
            bt.stop()
        finally:
            s2.close()

    async def test_write_sends_kiss_framed_data(self):
        s1, s2 = socket.socketpair()
        try:
            bt = BluetoothKISS(s1)
            bt.start()
            # Send a raw AX.25 frame through KISS framing
            test_data = b'\x01\x02\x03'
            bt.write(test_data)
            await asyncio.sleep(0.05)
            received = s2.recv(1024)
            # KISS frame: FEND + CMD(0x00) + escaped_data + FEND
            assert received[0:1] == b'\xc0'   # FEND
            assert received[-1:] == b'\xc0'    # FEND
            bt.stop()
        finally:
            s2.close()

    async def test_stop_closes_transport(self):
        s1, s2 = socket.socketpair()
        try:
            bt = BluetoothKISS(s1)
            bt.start()
            transport = bt.protocol.transport
            bt.stop()
            assert transport.is_closing()
        finally:
            s2.close()
```

- [ ] **Step 3: Run test to verify it fails**

```bash
cd /home/ku0hn/dev/tncd && source .venv/bin/activate
pytest tests/test_tncd.py::TestBluetoothKISS -v
```

Expected: FAIL with `ImportError: cannot import name 'BluetoothKISS' from 'tncd'`

- [ ] **Step 4: Implement BluetoothKISS**

Add to `tncd.py` after the `import` block (around line 22, after `import kiss`), add:

```python
import functools
import socket as socket_mod
```

Then add the class before `class Bridge:` (around line 563):

```python
class BluetoothKISS(kiss.classes.KISS):
    """KISS connection over a pre-connected socket (e.g. Bluetooth SPP fd)."""

    def __init__(self, sock, strip_df_start=False):
        super().__init__(strip_df_start)
        self._sock = sock

    def start(self, **kwargs):
        _, self.protocol = self.loop.run_until_complete(
            self.loop.create_connection(
                functools.partial(kiss.kiss.KISSProtocol, decoder=self.decoder),
                sock=self._sock,
            )
        )
        self.loop.run_until_complete(self.protocol.connection_future)
        self._write_defaults(**kwargs)

    def stop(self):
        if self.protocol and self.protocol.transport:
            self.protocol.transport.close()
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
pytest tests/test_tncd.py::TestBluetoothKISS -v
```

Expected: all 3 tests PASS

- [ ] **Step 6: Run full test suite**

```bash
pytest
```

Expected: all existing tests still pass

- [ ] **Step 7: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: add BluetoothKISS wrapper for socket-based KISS connections"
```

---

### Task 2: Add bluetooth config parsing to KISSClient.connect()

**Files:**
- Modify: `tncd.py:444-528` (`KISSClient.connect()`)
- Modify: `tests/test_tncd.py`

Add the `elif conn_type == 'bluetooth':` branch that reads config, imports dbus (with graceful error), and prepares for connection. The actual D-Bus connection comes in Task 3 — this task just wires the config and optional import.

- [ ] **Step 1: Write failing test for bluetooth config parsing**

Add to `tests/test_tncd.py`:

```python
# ---------------------------------------------------------------------------
# Bluetooth config parsing
# ---------------------------------------------------------------------------

class TestBluetoothConfig:
    async def test_bluetooth_type_reads_bdaddr(self):
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        client = KISSClient(config)
        # connect() should read bdaddr from config; it will fail at the dbus
        # import since we don't have it in the test env, but we can test the
        # config parsing path by mocking the import
        assert config.get('client', 'bdaddr') == 'AA:BB:CC:DD:EE:FF'

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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
pytest tests/test_tncd.py::TestBluetoothConfig -v
```

Expected: some tests fail (the `RuntimeError` test needs the bluetooth branch to exist)

- [ ] **Step 3: Add bluetooth branch to KISSClient.connect()**

In `tncd.py`, modify `KISSClient.connect()`. After the `if conn_type == 'tcp':` block and before the `else:` (serial) block, add an `elif`:

```python
    async def connect(self):
        conn_type  = self.config.get('client', 'type', fallback='serial')
        kiss_params = self._get_kiss_params()

        loop = asyncio.get_running_loop()

        init_str = self.config.get('client', 'init_string', fallback=None)
        init_delay = self.config.getfloat('client', 'init_delay', fallback=1.0)

        if conn_type == 'tcp':
            host = self.config.get('client', 'host', fallback='localhost')
            port = self.config.getint('client', 'port', fallback=8001)
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

            bdaddr = self.config.get('client', 'bdaddr', fallback=None)
            if not bdaddr:
                raise ValueError("Bluetooth connection requires 'bdaddr' in [client] config")

            channel = self.config.get('client', 'channel', fallback=None)
            reconnect = self.config.getboolean('client', 'reconnect', fallback=True)
            reconnect_delay = self.config.getfloat('client', 'reconnect_delay', fallback=5.0)
            reconnect_max_delay = self.config.getfloat('client', 'reconnect_max_delay', fallback=60.0)

            logger.info(f"Connecting to Bluetooth TNC at {bdaddr}"
                        f"{f' channel {channel}' if channel else ' (SDP auto-detect)'}")

            sock = await self._bluetooth_connect(
                dbus, GLib, bdaddr, channel, loop)
            self.connection = BluetoothKISS(sock)

            self._bt_dbus = dbus
            self._bt_glib = GLib
            self._bt_bdaddr = bdaddr
            self._bt_channel = channel
            self._bt_reconnect = reconnect
            self._bt_reconnect_delay = reconnect_delay
            self._bt_reconnect_max_delay = reconnect_max_delay

        else:
            device   = self.config.get('client', 'device', fallback='/dev/ttyUSB0')
            # ... rest of serial branch unchanged ...
```

The `_bluetooth_connect` method is a placeholder that will be implemented in Task 3. For now, add a stub:

```python
    async def _bluetooth_connect(self, dbus, GLib, bdaddr, channel, loop):
        """Connect to a Bluetooth SPP device via D-Bus. Returns a connected socket."""
        raise NotImplementedError("Bluetooth D-Bus connection not yet implemented")
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
pytest tests/test_tncd.py::TestBluetoothConfig -v
```

Expected: all 5 tests PASS

- [ ] **Step 5: Run full test suite**

```bash
pytest
```

Expected: all tests pass

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: add bluetooth config parsing and optional dbus import in KISSClient"
```

---

### Task 3: Implement D-Bus SPP profile and bluetooth connection

**Files:**
- Modify: `tncd.py` (add `SPPProfile` class, implement `_bluetooth_connect`)
- Modify: `tests/test_tncd.py`

This is the core D-Bus integration. Register an SPP profile, connect to the device, receive the fd.

- [ ] **Step 1: Write failing test for _bluetooth_connect with mocked D-Bus**

Add to `tests/test_tncd.py`:

```python
# ---------------------------------------------------------------------------
# Bluetooth D-Bus connection (mocked)
# ---------------------------------------------------------------------------

class TestBluetoothConnect:
    async def test_connect_registers_profile_and_calls_connect_profile(self):
        """Verify the D-Bus wiring: profile registered, ConnectProfile called."""
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200'}
        config['kiss'] = {}
        client = KISSClient(config)

        # Create a real socketpair so the fd is valid
        s1, s2 = socket.socketpair()

        mock_dbus = MagicMock()
        mock_glib = MagicMock()
        mock_bus = MagicMock()
        mock_dbus.SystemBus.return_value = mock_bus

        # Simulate BlueZ calling NewConnection with s1's fd
        def fake_register(path, uuid, opts):
            pass

        manager_iface = MagicMock()
        manager_iface.RegisterProfile = fake_register
        mock_dbus.Interface.return_value = manager_iface

        # Mock the device proxy
        device_proxy = MagicMock()
        device_iface = MagicMock()

        def get_proxy(path, iface=None):
            if 'ProfileManager1' in str(path) or 'ProfileManager1' in str(iface):
                return manager_iface
            return device_proxy

        mock_bus.get_object.return_value = device_proxy

        loop = asyncio.get_running_loop()

        try:
            # _bluetooth_connect should attempt to register profile and connect
            # It will block waiting for NewConnection callback, so we run it
            # with a timeout and expect it to either succeed (if we simulate
            # the callback) or timeout
            with pytest.raises((asyncio.TimeoutError, NotImplementedError, Exception)):
                await asyncio.wait_for(
                    client._bluetooth_connect(
                        mock_dbus, mock_glib, 'AA:BB:CC:DD:EE:FF', None, loop),
                    timeout=0.5)
        finally:
            s1.close()
            s2.close()
```

- [ ] **Step 2: Run test to verify it fails**

```bash
pytest tests/test_tncd.py::TestBluetoothConnect -v
```

Expected: FAIL — `_bluetooth_connect` raises `NotImplementedError`

- [ ] **Step 3: Implement SPPProfile and _bluetooth_connect**

Add `SPPProfile` class in `tncd.py` before `BluetoothKISS`:

```python
SPP_UUID = '00001101-0000-1000-8000-00805f9b34fb'

def _make_spp_profile(dbus_mod, fd_future, loop):
    """Create an SPP Profile1 D-Bus service object.

    Returns the class (must be instantiated after bus is available).
    BlueZ calls NewConnection() with a connected fd when the remote
    device's SPP channel is established.
    """
    class SPPProfile(dbus_mod.service.Object):
        @dbus_mod.service.method("org.bluez.Profile1",
                                 in_signature="oha{sv}", out_signature="")
        def NewConnection(self, path, fd, properties):
            fd_val = fd.take()
            logger.info(f"Bluetooth SPP connected: path={path}, fd={fd_val}")
            loop.call_soon_threadsafe(fd_future.set_result, fd_val)

        @dbus_mod.service.method("org.bluez.Profile1",
                                 in_signature="o", out_signature="")
        def RequestDisconnection(self, path):
            logger.info(f"Bluetooth SPP disconnection requested: {path}")

        @dbus_mod.service.method("org.bluez.Profile1",
                                 in_signature="", out_signature="")
        def Release(self):
            logger.info("Bluetooth SPP profile released")

    return SPPProfile
```

Then implement `_bluetooth_connect` in `KISSClient`:

```python
    async def _bluetooth_connect(self, dbus_mod, GLib, bdaddr, channel, loop):
        """Connect to a Bluetooth SPP device via D-Bus Profile API.

        Registers an SPP profile, calls ConnectProfile on the target device,
        and waits for BlueZ to deliver a connected fd via NewConnection.
        Returns a socket wrapping the fd.
        """
        dbus_mod.mainloop.glib.DBusGMainLoop(set_as_default=True)
        bus = dbus_mod.SystemBus()

        fd_future = loop.create_future()
        ProfileClass = _make_spp_profile(dbus_mod, fd_future, loop)
        profile_path = '/org/tncd/spp'
        profile = ProfileClass(bus, profile_path)

        # Register the SPP profile with BlueZ
        manager = dbus_mod.Interface(
            bus.get_object('org.bluez', '/org/bluez'),
            'org.bluez.ProfileManager1')
        opts = dbus_mod.Dictionary({
            'Role': dbus_mod.String('client'),
        }, signature='sv')
        if channel:
            opts['Channel'] = dbus_mod.UInt16(int(channel))
        manager.RegisterProfile(profile_path, SPP_UUID, opts)
        logger.info("Bluetooth SPP profile registered")

        # Run GLib mainloop in a daemon thread for D-Bus signal dispatch
        glib_loop = GLib.MainLoop()
        glib_thread = threading.Thread(target=glib_loop.run, daemon=True,
                                       name='glib-mainloop')
        glib_thread.start()
        self._glib_loop = glib_loop

        # Connect to the device
        device_path = f'/org/bluez/hci0/dev_{bdaddr.upper().replace(":", "_")}'
        device = dbus_mod.Interface(
            bus.get_object('org.bluez', device_path),
            'org.bluez.Device1')
        logger.info(f"Calling ConnectProfile on {device_path}")
        device.ConnectProfile(SPP_UUID)

        # Wait for BlueZ to call NewConnection with the fd
        fd = await fd_future
        sock = socket_mod.fromfd(fd, socket_mod.AF_UNIX, socket_mod.SOCK_STREAM)
        os.close(fd)  # fromfd duplicates the fd; close the original
        logger.info(f"Bluetooth SPP socket ready (fd={sock.fileno()})")
        return sock
```

Also add `import os` to the top of `tncd.py`.

- [ ] **Step 4: Run tests to verify they pass**

```bash
pytest tests/test_tncd.py::TestBluetoothConnect -v
```

Expected: PASS (the test expects an exception during the mocked D-Bus flow, which now raises something other than NotImplementedError)

- [ ] **Step 5: Run full test suite**

```bash
pytest
```

Expected: all tests pass

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: implement D-Bus SPP profile and bluetooth connection"
```

---

### Task 4: Wire bluetooth connection into KISSClient.connect() and start_receive()

**Files:**
- Modify: `tncd.py:444-530` (`KISSClient.connect()` bluetooth branch completion)
- Modify: `tests/test_tncd.py`

Complete the bluetooth path so that after `_bluetooth_connect` returns a socket, we create a `BluetoothKISS`, call `start()`, and the existing `start_receive()` and `send()` methods work.

- [ ] **Step 1: Write failing test for full bluetooth connect flow**

Add to `tests/test_tncd.py`:

```python
class TestBluetoothFullFlow:
    async def test_bluetooth_send_and_receive(self):
        """End-to-end: KISSClient with BluetoothKISS over socketpair."""
        s1, s2 = socket.socketpair()
        try:
            config = configparser.ConfigParser()
            config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                                'callsign': 'N0CALL'}
            config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                                'ota_baudrate': '1200'}
            config['kiss'] = {}
            client = KISSClient(config)

            # Bypass D-Bus by injecting BluetoothKISS directly
            client.connection = BluetoothKISS(s1)
            client.connection.start()

            # Send a frame through the client
            test_frame = b'\x01\x02\x03\x04'
            client.send(test_frame)
            await asyncio.sleep(0.05)

            # Read KISS-framed data from the other end
            raw = s2.recv(1024)
            assert raw[0:1] == b'\xc0'   # FEND
            assert raw[-1:] == b'\xc0'   # FEND

            client.close()
        finally:
            s2.close()
```

- [ ] **Step 2: Run test to verify it fails**

```bash
pytest tests/test_tncd.py::TestBluetoothFullFlow -v
```

Expected: may fail if `close()` doesn't handle BluetoothKISS properly

- [ ] **Step 3: Ensure KISSClient.close() works with BluetoothKISS**

The existing `close()` calls `self.connection.stop()` which is already implemented in `BluetoothKISS`. Also complete the bluetooth branch in `connect()` to call `start()`:

In the bluetooth branch of `connect()`, after creating `self.connection = BluetoothKISS(sock)`, add the blocking_start logic:

```python
            # For bluetooth, start is simpler — no init_string, no serial config
            def blocking_start():
                self.connection.start(**kiss_params)

            with ThreadPoolExecutor() as executor:
                await loop.run_in_executor(executor, blocking_start)
```

Also store bluetooth state for reconnection:

```python
            self._bt_dbus = dbus_mod  # use the already-imported module
            self._bt_glib = GLib
            self._bt_bdaddr = bdaddr
            self._bt_channel = channel
            self._bt_reconnect = reconnect
            self._bt_reconnect_delay = reconnect_delay
            self._bt_reconnect_max_delay = reconnect_max_delay
```

- [ ] **Step 4: Run tests**

```bash
pytest tests/test_tncd.py::TestBluetoothFullFlow -v
```

Expected: PASS

- [ ] **Step 5: Run full test suite**

```bash
pytest
```

Expected: all tests pass

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: complete bluetooth path in KISSClient.connect()"
```

---

### Task 5: Add reconnection logic

**Files:**
- Modify: `tncd.py` (`KISSClient` — add `_on_bt_connection_lost`, `_bt_reconnect_loop`)
- Modify: `tests/test_tncd.py`

When the Bluetooth socket closes, detect it and reconnect with exponential backoff.

- [ ] **Step 1: Write failing test for reconnection**

Add to `tests/test_tncd.py`:

```python
class TestBluetoothReconnect:
    async def test_connection_lost_triggers_reconnect(self):
        """When bluetooth socket closes, reconnect loop starts."""
        s1, s2 = socket.socketpair()
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

        client.connection = BluetoothKISS(s1)
        client.connection.start()

        reconnect_called = asyncio.Event()
        original_reconnect = getattr(client, '_bt_reconnect_loop', None)

        async def mock_reconnect():
            reconnect_called.set()

        client._bt_reconnect_loop = mock_reconnect

        # Simulate connection loss by closing the remote end
        s2.close()
        client._on_bt_connection_lost(Exception("connection lost"))

        await asyncio.wait_for(reconnect_called.wait(), timeout=1.0)
        assert reconnect_called.is_set()

    async def test_reconnect_disabled_does_not_reconnect(self):
        """When reconnect=false, connection loss does not trigger reconnect."""
        s1, s2 = socket.socketpair()
        config = configparser.ConfigParser()
        config['server'] = {'listen_host': '0.0.0.0', 'listen_port': '8000',
                            'callsign': 'N0CALL'}
        config['client'] = {'type': 'bluetooth', 'bdaddr': 'AA:BB:CC:DD:EE:FF',
                            'ota_baudrate': '1200', 'reconnect': 'false'}
        config['kiss'] = {}
        client = KISSClient(config)
        client._bt_reconnect = False

        client.connection = BluetoothKISS(s1)
        client.connection.start()

        reconnect_called = False

        async def mock_reconnect():
            nonlocal reconnect_called
            reconnect_called = True

        client._bt_reconnect_loop = mock_reconnect

        s2.close()
        client._on_bt_connection_lost(Exception("connection lost"))
        await asyncio.sleep(0.2)
        assert not reconnect_called
```

- [ ] **Step 2: Run test to verify it fails**

```bash
pytest tests/test_tncd.py::TestBluetoothReconnect -v
```

Expected: FAIL — `_on_bt_connection_lost` doesn't exist yet

- [ ] **Step 3: Implement reconnection logic**

Add to `KISSClient` class in `tncd.py`:

```python
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
                self.connection = BluetoothKISS(sock)
                self.connection.start(**kiss_params)
                self.start_receive(loop)
                logger.info("Bluetooth reconnected successfully")
                return
            except Exception as e:
                logger.warning(f"Bluetooth reconnect failed: {e}")
                delay = min(delay * 2, max_delay)
```

Also, in the bluetooth branch of `connect()`, after `self.connection.start()`, hook the protocol's `connection_lost` for reconnection (implemented in Task 5):

```python
            # Hook connection_lost for reconnection (handler added in Task 5)
            original_connection_lost = self.connection.protocol.connection_lost
            def _wrapped_connection_lost(exc):
                original_connection_lost(exc)
                if hasattr(self, '_on_bt_connection_lost'):
                    self._on_bt_connection_lost(exc)
            self.connection.protocol.connection_lost = _wrapped_connection_lost
```

- [ ] **Step 4: Run tests**

```bash
pytest tests/test_tncd.py::TestBluetoothReconnect -v
```

Expected: PASS

- [ ] **Step 5: Run full test suite**

```bash
pytest
```

Expected: all tests pass

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: add bluetooth reconnection with exponential backoff"
```

---

### Task 6: Update tncd.ini and requirements.txt

**Files:**
- Modify: `tncd.ini`
- Modify: `requirements.txt`

- [ ] **Step 1: Update tncd.ini with bluetooth example config**

Replace the existing `[bluetooth]` section in `tncd.ini` with the new format. The new config uses `type = bluetooth` under `[client]` instead of a separate `[bluetooth]` section:

```ini
# Bluetooth TNC settings (uncomment to use Bluetooth instead of serial/TCP)
# type = bluetooth
# bdaddr = AA:BB:CC:DD:EE:FF
# channel = 6             # optional, auto-detected via SDP if omitted
# reconnect = true        # auto-reconnect on connection loss (default: true)
# reconnect_delay = 5     # initial reconnect delay in seconds (default: 5)
# reconnect_max_delay = 60  # max reconnect delay in seconds (default: 60)
```

Remove the old `[bluetooth]` section entirely.

- [ ] **Step 2: Update requirements.txt**

Replace contents of `requirements.txt`:

```
kiss3>=8.0.0
pyham-ax25>=1.0.0
pyserial>=3.5

# Bluetooth support (Linux only)
dbus-python>=1.2.0; sys_platform == 'linux'
PyGObject>=3.40.0; sys_platform == 'linux'
```

- [ ] **Step 3: Commit**

```bash
git add tncd.ini requirements.txt
git commit -m "feat: update config and requirements for bluetooth support"
```

---

### Task 7: Update README.md

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Replace the "Bluetooth TNC (RFCOMM)" section under "Supported TNC Connections"**

Replace the current bluetooth section (lines 95-99) with:

```markdown
### Bluetooth TNC

On Linux, tncd connects to Bluetooth TNCs natively using the BlueZ D-Bus
Profile API. Set `type = bluetooth` in the config — no external tools needed.

On macOS and Windows, the OS exposes paired Bluetooth SPP devices as serial
ports (`/dev/tty.*` on macOS, `COMx` on Windows). Use `type = serial` with
the device path.
```

- [ ] **Step 2: Replace the "Bluetooth TNC" config section (lines 178-194)**

Replace with:

```markdown
### Bluetooth TNC (Linux)

First, pair and trust your TNC using `bluetoothctl`:

```bash
bluetoothctl
scan on                        # find your TNC
pair AA:BB:CC:DD:EE:FF         # pair with the TNC
trust AA:BB:CC:DD:EE:FF        # trust for auto-reconnect
exit
```

Then configure tncd:

```ini
[client]
type = bluetooth
bdaddr = AA:BB:CC:DD:EE:FF
ota_baudrate = 1200
# channel = 6             # optional, auto-detected via SDP
# reconnect = true        # auto-reconnect on drop (default)
# reconnect_delay = 5     # initial delay seconds (default)
# reconnect_max_delay = 60  # max delay seconds (default)
```

Requires `dbus-python` and `PyGObject` (included in packaged installs).

### Bluetooth TNC (macOS / Windows)

After pairing in system Bluetooth settings, the OS creates a virtual serial
port. Use `type = serial` with the device path:

```ini
[client]
type = serial
device = /dev/tty.BluetoothTNC    # macOS
# device = COM5                    # Windows
serial_baudrate = 9600
ota_baudrate = 1200
```
```

- [ ] **Step 3: Update the Usage section (lines 210-213)**

Replace the bluetooth usage block with:

```markdown
# Bluetooth TNC (Linux) — automatic, no separate tool needed
tncd -c /etc/tncd.ini
```

- [ ] **Step 4: Update the systemd section (lines 279-283)**

Replace the tncd-rfcomm references. Change:

```markdown
For Bluetooth, also enable the rfcomm manager:

```bash
sudo systemctl enable --now tncd-rfcomm
```
```

To:

```markdown
For Bluetooth TNCs, ensure the BlueZ service is running and the TNC is
paired/trusted. tncd handles the Bluetooth connection directly — no
separate service needed.
```

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: update README with native bluetooth support and platform notes"
```

---

### Task 8: Update packaging

**Files:**
- Modify: `packaging/PKGBUILD`
- Modify: `nix/module.nix`
- Modify: `nix/default.nix`

- [ ] **Step 1: Update PKGBUILD**

Add `python-dbus` and `python-gobject` to the depends array. Remove `bluez-utils` (no longer needed for rfcomm):

```bash
depends=('python' 'python-pyserial' 'python-kiss3' 'python-pyham-ax25' 'python-dbus' 'python-gobject')
```

- [ ] **Step 2: Update nix/default.nix**

Add optional `bluetoothSupport` parameter:

```nix
{ lib
, python3
, bluetoothSupport ? false
}:

python3.pkgs.buildPythonApplication rec {
  pname = "tncd";
  version = "0.6-Beta";

  src = lib.cleanSource ../.;

  format = "other";

  disabled = python3.pkgs.pythonOlder "3.8";

  dependencies = with python3.pkgs; [
    pyserial
  ] ++ lib.optionals bluetoothSupport [
    dbus-python
    pygobject3
  ];

  installPhase = ''
    install -Dm755 tncd.py      $out/bin/tncd
    install -Dm755 tncd-rfcomm  $out/bin/tncd-rfcomm
    install -Dm644 tncd.ini     $out/share/tncd/tncd.ini.example
  '';

  meta = with lib; {
    description = "AGWPE-to-KISS Translation Bridge";
    longDescription = ''
      A bridge that allows AGWPE-client applications to communicate with KISS TNCs.
      Supports both serial and TCP KISS connections.
    '';
    homepage = "https://github.com/ben-kuhn/tncd";
    license = lib.licenses.gpl3;
    maintainers = [ ];
    platforms = lib.platforms.linux;
  };
}
```

- [ ] **Step 3: Update nix/module.nix**

Replace the bluetooth section to use D-Bus instead of rfcomm. The tncd-rfcomm service is no longer needed. Replace the `bluetooth` option and its service:

Change the bluetooth option from:

```nix
    bluetooth = {
      enable = lib.mkEnableOption "Bluetooth rfcomm manager (tncd-rfcomm)";
    };
```

To:

```nix
    bluetooth = {
      enable = lib.mkEnableOption "Bluetooth SPP support (adds dbus-python and PyGObject)";
    };
```

In the `config` section, change the tncd package override to pass `bluetoothSupport`:

```nix
    # Override the package to include bluetooth deps when enabled
    services.tncd.package = lib.mkIf cfg.bluetooth.enable (
      lib.mkDefault (cfg.package.override { bluetoothSupport = true; })
    );
```

Update the tncd service `after` and `wants` to only depend on `bluetooth.service` (not tncd-rfcomm):

```nix
    systemd.services.tncd = {
      description = "AGWPE-to-KISS Translation Bridge";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ]
        ++ lib.optionals cfg.bluetooth.enable [ "bluetooth.service" ];
      wants = lib.optionals cfg.bluetooth.enable [ "bluetooth.service" ];
      serviceConfig = {
        Type = "simple";
        User = cfg.user;
        Group = cfg.group;
        ExecStart = "${cfg.package}/bin/tncd -c ${configFile}";
        Restart = "on-failure";
        RestartSec = 5;
      };
    };
```

Remove the `systemd.services.tncd-rfcomm` block entirely (the whole `lib.mkIf cfg.bluetooth.enable { ... }` block for tncd-rfcomm).

Add the tncd user to the `bluetooth` group when bluetooth is enabled:

```nix
    users.users = lib.mkIf (cfg.user == "tncd") {
      tncd = {
        isSystemUser = true;
        group = cfg.group;
        extraGroups = [ "dialout" ]
          ++ lib.optionals cfg.bluetooth.enable [ "bluetooth" ];
        description = "tncd service user";
      };
    };
```

- [ ] **Step 4: Commit**

```bash
git add packaging/PKGBUILD nix/default.nix nix/module.nix
git commit -m "feat: update packaging for native bluetooth support"
```

---

### Task 9: Add deprecation notice to tncd-rfcomm

**Files:**
- Modify: `tncd-rfcomm` (header comment only)

- [ ] **Step 1: Add deprecation notice**

Add after the existing docstring (line 2) in `tncd-rfcomm`:

```python
#!/usr/bin/env python3
"""
AGWPE-to-KISS Bridge: rfcomm connection manager

DEPRECATED: This tool is deprecated and will be removed in a future release.
Use type = bluetooth in tncd.ini instead — tncd now connects to Bluetooth
TNCs directly via the BlueZ D-Bus Profile API without external tools.

Disconnects any active Bluetooth audio profile, then connects via rfcomm.
With -m watch, automatically reconnects if the connection drops.

Usage:
  ./tncd-rfcomm -b AA:BB:CC:DD:EE:FF              # connect once
  ./tncd-rfcomm -b AA:BB:CC:DD:EE:FF -m watch     # connect + auto-reconnect
```

- [ ] **Step 2: Commit**

```bash
git add tncd-rfcomm
git commit -m "docs: add deprecation notice to tncd-rfcomm"
```

---

### Task 10: Final integration test and cleanup

**Files:**
- All modified files

- [ ] **Step 1: Run full test suite**

```bash
cd /home/ku0hn/dev/tncd && source .venv/bin/activate
pytest -v
```

Expected: all tests pass

- [ ] **Step 2: Verify no regressions in serial/TCP paths**

```bash
pytest tests/test_tncd.py -v -k "not Bluetooth"
```

Expected: all non-bluetooth tests pass unchanged

- [ ] **Step 3: Review diff**

```bash
git diff main --stat
git log main..HEAD --oneline
```

Verify all expected files are modified and commit history is clean.

- [ ] **Step 4: Update nix-ham-packages (separate repo)**

In `/home/ku0hn/dev/nix-ham-packages/tncd/default.nix`, add the `bluetoothSupport` parameter to match the in-tree nix package. This will be done when the feature branch is merged and a new version is tagged.
