# Multi-Port / Multi-Modem Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow tncd to manage multiple KISS TNC connections simultaneously, each mapped to a different AGWPE port number.

**Architecture:** Each port gets its own `KISSClient` instance with independent connection state, AX.25 sessions, KISS parameters, and reconnect logic. The `Bridge` class holds a list of `KISSClient` instances indexed by port number. The AGWPE header's port byte routes frames to the correct client. Config uses `[client.N]` / `[kiss.N]` sections with backward-compatible bare `[client]` / `[kiss]` support.

**Tech Stack:** Python 3, asyncio, configparser, kiss3, pyham-ax25

---

### Task 1: Multi-Port Config Parsing

**Files:**
- Modify: `tncd.py:1542-1586` (`load_config` function)
- Test: `tests/test_tncd.py` (new `TestMultiPortConfig` class)

- [ ] **Step 1: Write failing tests for multi-port config parsing**

```python
class TestMultiPortConfig:
    """Tests for multi-port configuration parsing."""

    def test_numbered_client_sections(self):
        """[client.0] and [client.1] are parsed into port list."""
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type=None, kiss_device=None, kiss_host=None, kiss_port=None,
            baudrate=None, ota_baudrate=None)
        import tempfile, os
        ini = tempfile.NamedTemporaryFile(mode='w', suffix='.ini', delete=False)
        ini.write("[server]\ncallsign = TEST\n\n")
        ini.write("[client.0]\ntype = tcp\nhost = 127.0.0.1\nport = 8001\nota_baudrate = 1200\n\n")
        ini.write("[client.1]\ntype = serial\ndevice = /dev/ttyUSB0\nserial_baudrate = 57600\nota_baudrate = 1200\n\n")
        ini.close()
        args.config = ini.name
        config = load_config(args)
        try:
            assert config.port_count == 2
            assert config.port_config(0)['type'] == 'tcp'
            assert config.port_config(1)['type'] == 'serial'
            assert config.port_config(1)['device'] == '/dev/ttyUSB0'
        finally:
            os.unlink(ini.name)

    def test_bare_client_treated_as_port_0(self):
        """Legacy [client] section maps to port 0 with deprecation warning."""
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type=None, kiss_device=None, kiss_host=None, kiss_port=None,
            baudrate=None, ota_baudrate=None)
        import tempfile, os
        ini = tempfile.NamedTemporaryFile(mode='w', suffix='.ini', delete=False)
        ini.write("[server]\ncallsign = TEST\n\n")
        ini.write("[client]\ntype = serial\ndevice = /dev/ttyUSB0\n\n")
        ini.close()
        args.config = ini.name
        import logging
        with patch('tncd.logger') as mock_logger:
            config = load_config(args)
        try:
            assert config.port_count == 1
            assert config.port_config(0)['type'] == 'serial'
            mock_logger.warning.assert_called()
        finally:
            os.unlink(ini.name)

    def test_kiss_sections_per_port(self):
        """[kiss.N] sections provide per-port KISS params."""
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type=None, kiss_device=None, kiss_host=None, kiss_port=None,
            baudrate=None, ota_baudrate=None)
        import tempfile, os
        ini = tempfile.NamedTemporaryFile(mode='w', suffix='.ini', delete=False)
        ini.write("[server]\ncallsign = TEST\n\n")
        ini.write("[client.0]\ntype = tcp\nhost = 127.0.0.1\nport = 8001\nota_baudrate = 1200\n\n")
        ini.write("[client.1]\ntype = tcp\nhost = 127.0.0.1\nport = 8002\nota_baudrate = 1200\n\n")
        ini.write("[kiss.1]\ntx_delay = 80\npersistence = 32\n\n")
        ini.close()
        args.config = ini.name
        config = load_config(args)
        try:
            assert config.kiss_config(0) == {}  # defaults
            assert config.kiss_config(1)['tx_delay'] == '80'
            assert config.kiss_config(1)['persistence'] == '32'
        finally:
            os.unlink(ini.name)

    def test_port_names(self):
        """Port name comes from config, defaults to 'Port N'."""
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type=None, kiss_device=None, kiss_host=None, kiss_port=None,
            baudrate=None, ota_baudrate=None)
        import tempfile, os
        ini = tempfile.NamedTemporaryFile(mode='w', suffix='.ini', delete=False)
        ini.write("[server]\ncallsign = TEST\n\n")
        ini.write("[client.0]\ntype = tcp\nhost = 127.0.0.1\nport = 8001\nname = My TNC\nota_baudrate = 1200\n\n")
        ini.write("[client.1]\ntype = tcp\nhost = 127.0.0.1\nport = 8002\nota_baudrate = 1200\n\n")
        ini.close()
        args.config = ini.name
        config = load_config(args)
        try:
            assert config.port_name(0) == 'My TNC'
            assert config.port_name(1) == 'Port 1'
        finally:
            os.unlink(ini.name)

    def test_noncontiguous_ports_error(self):
        """Port numbers must be contiguous from 0."""
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type=None, kiss_device=None, kiss_host=None, kiss_port=None,
            baudrate=None, ota_baudrate=None)
        import tempfile, os
        ini = tempfile.NamedTemporaryFile(mode='w', suffix='.ini', delete=False)
        ini.write("[server]\ncallsign = TEST\n\n")
        ini.write("[client.0]\ntype = tcp\nhost = 127.0.0.1\nport = 8001\nota_baudrate = 1200\n\n")
        ini.write("[client.2]\ntype = tcp\nhost = 127.0.0.1\nport = 8002\nota_baudrate = 1200\n\n")
        ini.close()
        args.config = ini.name
        with pytest.raises(SystemExit):
            load_config(args)
        os.unlink(ini.name)

    def test_no_client_sections_error(self):
        """At least one [client.N] section must exist."""
        args = argparse.Namespace(
            config=None, listen_host=None, listen_port=None, callsign=None,
            kiss_type=None, kiss_device=None, kiss_host=None, kiss_port=None,
            baudrate=None, ota_baudrate=None)
        import tempfile, os
        ini = tempfile.NamedTemporaryFile(mode='w', suffix='.ini', delete=False)
        ini.write("[server]\ncallsign = TEST\n\n")
        ini.close()
        args.config = ini.name
        with pytest.raises(SystemExit):
            load_config(args)
        os.unlink(ini.name)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestMultiPortConfig -v`
Expected: FAIL — `load_config` returns a plain ConfigParser without `port_count`, `port_config`, etc.

- [ ] **Step 3: Implement PortConfig wrapper and update load_config**

Create a `PortConfig` class that wraps the raw ConfigParser and provides multi-port access. Update `load_config` to detect numbered sections, validate, and return a `PortConfig`.

```python
class PortConfig:
    """Multi-port configuration wrapper around ConfigParser."""

    def __init__(self, raw_config, port_sections, kiss_sections):
        self._raw = raw_config
        self._ports = port_sections    # list of section names, indexed by port number
        self._kiss = kiss_sections     # dict: port_number -> section name (or None)

    @property
    def port_count(self):
        return len(self._ports)

    def port_config(self, port_num):
        """Return dict of config values for a port."""
        section = self._ports[port_num]
        return dict(self._raw.items(section))

    def kiss_config(self, port_num):
        """Return dict of KISS config values for a port (empty dict if no section)."""
        section = self._kiss.get(port_num)
        if section is None:
            return {}
        return dict(self._raw.items(section))

    def port_name(self, port_num):
        """Return the human-readable name for a port."""
        section = self._ports[port_num]
        return self._raw.get(section, 'name', fallback=f'Port {port_num}')

    # Delegate server/ax25 access to raw config
    def get(self, section, key, **kwargs):
        return self._raw.get(section, key, **kwargs)

    def getint(self, section, key, **kwargs):
        return self._raw.getint(section, key, **kwargs)

    def getfloat(self, section, key, **kwargs):
        return self._raw.getfloat(section, key, **kwargs)

    def getboolean(self, section, key, **kwargs):
        return self._raw.getboolean(section, key, **kwargs)

    def has_option(self, section, key):
        return self._raw.has_option(section, key)

    def has_section(self, section):
        return self._raw.has_section(section)

    def __getitem__(self, key):
        return self._raw[key]

    def __contains__(self, key):
        return key in self._raw
```

Update `load_config` to:
1. Read the INI file
2. Detect `[client.N]` sections vs bare `[client]`
3. If bare `[client]` found, log deprecation warning, treat as `[client.0]`
4. Same for `[kiss]` → `[kiss.0]`
5. Validate contiguous port numbering, at least one port, required fields per type
6. Return `PortConfig`

```python
def load_config(args):
    raw = configparser.ConfigParser()
    # Read defaults for server
    raw.add_section("server")
    raw["server"]["listen_host"] = "0.0.0.0"
    raw["server"]["listen_port"] = "8000"
    raw["server"]["callsign"]    = "AGWPE"

    if args.config:
        config_file = Path(args.config)
        if config_file.exists():
            raw.read(config_file)
            logger.info(f"Loaded config from {args.config}")

    # CLI overrides for server
    if args.listen_host:
        raw["server"]["listen_host"] = args.listen_host
    if args.listen_port:
        raw["server"]["listen_port"] = str(args.listen_port)
    if args.callsign:
        raw["server"]["callsign"] = args.callsign

    # Detect port sections
    port_sections = []  # ordered list of section names
    kiss_sections = {}  # port_num -> section name

    # Check for bare [client] (legacy)
    has_bare_client = raw.has_section('client')
    numbered = sorted(
        [s for s in raw.sections() if s.startswith('client.') and s[7:].isdigit()],
        key=lambda s: int(s[7:])
    )

    if numbered:
        port_sections = numbered
    elif has_bare_client:
        logger.warning(
            "Deprecated: bare [client] section detected. "
            "Please migrate to [client.0] format."
        )
        # Rename internally
        if not raw.has_section('client.0'):
            raw.add_section('client.0')
            for key, val in raw.items('client'):
                if key != '__name__':
                    raw.set('client.0', key, val)
        port_sections = ['client.0']
    else:
        logger.error("No [client.N] sections found in config")
        sys.exit(1)

    # Validate contiguous numbering
    for i, section in enumerate(port_sections):
        expected = f'client.{i}'
        if section != expected:
            logger.error(
                f"Port numbers must be contiguous from 0. "
                f"Expected [client.{i}], found [{section}]"
            )
            sys.exit(1)

    # Validate required fields per port
    for i, section in enumerate(port_sections):
        conn_type = raw.get(section, 'type', fallback=None)
        if not conn_type:
            logger.error(f"[{section}] missing required 'type' field")
            sys.exit(1)
        if conn_type == 'bluetooth' and not raw.get(section, 'bdaddr', fallback=None):
            logger.error(f"[{section}] type=bluetooth requires 'bdaddr'")
            sys.exit(1)
        if conn_type == 'serial' and not raw.get(section, 'device', fallback=None):
            logger.error(f"[{section}] type=serial requires 'device'")
            sys.exit(1)
        if conn_type == 'tcp':
            if not raw.get(section, 'host', fallback=None):
                logger.error(f"[{section}] type=tcp requires 'host'")
                sys.exit(1)
            if not raw.get(section, 'port', fallback=None):
                logger.error(f"[{section}] type=tcp requires 'port'")
                sys.exit(1)

    # Detect kiss sections
    has_bare_kiss = raw.has_section('kiss')
    for s in raw.sections():
        if s.startswith('kiss.') and s[5:].isdigit():
            kiss_sections[int(s[5:])] = s
    if has_bare_kiss and 0 not in kiss_sections:
        logger.warning(
            "Deprecated: bare [kiss] section detected. "
            "Please migrate to [kiss.0] format."
        )
        if not raw.has_section('kiss.0'):
            raw.add_section('kiss.0')
            for key, val in raw.items('kiss'):
                if key != '__name__':
                    raw.set('kiss.0', key, val)
        kiss_sections[0] = 'kiss.0'

    # CLI overrides for single-port backward compat
    if args.kiss_type:
        raw.set(port_sections[0], 'type', args.kiss_type)
    if args.kiss_device:
        raw.set(port_sections[0], 'device', args.kiss_device)
    if args.kiss_host:
        raw.set(port_sections[0], 'host', args.kiss_host)
    if args.kiss_port:
        raw.set(port_sections[0], 'port', str(args.kiss_port))
    if args.baudrate:
        raw.set(port_sections[0], 'serial_baudrate', str(args.baudrate))
    if getattr(args, 'ota_baudrate', None):
        raw.set(port_sections[0], 'ota_baudrate', str(args.ota_baudrate))

    return PortConfig(raw, port_sections, kiss_sections)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestMultiPortConfig -v`
Expected: PASS

- [ ] **Step 5: Run full test suite for regressions**

Run: `pytest`
Expected: Some existing tests may fail because they use `config['client']` / `config['kiss']` directly. That's expected — we'll fix those in Task 2.

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: multi-port config parsing with [client.N] / [kiss.N] sections

Adds PortConfig wrapper class that detects numbered port sections,
validates contiguity, and provides per-port config access.
Backward-compatible: bare [client]/[kiss] treated as port 0 with
deprecation warning."
```

---

### Task 2: Refactor KISSClient to Accept Per-Port Config

**Files:**
- Modify: `tncd.py:434-510` (`KISSClient.__init__` and `connect`)
- Modify: `tncd.py:445-451` (`_get_kiss_params`)
- Test: `tests/test_tncd.py`

- [ ] **Step 1: Write failing test for KISSClient with port config**

```python
class TestKISSClientPortConfig:
    """Tests for KISSClient accepting per-port config."""

    def test_kiss_client_reads_from_port_section(self):
        """KISSClient reads connection params from its port section."""
        raw = configparser.ConfigParser()
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '10.0.0.1')
        raw.set('client.1', 'port', '9001')
        raw.set('client.1', 'ota_baudrate', '9600')
        raw.add_section('kiss.1')
        raw.set('kiss.1', 'tx_delay', '80')

        client = KISSClient(port_num=1, port_section='client.1',
                            kiss_section='kiss.1', raw_config=raw)
        assert client.port_num == 1
        assert client._get_kiss_params() == {'TX_DELAY': 80}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `pytest tests/test_tncd.py::TestKISSClientPortConfig -v`
Expected: FAIL — `KISSClient.__init__` doesn't accept those params.

- [ ] **Step 3: Refactor KISSClient constructor**

Change `KISSClient.__init__` to accept per-port parameters instead of a global config:

```python
class KISSClient:
    def __init__(self, port_num, port_section, kiss_section, raw_config, traffic_debug=0):
        self.port_num = port_num
        self.port_section = port_section
        self.kiss_section = kiss_section
        self.config = raw_config
        self.connection = None
        self.bridge = None
        self.traffic_debug = traffic_debug
        self._rx_thread = None
        self.online = False
        self.name = raw_config.get(port_section, 'name', fallback=f'Port {port_num}')

    def _get_kiss_params(self):
        params = {}
        if self.kiss_section and self.config.has_section(self.kiss_section):
            for key in ['TX_DELAY', 'PERSISTENCE', 'SLOT_TIME', 'TX_TAIL', 'FULL_DUPLEX']:
                if self.config.has_option(self.kiss_section, key.lower()):
                    params[key] = self.config.getint(self.kiss_section, key.lower())
        return params

    async def connect(self):
        conn_type = self.config.get(self.port_section, 'type', fallback='serial')
        kiss_params = self._get_kiss_params()
        loop = asyncio.get_running_loop()
        init_str = self.config.get(self.port_section, 'init_string', fallback=None)
        init_delay = self.config.getfloat(self.port_section, 'init_delay', fallback=1.0)

        if conn_type == 'tcp':
            host = self.config.get(self.port_section, 'host', fallback='localhost')
            port = self.config.getint(self.port_section, 'port', fallback=8001)
            # ... rest unchanged, using self.port_section instead of 'client'
```

Update all `self.config.get('client', ...)` in `KISSClient.connect()` to use `self.port_section`.

- [ ] **Step 4: Run test to verify it passes**

Run: `pytest tests/test_tncd.py::TestKISSClientPortConfig -v`
Expected: PASS

- [ ] **Step 5: Update test helpers to use new KISSClient interface**

Update `make_protocol()` and `make_real_protocol()` in the test file to construct `KISSClient` with the new signature. Also update `Bridge.__init__` (Task 3 will do the full rewrite, but we need it to not break here).

- [ ] **Step 6: Run full test suite**

Run: `pytest`
Expected: PASS (all tests adapted)

- [ ] **Step 7: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "refactor: KISSClient accepts per-port section names

KISSClient now reads connection and KISS parameters from its assigned
port section ([client.N]) and kiss section ([kiss.N]) rather than
hardcoded [client]/[kiss] global sections."
```

---

### Task 3: Refactor Bridge to Hold Multiple KISSClients

**Files:**
- Modify: `tncd.py:797-850` (`Bridge.__init__`, `Bridge.start`)
- Modify: `tncd.py:920-927` (`_send_ax25`, `send_to_kiss`)
- Modify: `tncd.py:1157` (`on_kiss_frame`)
- Test: `tests/test_tncd.py`

- [ ] **Step 1: Write failing test for multi-port Bridge**

```python
class TestMultiPortBridge:
    """Tests for Bridge managing multiple KISSClients."""

    def test_bridge_creates_multiple_kiss_clients(self):
        """Bridge creates one KISSClient per configured port."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '9600')

        config = PortConfig(raw, ['client.0', 'client.1'], {})
        bridge = Bridge(config)
        assert len(bridge.kiss_clients) == 2
        assert bridge.kiss_clients[0].port_num == 0
        assert bridge.kiss_clients[1].port_num == 1

    def test_send_to_kiss_routes_by_port(self):
        """send_to_kiss routes data to the correct KISSClient."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '1200')

        config = PortConfig(raw, ['client.0', 'client.1'], {})
        bridge = Bridge(config)
        bridge.kiss_clients[0] = Mock()
        bridge.kiss_clients[0].online = True
        bridge.kiss_clients[1] = Mock()
        bridge.kiss_clients[1].online = True

        bridge.send_to_kiss(0, b'\x00test_data')
        bridge.kiss_clients[0].send.assert_called_once_with(b'\x00test_data')
        bridge.kiss_clients[1].send.assert_not_called()

    def test_on_kiss_frame_tags_port(self):
        """on_kiss_frame is called with port number from the KISSClient."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')

        config = PortConfig(raw, ['client.0'], {})
        bridge = Bridge(config)
        bridge.kiss_clients[0] = Mock()
        # Simulate a KISS frame arriving — the port number should be included
        # in monitoring and connection lookups
        # (integration tested more thoroughly in Task 5)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestMultiPortBridge -v`
Expected: FAIL — `Bridge` still creates single `kiss_client`.

- [ ] **Step 3: Rewrite Bridge.__init__ for multiple ports**

```python
class Bridge:
    def __init__(self, config, traffic_debug=0, verbose=0):
        self.config = config
        self.clients = []
        self.connections = {}   # (port, local, remote) -> Connection
        self.callsign = config.get('server', 'callsign', fallback='AGWPE')
        self.traffic_debug = traffic_debug
        self.verbose = verbose
        self._sent_frames = collections.deque(maxlen=20)

        # Create a KISSClient per port
        self.kiss_clients = []
        for i in range(config.port_count):
            port_section = config._ports[i]
            kiss_section = config._kiss.get(i)
            kc = KISSClient(
                port_num=i,
                port_section=port_section,
                kiss_section=kiss_section,
                raw_config=config._raw,
                traffic_debug=traffic_debug,
            )
            kc.set_bridge(self)
            self.kiss_clients.append(kc)

        # Per-port T1/T2 timers and window size
        self._port_params = []
        for i in range(config.port_count):
            port_section = config._ports[i]
            max_window = config.getint('ax25', 'max_window', fallback=DEFAULT_MAX_WINDOW)
            max_window = max(1, min(7, max_window))
            n2_retry = config.getint('ax25', 'n2_retry', fallback=DEFAULT_N2_RETRY)
            ota_baudrate = config._raw.getint(port_section, 'ota_baudrate', fallback=1200)
            max_frame_bytes = 256 + AX25_OVERHEAD
            frame_time = (max_frame_bytes * 8) / ota_baudrate
            turnaround = 1.0
            t1_timeout = max(3.0, 2.0 * (max_window * frame_time + turnaround))
            t2_delay = max(0.1, T2_MULTIPLIER * frame_time)

            self._port_params.append({
                'max_window': max_window,
                'n2_retry': n2_retry,
                't1_timeout': t1_timeout,
                't2_delay': t2_delay,
                'ota_baudrate': ota_baudrate,
            })
            logger.info(
                f"Port {i}: window={max_window}, T1={t1_timeout:.1f}s, "
                f"T2={t2_delay:.2f}s (ota_baudrate={ota_baudrate})"
            )

        # Backward compat: expose first port's params as instance attrs
        # (used by existing code until fully ported)
        if self._port_params:
            self.max_window = self._port_params[0]['max_window']
            self.n2_retry = self._port_params[0]['n2_retry']
            self.t1_timeout = self._port_params[0]['t1_timeout']
            self.t2_delay = self._port_params[0]['t2_delay']
```

- [ ] **Step 4: Rewrite Bridge.start() for parallel port connection**

```python
    async def start(self):
        # Connect all ports in parallel; failures start reconnect loops
        connect_tasks = []
        for kc in self.kiss_clients:
            connect_tasks.append(asyncio.create_task(self._connect_port(kc)))
        await asyncio.gather(*connect_tasks, return_exceptions=True)

        # Start AGWPE server immediately (doesn't wait for all TNCs)
        loop = asyncio.get_running_loop()
        server_cfg = self.config['server']
        host = server_cfg.get('listen_host', '0.0.0.0')
        port = server_cfg.getint('listen_port', 8000)
        logger.info(f"Starting AGWPE server on {host}:{port}")
        server = await loop.create_server(
            lambda: AGWPEServerProtocol(self, self.traffic_debug),
            host, port
        )

    async def _connect_port(self, kc):
        """Connect a single port, marking it online on success."""
        try:
            await kc.connect()
            loop = asyncio.get_running_loop()
            kc.start_receive(loop)
            kc.online = True
            logger.info(f"Port {kc.port_num} ({kc.name}) online")
        except Exception as e:
            logger.error(f"Port {kc.port_num} ({kc.name}) failed to connect: {e}")
            kc.online = False
            # Port enters its reconnect loop if applicable
```

- [ ] **Step 5: Update send_to_kiss to route by port**

```python
    def send_to_kiss(self, port, data):
        """Send data to the KISSClient for the given port number."""
        if port >= len(self.kiss_clients):
            return
        kc = self.kiss_clients[port]
        if not kc.online:
            return
        self._sent_frames.append(bytes(data))
        kc.send(data)
```

Update `_send_ax25` to pass port number:

```python
    def _send_ax25(self, frame, port=0):
        """Log (at verbose>=1) and send an AX.25 frame to the KISS TNC."""
        self._log_ax25(frame, 'TX')
        self.send_to_kiss(port, bytes(frame))
```

- [ ] **Step 6: Update on_kiss_frame to accept port number**

```python
    def on_kiss_frame(self, port_num, raw_kiss):
        """Called on the asyncio thread when a frame arrives from a KISS TNC."""
        if not raw_kiss:
            return
        kiss_cmd = raw_kiss[0]
        if (kiss_cmd & 0x0F) != 0:
            logger.debug(f"Port {port_num}: ignoring non-data KISS command 0x{kiss_cmd:02x}")
            return
        raw_ax25 = raw_kiss[1:]
        if raw_ax25 in self._sent_frames:
            logger.debug(f"Port {port_num}: ignoring echoed TX frame")
            return
        # ... rest of dispatch unchanged, but uses port_num for connection lookups
```

Update `KISSClient.start_receive` to pass its `port_num` when calling `on_kiss_frame`:

```python
    def start_receive(self, loop):
        def _on_frame(frame):
            loop.call_soon_threadsafe(self.bridge.on_kiss_frame, self.port_num, frame)
        # ...
```

- [ ] **Step 7: Run tests**

Run: `pytest tests/test_tncd.py::TestMultiPortBridge -v`
Expected: PASS

- [ ] **Step 8: Fix remaining test suite breakage**

Update all `make_real_protocol()` and other test helpers to use `PortConfig`. Ensure all existing tests pass by providing single-port configs in the new format.

- [ ] **Step 9: Run full test suite**

Run: `pytest`
Expected: PASS

- [ ] **Step 10: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: Bridge manages multiple KISSClients indexed by port

Bridge creates one KISSClient per configured port. Ports connect in
parallel at startup. send_to_kiss routes by port number.
on_kiss_frame receives port number from the originating KISSClient.
Per-port T1/T2/window parameters derived from each port's ota_baudrate."
```

---

### Task 4: Per-Port AX.25 State and T1/T2 Timers

**Files:**
- Modify: `tncd.py` (Bridge methods that use `self.t1_timeout`, `self.t2_delay`, `self.max_window`, `self.n2_retry`)
- Test: `tests/test_tncd.py`

- [ ] **Step 1: Write failing test for per-port timer values**

```python
class TestPerPortTimers:
    """Tests for per-port T1/T2 timer derivation."""

    def test_different_ota_baud_gives_different_t1(self):
        """Ports with different ota_baudrate get different T1 values."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '9600')

        config = PortConfig(raw, ['client.0', 'client.1'], {})
        bridge = Bridge(config)
        # 1200 baud should have a much longer T1 than 9600 baud
        assert bridge._port_params[0]['t1_timeout'] > bridge._port_params[1]['t1_timeout']

    def test_start_t1_uses_port_params(self):
        """_start_t1 uses the connection's port T1 timeout."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '9600')

        config = PortConfig(raw, ['client.0', 'client.1'], {})
        bridge = Bridge(config)
        bridge.kiss_clients[0] = Mock()
        bridge.kiss_clients[1] = Mock()

        conn0 = bridge.get_or_create_connection(0, 'A', 'B')
        conn1 = bridge.get_or_create_connection(1, 'A', 'C')

        # Verify _start_t1 uses the correct port's timeout
        with patch('asyncio.get_event_loop') as mock_loop:
            mock_loop.return_value.call_later = Mock()
            bridge._start_t1(conn0)
            t1_0 = mock_loop.return_value.call_later.call_args[0][0]
            bridge._start_t1(conn1)
            t1_1 = mock_loop.return_value.call_later.call_args[0][0]
            assert t1_0 > t1_1  # 1200 baud → longer T1
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestPerPortTimers -v`
Expected: FAIL

- [ ] **Step 3: Update _start_t1, _start_t2, _drain_outbound to use per-port params**

Find all uses of `self.t1_timeout`, `self.t2_delay`, `self.max_window`, `self.n2_retry` in Bridge methods. Replace with lookups from `self._port_params[conn.port]`:

```python
    def _get_port_param(self, port, key):
        """Get a per-port parameter (t1_timeout, t2_delay, max_window, n2_retry)."""
        if port < len(self._port_params):
            return self._port_params[port][key]
        # Fallback for safety
        return self._port_params[0][key]

    def _start_t1(self, conn):
        self._cancel_t1(conn)
        timeout = self._get_port_param(conn.port, 't1_timeout')
        loop = asyncio.get_event_loop()
        conn.t1_handle = loop.call_later(timeout, self._t1_expired, conn)

    def _start_t2(self, conn):
        self._cancel_t2(conn)
        delay = self._get_port_param(conn.port, 't2_delay')
        loop = asyncio.get_event_loop()
        conn.t2_handle = loop.call_later(delay, self._t2_expired, conn)
```

Update `_drain_outbound` to use `self._get_port_param(conn.port, 'max_window')` instead of `self.max_window`.

Update `_t1_expired` to use `self._get_port_param(conn.port, 'n2_retry')` instead of `self.n2_retry`.

- [ ] **Step 4: Run tests**

Run: `pytest tests/test_tncd.py::TestPerPortTimers -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `pytest`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: per-port AX.25 T1/T2 timers and window size

Each port derives T1, T2, max_window from its own ota_baudrate.
_start_t1, _start_t2, _drain_outbound, _t1_expired all look up
the connection's port params instead of global Bridge-level values."
```

---

### Task 5: AGWPE G/g Frame Routing for Multiple Ports

**Files:**
- Modify: `tncd.py:427-431` (`send_port_info`)
- Modify: `tncd.py:181-193` (`g` frame handler)
- Test: `tests/test_tncd.py`

- [ ] **Step 1: Write failing tests for G and g frames**

```python
class TestMultiPortAGWPE:
    """Tests for AGWPE multi-port protocol frames."""

    async def test_G_frame_reports_all_ports(self):
        """G frame returns count and names of all configured ports."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.set('client.0', 'name', 'TNC3 (2m)')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '1200')
        raw.set('client.1', 'name', 'TS-2000 (HF)')

        config = PortConfig(raw, ['client.0', 'client.1'], {})
        bridge = Bridge(config)
        bridge.kiss_clients = [Mock(), Mock()]
        protocol = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        protocol.connection_made(transport)

        protocol.data_received(make_frame(0, ord('G')))
        written = transport.write.call_args[0][0]
        resp = parse_frame(written)
        assert resp['kind'] == 'G'
        payload = resp['data'].split(b'\x00')[0].decode()
        assert payload == '2;TNC3 (2m);TS-2000 (HF);'

    async def test_g_frame_returns_port_specific_kiss_params(self):
        """g frame returns KISS params for the requested port."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '1200')
        raw.add_section('kiss.1')
        raw.set('kiss.1', 'tx_delay', '80')
        raw.set('kiss.1', 'persistence', '32')

        config = PortConfig(raw, ['client.0', 'client.1'], {1: 'kiss.1'})
        bridge = Bridge(config)
        bridge.kiss_clients = [Mock(), Mock()]
        protocol = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        protocol.connection_made(transport)

        # Request port 1 capabilities
        protocol.data_received(make_frame(1, ord('g')))
        written = transport.write.call_args[0][0]
        resp = parse_frame(written)
        assert resp['port'] == 1
        caps = struct.unpack('<8BI', resp['data'])
        assert caps[2] == 80   # tx_delay
        assert caps[4] == 32   # persistence
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestMultiPortAGWPE -v`
Expected: FAIL

- [ ] **Step 3: Update send_port_info for multi-port**

```python
    def send_port_info(self):
        """Send G frame with count and names of all configured ports."""
        count = self.bridge.config.port_count
        names = [self.bridge.config.port_name(i) for i in range(count)]
        payload = f"{count};{';'.join(names)};"
        self.send_frame(0, ord(b'G'), b'', b'', payload.encode())
```

- [ ] **Step 4: Update g frame handler for per-port KISS params**

```python
        elif datakind_bytes == b'g':
            logger.debug(f"PORT CAPABILITIES request for port {port}")
            kiss_cfg = self.bridge.config.kiss_config(port)
            tx_delay    = int(kiss_cfg.get('tx_delay', '40'))
            persistence = int(kiss_cfg.get('persistence', '63'))
            slot_time   = int(kiss_cfg.get('slot_time', '20'))
            tx_tail     = int(kiss_cfg.get('tx_tail', '30'))
            caps = struct.pack('<8BI', 0, 255, tx_delay, tx_tail,
                               persistence, slot_time, 7, 0, 0)
            self.send_frame(port, ord(b'g'), b'', b'', caps)
```

- [ ] **Step 5: Run tests**

Run: `pytest tests/test_tncd.py::TestMultiPortAGWPE -v`
Expected: PASS

- [ ] **Step 6: Run full test suite**

Run: `pytest`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: AGWPE G/g frames report multi-port info

G frame returns port count and names from config.
g frame returns per-port KISS parameters from [kiss.N] sections."
```

---

### Task 6: Route All AGWPE Frames by Port Byte

**Files:**
- Modify: `tncd.py:230-350` (C, D, d, M, V, K frame handlers)
- Test: `tests/test_tncd.py`

- [ ] **Step 1: Write failing test for port-routed send**

```python
class TestPortRouting:
    """Tests for AGWPE frame routing to correct port."""

    async def test_unproto_sent_to_correct_port(self):
        """M frame on port 1 sends via kiss_clients[1]."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '1200')

        config = PortConfig(raw, ['client.0', 'client.1'], {})
        bridge = Bridge(config)
        bridge.kiss_clients[0] = Mock()
        bridge.kiss_clients[0].online = True
        bridge.kiss_clients[1] = Mock()
        bridge.kiss_clients[1].online = True
        protocol = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        protocol.connection_made(transport)

        # Send M frame on port 1
        protocol.data_received(make_frame(1, ord('M'), b'SRC', b'DST', b'hello'))
        # Should route to kiss_clients[1]
        bridge.kiss_clients[1].send.assert_called_once()
        bridge.kiss_clients[0].send.assert_not_called()
```

- [ ] **Step 2: Run test to verify it fails**

Run: `pytest tests/test_tncd.py::TestPortRouting -v`
Expected: FAIL

- [ ] **Step 3: Update _send_ax25 calls to pass port number**

All code paths that call `self.bridge._send_ax25(frame)` must now pass the port from the AGWPE header or connection object:

- `_send_unproto` → pass `port` arg through
- `C` (connect) handler → pass `port` to `_send_ax25`
- `D` (disconnect) handler → use `conn.port`
- `d` (data) handler → use `conn.port`
- In `_drain_outbound` → use `conn.port`
- In `_t1_expired` → use `conn.port`
- In `_t2_expired` → use `conn.port`
- In `_dispatch_sabm`, `_dispatch_ua`, `_dispatch_disc`, `_dispatch_s` → use connection's port

The key change: `_send_ax25(self, frame, port=0)` and `send_to_kiss(self, port, data)`.

- [ ] **Step 4: Run tests**

Run: `pytest tests/test_tncd.py::TestPortRouting -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `pytest`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: route all AGWPE frames by port byte to correct KISSClient

M, V, C, D, d, K frames and all AX.25 responses now route through
the port number in the AGWPE header to the correct KISSClient."
```

---

### Task 7: Offline Port Behavior

**Files:**
- Modify: `tncd.py` (C handler, M/V/K handlers, port disconnect logic)
- Test: `tests/test_tncd.py`

- [ ] **Step 1: Write failing tests for offline port behavior**

```python
class TestOfflinePort:
    """Tests for offline port behavior."""

    async def test_connect_on_offline_port_returns_busy(self):
        """C frame on offline port returns BUSY + d frame."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')

        config = PortConfig(raw, ['client.0'], {})
        bridge = Bridge(config)
        bridge.kiss_clients[0] = Mock()
        bridge.kiss_clients[0].online = False  # port is offline

        protocol = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        protocol.connection_made(transport)
        protocol.registered_calls.add('SRC')

        protocol.data_received(make_frame(0, ord('C'), b'SRC', b'DST'))

        # Should get a d (disconnect) frame with BUSY message
        calls = transport.write.call_args_list
        # Find the 'd' frame
        found_d = False
        for call in calls:
            resp = parse_frame(call[0][0])
            if resp['kind'] == 'd':
                found_d = True
                assert b'BUSY' in resp['data'] or b'*** BUSY' in resp['data']
        assert found_d

    async def test_unproto_on_offline_port_silently_dropped(self):
        """M frame on offline port is silently dropped."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')

        config = PortConfig(raw, ['client.0'], {})
        bridge = Bridge(config)
        bridge.kiss_clients[0] = Mock()
        bridge.kiss_clients[0].online = False

        protocol = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        protocol.connection_made(transport)

        protocol.data_received(make_frame(0, ord('M'), b'SRC', b'DST', b'hello'))
        # No send to KISS, no error to client
        bridge.kiss_clients[0].send.assert_not_called()
        transport.write.assert_not_called()

    async def test_invalid_port_silently_ignored(self):
        """Frame with port >= port_count is silently ignored."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')

        config = PortConfig(raw, ['client.0'], {})
        bridge = Bridge(config)
        bridge.kiss_clients[0] = Mock()
        bridge.kiss_clients[0].online = True

        protocol = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        protocol.connection_made(transport)

        # Port 5 doesn't exist
        protocol.data_received(make_frame(5, ord('M'), b'SRC', b'DST', b'hello'))
        bridge.kiss_clients[0].send.assert_not_called()
        transport.write.assert_not_called()
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestOfflinePort -v`
Expected: FAIL

- [ ] **Step 3: Implement offline port guards**

Add port validation at the top of `handle_frame()`:

```python
    def handle_frame(self, port, datakind, pid, call_from, call_to, data):
        # ... existing parsing ...

        # Validate port number for frame types that route to a port
        routed_kinds = {b'M', b'V', b'C', b'c', b'D', b'd', b'K', b'g', b'y', b'Y'}
        if datakind_bytes in routed_kinds:
            if port >= self.bridge.config.port_count:
                logger.debug(f"Ignoring frame for invalid port {port}")
                return
```

In the `C` handler, before sending SABM, check port online status:

```python
        elif datakind_bytes == b'C':
            if not self.bridge.kiss_clients[port].online:
                # Port offline — send BUSY notification
                busy_msg = f"*** BUSY From {from_str}\r".encode()
                self.send_frame(port, ord(b'd'), call_from, call_to, busy_msg)
                return
            # ... existing SABM logic
```

In `send_to_kiss`, the online check already returns early for offline ports (from Task 3). M/V/K silently drop by that mechanism.

- [ ] **Step 4: Run tests**

Run: `pytest tests/test_tncd.py::TestOfflinePort -v`
Expected: PASS

- [ ] **Step 5: Add port-goes-offline-mid-session logic**

When a port goes offline (connection lost), disconnect all active AX.25 connections on that port:

```python
    def _port_went_offline(self, port_num):
        """Called when a KISSClient loses its connection."""
        logger.warning(f"Port {port_num} went offline")
        self.kiss_clients[port_num].online = False
        # Notify active connections
        to_remove = [(k, conn) for k, conn in self.connections.items()
                     if conn.port == port_num and conn.state in ('CONNECTED', 'CONNECTING')]
        for key, conn in to_remove:
            self._cancel_t1(conn)
            self._cancel_t2(conn)
            if conn.owner:
                msg = f"*** DISCONNECTED From {conn.remote}\r".encode()
                try:
                    conn.owner.send_frame(port_num, ord(b'd'),
                                          conn.local.encode(), conn.remote.encode(), msg)
                except Exception:
                    pass
            del self.connections[key]
```

- [ ] **Step 6: Run full test suite**

Run: `pytest`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: offline port behavior (BUSY, silent drop, mid-session disconnect)

C on offline port returns BUSY + d frame. M/V/K on offline port
silently dropped. Invalid port numbers ignored. Port going offline
disconnects active AX.25 sessions with notification."
```

---

### Task 8: Monitoring Frames Include Port Number

**Files:**
- Modify: `tncd.py` (monitoring dispatch in `on_kiss_frame` / `_dispatch_ui`)
- Test: `tests/test_tncd.py`

- [ ] **Step 1: Write failing test**

```python
class TestMultiPortMonitoring:
    """Tests for monitoring across multiple ports."""

    async def test_monitor_frame_includes_correct_port(self):
        """UI frames received on port 1 are monitored with port=1."""
        raw = configparser.ConfigParser()
        raw.add_section('server')
        raw.set('server', 'listen_host', '0.0.0.0')
        raw.set('server', 'listen_port', '8000')
        raw.set('server', 'callsign', 'TEST')
        raw.add_section('client.0')
        raw.set('client.0', 'type', 'tcp')
        raw.set('client.0', 'host', '127.0.0.1')
        raw.set('client.0', 'port', '8001')
        raw.set('client.0', 'ota_baudrate', '1200')
        raw.add_section('client.1')
        raw.set('client.1', 'type', 'tcp')
        raw.set('client.1', 'host', '127.0.0.1')
        raw.set('client.1', 'port', '8002')
        raw.set('client.1', 'ota_baudrate', '1200')

        config = PortConfig(raw, ['client.0', 'client.1'], {})
        bridge = Bridge(config)
        bridge.kiss_clients = [Mock(), Mock()]
        bridge.kiss_clients[0].online = True
        bridge.kiss_clients[1].online = True

        protocol = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        protocol.connection_made(transport)
        protocol.monitoring = True

        # Build a UI frame as if received on port 1
        ui_frame = ax25.Frame(
            dst=ax25.Address('CQ'),
            src=ax25.Address('N0CALL'),
            control=ax25.Control(ax25.FrameType.UI),
            info=b'test'
        )
        raw_ax25 = bytes(ui_frame)
        # Simulate KISS frame with command byte
        bridge.on_kiss_frame(1, bytes([0x00]) + raw_ax25)

        # Monitoring frame should have port=1
        calls = transport.write.call_args_list
        assert len(calls) >= 1
        resp = parse_frame(calls[0][0][0])
        assert resp['port'] == 1
```

- [ ] **Step 2: Run test to verify it fails**

Run: `pytest tests/test_tncd.py::TestMultiPortMonitoring -v`
Expected: FAIL

- [ ] **Step 3: Pass port_num through dispatch chain**

Ensure `on_kiss_frame(port_num, raw_kiss)` passes `port_num` to `_dispatch_ui`, `_dispatch_i`, `_dispatch_sabm`, etc. so monitoring frames carry the correct port byte.

The key changes:
- `_dispatch_ui(self, frame, port_num)` → `send_frame(port_num, ...)`
- `_dispatch_monitor(self, frame, port_num)` → `send_frame(port_num, ...)`
- All S-frame monitoring → use `conn.port`

- [ ] **Step 4: Run tests**

Run: `pytest tests/test_tncd.py::TestMultiPortMonitoring -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `pytest`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "feat: monitoring frames carry correct port number

UI, I, and S-frame monitoring notifications include the port byte
from the KISSClient that received the frame."
```

---

### Task 9: Update Existing Tests and Final Integration

**Files:**
- Modify: `tests/test_tncd.py` (update all helpers and existing tests)
- Modify: `tncd.py` (any remaining single-port assumptions)

- [ ] **Step 1: Audit and fix all remaining test failures**

Run `pytest` and fix any remaining test breakage from the multi-port refactor. Common fixes:
- Update `make_protocol()` and `make_real_protocol()` to build `PortConfig`
- Update mocked bridge configs to use new format
- Ensure `bridge.kiss_client` references are updated to `bridge.kiss_clients[0]`

- [ ] **Step 2: Run full test suite**

Run: `pytest`
Expected: ALL PASS

- [ ] **Step 3: Manual smoke test with single-port config**

Create a test config with bare `[client]` section and verify it works with deprecation warning:

Run: `python tncd.py -c tests/fixtures/legacy-single-port.ini` (brief startup check)

- [ ] **Step 4: Manual smoke test with multi-port config**

Create a test config with two TCP Direwolf instances and verify both ports show in AGWPE G response.

- [ ] **Step 5: Commit**

```bash
git add tncd.py tests/test_tncd.py
git commit -m "fix: update all tests for multi-port architecture

Adapts test helpers and assertions to use PortConfig.
All existing functionality preserved with single-port configs."
```

---

### Task 10: Documentation Update

**Files:**
- Modify: `PLAN.md`
- Modify: `nix/README.md`
- Modify: `README.md` (if config examples need updating)

- [ ] **Step 1: Add Milestone 5 to PLAN.md**

```markdown
### Milestone 5: Multi-Port / Multi-Modem Support (COMPLETE)
Multiple KISS TNC connections managed simultaneously:

- `[client.N]` numbered port sections with per-port config
- `[kiss.N]` per-port KISS parameters
- Per-port AX.25 state (connections, T1/T2 timers, window size)
- AGWPE `G` frame reports port count and names
- AGWPE `g` frame returns per-port KISS capabilities
- Ports connect in parallel; offline ports return BUSY for `C` frames
- Backward compatible: bare `[client]`/`[kiss]` treated as port 0
```

- [ ] **Step 2: Update nix/README.md with multi-port example**

Add a "Multi-Port" section showing `[client.0]` / `[client.1]` / `[kiss.1]` Nix config.

- [ ] **Step 3: Commit**

```bash
git add PLAN.md nix/README.md
git commit -m "docs: add multi-port milestone and config examples"
```
