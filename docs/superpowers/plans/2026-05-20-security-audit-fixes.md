# Security Audit Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix all security audit findings: enforce AGWPE connection ownership, unique callsign registration, client lifecycle cleanup, client limits/idle timeout, and retire the standalone tncd-rfcomm service.

**Architecture:** All AGWPE protocol fixes are in `tncd.py` — owner checks added to handlers, registration uniqueness enforced in X handler, client cleanup in `remove_client`, limits/timeout in Bridge init and connection_made. The tncd-rfcomm standalone script and service file are deleted, with references stripped from packaging files.

**Tech Stack:** Python 3, asyncio, pytest, pytest-asyncio

---

### Task 1: Owner enforcement on D handler

**Files:**
- Modify: `tests/test_tncd.py` (add test class after existing TestConnectedMode)
- Modify: `tncd.py:336-357` (D handler)

- [ ] **Step 1: Write the failing test for non-owner D rejection**

Add to `tests/test_tncd.py` after existing test classes:

```python
class TestOwnerEnforcement:
    """Security audit: non-owner clients must not operate on another client's session."""

    def test_non_owner_D_rejected(self):
        """Non-owner client cannot send data on another client's connection."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner

        intruder = AGWPEServerProtocol(bridge)
        intruder_transport = Mock()
        intruder_transport.is_closing.return_value = False
        intruder.connection_made(intruder_transport)

        intruder.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'evil'))
        # No data should be queued — intruder is not the owner
        assert len(conn.outbound_queue) == 0

    def test_owner_D_allowed(self):
        """Owner client can still send data normally."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner

        owner.data_received(make_frame(0, ord('D'), b'W1ABC', b'W2DEF', b'hello'))
        assert len(conn.outbound_queue) > 0 or bridge.kiss_client.send.called
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement -v`
Expected: `test_non_owner_D_rejected` FAILS (data gets queued because no owner check exists)

- [ ] **Step 3: Add owner check to D handler**

In `tncd.py`, modify the `D` handler (around line 336). Change:

```python
        elif datakind_bytes == b'D':
            # Send data over a connected session as an AX.25 I-frame.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if not conn or conn.state != 'CONNECTED':
                logger.warning(
                    f"'D' from {from_str!r} to {to_str!r}: "
                    f"no active connection (state={conn.state if conn else 'None'})")
            else:
```

To:

```python
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: enforce owner check on D (data send) handler"
```

---

### Task 2: Owner enforcement on d handler

**Files:**
- Modify: `tests/test_tncd.py` (add tests to TestOwnerEnforcement)
- Modify: `tncd.py:359-372` (d handler)

- [ ] **Step 1: Write the failing test for non-owner d rejection**

Add to `TestOwnerEnforcement` in `tests/test_tncd.py`:

```python
    def test_non_owner_d_rejected(self):
        """Non-owner client cannot disconnect another client's session."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner

        intruder = AGWPEServerProtocol(bridge)
        intruder_transport = Mock()
        intruder_transport.is_closing.return_value = False
        intruder.connection_made(intruder_transport)

        intruder.data_received(make_frame(0, ord('d'), b'W1ABC', b'W2DEF'))
        # Connection state must remain CONNECTED — intruder cannot disconnect
        assert conn.state == 'CONNECTED'

    def test_owner_d_allowed(self):
        """Owner client can disconnect their own session."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner

        owner.data_received(make_frame(0, ord('d'), b'W1ABC', b'W2DEF'))
        assert conn.state == 'DISCONNECTING'
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement::test_non_owner_d_rejected -v`
Expected: FAIL (state changes to DISCONNECTING)

- [ ] **Step 3: Add owner check to d handler**

In `tncd.py`, modify the `d` handler (around line 359). Change:

```python
        elif datakind_bytes == b'd':
            # Disconnect: send DISC to TNC.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if not conn:
                logger.warning(f"'d' disconnect with no connection {from_str!r} -> {to_str!r}")
            else:
```

To:

```python
        elif datakind_bytes == b'd':
            # Disconnect: send DISC to TNC.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if not conn:
                logger.warning(f"'d' disconnect with no connection {from_str!r} -> {to_str!r}")
            elif conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'd' for {from_str}->{to_str}")
            else:
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: enforce owner check on d (disconnect) handler"
```

---

### Task 3: Owner enforcement on K handler

**Files:**
- Modify: `tests/test_tncd.py` (add tests to TestOwnerEnforcement)
- Modify: `tncd.py:374-379` (K handler)

- [ ] **Step 1: Write the failing test for non-owner K rejection**

Add to `TestOwnerEnforcement` in `tests/test_tncd.py`:

```python
    def test_non_owner_K_rejected_when_connection_exists(self):
        """Non-owner cannot send raw frames when a connection exists for callsign pair."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner

        intruder = AGWPEServerProtocol(bridge)
        intruder_transport = Mock()
        intruder_transport.is_closing.return_value = False
        intruder.connection_made(intruder_transport)

        bridge.kiss_client.send.reset_mock()
        # K frame: pe prepends 0x00 port byte, then raw AX.25 bytes
        intruder.data_received(make_frame(0, ord('K'), b'W1ABC', b'W2DEF', b'\x00rawdata'))
        bridge.kiss_client.send.assert_not_called()

    def test_K_allowed_when_no_connection(self):
        """K frame is allowed when no connection exists for the callsign pair (e.g. raw UI)."""
        proto, _, bridge = make_real_protocol()
        bridge.kiss_client.send.reset_mock()
        proto.data_received(make_frame(0, ord('K'), b'W1ABC', b'W2DEF', b'\x00rawdata'))
        bridge.kiss_client.send.assert_called_once()
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement::test_non_owner_K_rejected_when_connection_exists -v`
Expected: FAIL (raw frame gets sent regardless)

- [ ] **Step 3: Add owner check to K handler**

In `tncd.py`, modify the `K` handler (around line 374). Change:

```python
        elif datakind_bytes == b'K':
            # Raw AX.25 frame. pe prepends a 0x00 byte (port indicator), strip it.
            raw = data[1:] if (data and data[0] == 0x00) else data
            logger.debug(f"Raw KISS frame, {len(raw)} bytes")
            if raw:
                self.bridge.send_to_kiss(port, raw)
```

To:

```python
        elif datakind_bytes == b'K':
            # Raw AX.25 frame. pe prepends a 0x00 byte (port indicator), strip it.
            raw = data[1:] if (data and data[0] == 0x00) else data
            logger.debug(f"Raw KISS frame, {len(raw)} bytes")
            if raw:
                conn = self.bridge.get_connection(port, from_str, to_str)
                if conn and conn.owner is not self:
                    logger.warning(f"Rejecting non-owner 'K' for {from_str}->{to_str}")
                    return
                self.bridge.send_to_kiss(port, raw)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: enforce owner check on K (raw frame) handler"
```

---

### Task 4: Owner enforcement on Y handler

**Files:**
- Modify: `tests/test_tncd.py` (add tests to TestOwnerEnforcement)
- Modify: `tncd.py:391-403` (Y handler)

- [ ] **Step 1: Write the failing test for non-owner Y returning 0**

Add to `TestOwnerEnforcement` in `tests/test_tncd.py`:

```python
    def test_non_owner_Y_returns_zero(self):
        """Non-owner Y query must return 0 outstanding frames (don't leak queue depth)."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        conn.unacked = 5
        conn.outbound_queue.append((b'data', 0xF0))

        intruder = AGWPEServerProtocol(bridge)
        intruder_transport = Mock()
        intruder_transport.is_closing.return_value = False
        intruder.connection_made(intruder_transport)

        intruder.data_received(make_frame(0, ord('Y'), b'W1ABC', b'W2DEF'))
        resp = parse_frame(intruder_transport.write.call_args[0][0])
        assert resp['kind'] == 'Y'
        count = struct.unpack('<I', resp['data'])[0]
        assert count == 0
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement::test_non_owner_Y_returns_zero -v`
Expected: FAIL (returns actual queue depth instead of 0)

- [ ] **Step 3: Add owner check to Y handler**

In `tncd.py`, modify the `Y` handler (around line 391). Change:

```python
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
```

To:

```python
        elif datakind_bytes == b'Y':
            # Outstanding frames for a connection: queued + unacked.
            # Matches Direwolf behavior (i_frame_queue + txdata_by_ns).
            # PAT's Flush() has a 60s timeout waiting for outstanding==0.
            # Including queued frames makes PAT's Write() flow control
            # throttle sends (blocking at outstanding > maxFrame), so
            # Flush() only waits for the last batch — not the whole transfer.
            conn = self.bridge.get_connection(port, from_str, to_str)
            if conn and conn.owner is not self:
                count = 0
            else:
                unacked = conn.unacked if conn else 0
                queued = len(conn.outbound_queue) if conn else 0
                count = unacked + queued
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: enforce owner check on Y (outstanding frames) handler"
```

---

### Task 5: Guard C/c/v against stealing active sessions

**Files:**
- Modify: `tests/test_tncd.py` (add tests to TestOwnerEnforcement)
- Modify: `tncd.py:259-334` (C, c, v handlers)

- [ ] **Step 1: Write the failing test for non-owner C on active session**

Add to `TestOwnerEnforcement` in `tests/test_tncd.py`:

```python
    def test_non_owner_C_on_active_session_gets_busy(self):
        """Non-owner C on a CONNECTED session must return BUSY without disturbing it."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner
        conn.send_seqno = 5
        conn.recv_seqno = 3

        intruder = AGWPEServerProtocol(bridge)
        intruder_transport = Mock()
        intruder_transport.is_closing.return_value = False
        intruder.connection_made(intruder_transport)

        intruder.data_received(make_frame(0, ord('C'), b'W1ABC', b'W2DEF'))

        # Intruder should get a BUSY response
        resp = parse_frame(intruder_transport.write.call_args[0][0])
        assert resp['kind'] == 'd'
        assert b'BUSY' in resp['data']

        # Original connection must be undisturbed
        assert conn.owner is owner
        assert conn.state == 'CONNECTED'
        assert conn.send_seqno == 5
        assert conn.recv_seqno == 3

    def test_owner_C_on_disconnected_session_allowed(self):
        """Owner can reconnect a session that isn't CONNECTED."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'DISCONNECTED'
        conn.owner = owner

        owner.data_received(make_frame(0, ord('C'), b'W1ABC', b'W2DEF'))
        assert conn.state == 'CONNECTING'
        assert conn.owner is owner
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement::test_non_owner_C_on_active_session_gets_busy -v`
Expected: FAIL (session gets stolen)

- [ ] **Step 3: Add owner guard to C, c, v handlers**

In `tncd.py`, modify the `C` handler (around line 259). After `get_or_create_connection` and the `if not conn` check, add the guard. The section becomes:

```python
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
            if conn.state == 'CONNECTED' and conn.owner is not self:
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
```

Apply the same guard to the `c` handler (around line 285). After `get_or_create_connection` and `if not conn`, add:

```python
            if conn.state == 'CONNECTED' and conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'c' for active {from_str}->{to_str}")
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
```

Apply the same guard to the `v` handler (around line 306). After `get_or_create_connection` and `if not conn`, add:

```python
            if conn.state == 'CONNECTED' and conn.owner is not self:
                logger.warning(f"Rejecting non-owner 'v' for active {from_str}->{to_str}")
                busy_msg = f'*** BUSY From {from_str}\r'.encode()
                self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
                return
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestOwnerEnforcement -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: guard C/c/v from stealing active sessions (BUSY response)"
```

---

### Task 6: Unique callsign registration and case normalization

**Files:**
- Modify: `tests/test_tncd.py` (add TestCallsignRegistration class)
- Modify: `tncd.py:224-234` (X and x handlers)

- [ ] **Step 1: Write failing tests for registration**

Add to `tests/test_tncd.py`:

```python
class TestCallsignRegistration:
    """Security audit: unique callsign registration and case normalization."""

    def test_X_normalizes_case(self):
        """Callsign registration normalizes to uppercase."""
        proto, transport, bridge = make_real_protocol()
        proto.data_received(make_frame(0, ord('X'), b'w1abc'))
        assert 'W1ABC' in proto.registered_calls
        assert 'w1abc' not in proto.registered_calls

    def test_X_duplicate_from_second_client_rejected(self):
        """Second client cannot register a callsign already held by another client."""
        owner, _, bridge = make_real_protocol()
        owner.data_received(make_frame(0, ord('X'), b'W1ABC'))
        assert 'W1ABC' in owner.registered_calls

        intruder = AGWPEServerProtocol(bridge)
        intruder_transport = Mock()
        intruder_transport.is_closing.return_value = False
        intruder.connection_made(intruder_transport)

        intruder.data_received(make_frame(0, ord('X'), b'W1ABC'))
        # Intruder's registration must be rejected
        assert 'W1ABC' not in intruder.registered_calls
        resp = parse_frame(intruder_transport.write.call_args[0][0])
        assert resp['kind'] == 'X'
        assert resp['data'] == b'\x00'  # failure

    def test_X_same_client_idempotent(self):
        """Same client re-registering same callsign succeeds (idempotent)."""
        proto, transport, bridge = make_real_protocol()
        proto.data_received(make_frame(0, ord('X'), b'W1ABC'))
        transport.write.reset_mock()
        proto.data_received(make_frame(0, ord('X'), b'W1ABC'))
        resp = parse_frame(transport.write.call_args[0][0])
        assert resp['kind'] == 'X'
        assert resp['data'] == b'\x01'  # success

    def test_X_case_insensitive_duplicate_rejected(self):
        """Duplicate detection is case-insensitive."""
        owner, _, bridge = make_real_protocol()
        owner.data_received(make_frame(0, ord('X'), b'W1ABC'))

        intruder = AGWPEServerProtocol(bridge)
        intruder_transport = Mock()
        intruder_transport.is_closing.return_value = False
        intruder.connection_made(intruder_transport)

        intruder.data_received(make_frame(0, ord('X'), b'w1abc'))
        assert 'W1ABC' not in intruder.registered_calls
        resp = parse_frame(intruder_transport.write.call_args[0][0])
        assert resp['data'] == b'\x00'

    def test_x_unregister_normalizes_case(self):
        """Unregister normalizes case to match registration."""
        proto, transport, bridge = make_real_protocol()
        proto.data_received(make_frame(0, ord('X'), b'W1ABC'))
        assert 'W1ABC' in proto.registered_calls
        proto.data_received(make_frame(0, ord('x'), b'w1abc'))
        assert 'W1ABC' not in proto.registered_calls
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestCallsignRegistration -v`
Expected: Multiple failures (no normalization, no uniqueness check)

- [ ] **Step 3: Implement registration changes**

In `tncd.py`, replace the `X` handler (around line 224):

```python
        elif datakind_bytes == b'X':
            call = from_str.upper()
            logger.debug(f"REGISTER: from={call!r}")
            # Reject if another client already registered this callsign
            for other in self.bridge.clients:
                if other is not self and call in other.registered_calls:
                    logger.warning(f"Rejecting duplicate registration of {call}")
                    self.send_frame(port, ord(b'X'), call_from, b'', b'\x00')
                    return
            self.registered_calls.add(call)
            # pe reads CallFrom from the response to record registered callsign.
            # data[0] != 0 means success.  Echo the port from the request.
            self.send_frame(port, ord(b'X'), call_from, b'', b'\x01')
```

Replace the `x` handler (around line 231):

```python
        elif datakind_bytes == b'x':
            # Unregister callsign: per spec, no response is sent.
            call = from_str.upper()
            logger.debug(f"UNREGISTER: from={call!r}")
            self.registered_calls.discard(call)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestCallsignRegistration -v`
Expected: PASS

- [ ] **Step 5: Run full test suite to check for regressions**

Run: `pytest tests/test_tncd.py -v`
Expected: All pass. Existing tests that register callsigns use uppercase already, so normalization won't break them.

- [ ] **Step 6: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: enforce unique callsign registration with case normalization"
```

---

### Task 7: Scope incoming SABM notification to owner only

**Files:**
- Modify: `tests/test_tncd.py` (add TestIncomingSABMScoped class)
- Modify: `tncd.py:1731-1745` (_dispatch_sabm notification loop)

- [ ] **Step 1: Write failing tests for scoped notification**

Add to `tests/test_tncd.py`:

```python
class TestIncomingSABMScoped:
    """Security audit: incoming SABM notification goes only to the registered owner."""

    def test_incoming_sabm_notifies_only_owner(self):
        """Only the registered owner receives the incoming C notification."""
        owner, owner_transport, bridge = make_real_protocol()
        owner.data_received(make_frame(0, ord('X'), b'W1ABC'))

        bystander = AGWPEServerProtocol(bridge)
        bystander_transport = Mock()
        bystander_transport.is_closing.return_value = False
        bystander.connection_made(bystander_transport)

        # Simulate incoming SABM from remote W2DEF to our registered W1ABC
        sabm = ax25.Frame(
            dst=ax25.Address('W1ABC'),
            src=ax25.Address('W2DEF'),
            control=ax25.Control(ax25.FrameType.SABM, poll_final=True),
        )
        owner_transport.write.reset_mock()
        bystander_transport.write.reset_mock()
        bridge.on_kiss_frame(0, bytes(sabm))

        # Owner must receive the C notification
        owner_calls = [parse_frame(c[0][0]) for c in owner_transport.write.call_args_list]
        c_frames = [f for f in owner_calls if f['kind'] == 'C']
        assert len(c_frames) == 1
        assert b'CONNECTED To Station W2DEF' in c_frames[0]['data']

        # Bystander must NOT receive the C notification
        if bystander_transport.write.called:
            bystander_calls = [parse_frame(c[0][0]) for c in bystander_transport.write.call_args_list]
            c_frames_bystander = [f for f in bystander_calls if f['kind'] == 'C']
            assert len(c_frames_bystander) == 0

    def test_incoming_sabm_no_owner_no_notification(self):
        """If no client registered the callsign, no C notification is sent."""
        proto, transport, bridge = make_real_protocol()
        # Don't register W1ABC

        sabm = ax25.Frame(
            dst=ax25.Address('W1ABC'),
            src=ax25.Address('W2DEF'),
            control=ax25.Control(ax25.FrameType.SABM, poll_final=True),
        )
        transport.write.reset_mock()
        bridge.on_kiss_frame(0, bytes(sabm))

        if transport.write.called:
            calls = [parse_frame(c[0][0]) for c in transport.write.call_args_list]
            c_frames = [f for f in calls if f['kind'] == 'C']
            assert len(c_frames) == 0
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestIncomingSABMScoped -v`
Expected: `test_incoming_sabm_notifies_only_owner` FAILS (bystander receives notification too)

- [ ] **Step 3: Scope _dispatch_sabm notification to owner only**

In `tncd.py`, replace the notification loop in `_dispatch_sabm` (around lines 1740-1745). Change:

```python
        msg = f'*** CONNECTED To Station {src}\r'.encode()
        for client in self.clients:
            try:
                client.send_frame(port, ord('C'), src.encode(), dst.encode(), msg)
            except Exception as e:
                logger.error(f"Error sending 'C' notification: {e}")
```

To:

```python
        msg = f'*** CONNECTED To Station {src}\r'.encode()
        if conn.owner:
            try:
                conn.owner.send_frame(port, ord('C'), src.encode(), dst.encode(), msg)
            except Exception as e:
                logger.error(f"Error sending 'C' notification: {e}")
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestIncomingSABMScoped -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `pytest tests/test_tncd.py -v`
Expected: All pass. Check that existing `incoming_sabm_sets_owner_for_registered_client` still passes.

- [ ] **Step 6: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: scope incoming SABM notification to registered owner only"
```

---

### Task 8: Client disconnect cleans up owned connections

**Files:**
- Modify: `tests/test_tncd.py` (add TestClientCleanup class)
- Modify: `tncd.py:1052-1054` (remove_client method)

- [ ] **Step 1: Write failing tests for cleanup**

Add to `tests/test_tncd.py`:

```python
class TestClientCleanup:
    """Security audit: client disconnect must clean up owned connections."""

    def test_remove_client_sends_disc_and_removes_connection(self):
        """When a client disconnects, owned connections are torn down with DISC."""
        owner, _, bridge = make_real_protocol()
        conn = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn.state = 'CONNECTED'
        conn.owner = owner

        bridge.kiss_client.send.reset_mock()
        bridge.remove_client(owner)

        # DISC should have been sent
        assert bridge.kiss_client.send.called
        frame = ax25.Frame.unpack(bridge.kiss_client.send.call_args[0][0])
        assert frame.control.frame_type is ax25.FrameType.DISC

        # Connection should be removed
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None

    def test_remove_client_clears_registered_calls(self):
        """Client disconnect frees registered callsigns for other clients."""
        owner, _, bridge = make_real_protocol()
        owner.data_received(make_frame(0, ord('X'), b'W1ABC'))
        assert 'W1ABC' in owner.registered_calls

        bridge.remove_client(owner)

        # A new client should now be able to register the same callsign
        new_client = AGWPEServerProtocol(bridge)
        new_transport = Mock()
        new_transport.is_closing.return_value = False
        new_client.connection_made(new_transport)
        new_client.data_received(make_frame(0, ord('X'), b'W1ABC'))
        assert 'W1ABC' in new_client.registered_calls

    def test_remove_client_does_not_affect_other_owners(self):
        """Removing one client does not disturb another client's connections."""
        owner1, _, bridge = make_real_protocol()
        conn1 = bridge.get_or_create_connection(0, 'W1ABC', 'W2DEF')
        conn1.state = 'CONNECTED'
        conn1.owner = owner1

        owner2 = AGWPEServerProtocol(bridge)
        t2 = Mock()
        t2.is_closing.return_value = False
        owner2.connection_made(t2)
        conn2 = bridge.get_or_create_connection(0, 'W3GHI', 'W4JKL')
        conn2.state = 'CONNECTED'
        conn2.owner = owner2

        bridge.remove_client(owner1)
        # owner1's connection removed, owner2's untouched
        assert bridge.get_connection(0, 'W1ABC', 'W2DEF') is None
        assert bridge.get_connection(0, 'W3GHI', 'W4JKL') is conn2
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestClientCleanup -v`
Expected: FAIL (connections not cleaned up, callsigns not freed)

- [ ] **Step 3: Implement remove_client cleanup**

In `tncd.py`, replace `remove_client` (around line 1052):

```python
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestClientCleanup -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `pytest tests/test_tncd.py -v`
Expected: All pass.

- [ ] **Step 6: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: clean up owned connections and registrations on client disconnect"
```

---

### Task 9: AGWPE client limit (max_clients)

**Files:**
- Modify: `tests/test_tncd.py` (add TestClientLimits class)
- Modify: `tncd.py:937-942` (Bridge.__init__) and `tncd.py:117-130` (AGWPEServerProtocol)

- [ ] **Step 1: Write failing tests for client cap**

Add to `tests/test_tncd.py`:

```python
class TestClientLimits:
    """Security audit: configurable AGWPE client limits."""

    def test_client_cap_rejects_excess_connections(self):
        """New clients beyond max_clients are closed immediately."""
        raw = configparser.ConfigParser()
        raw['server'] = {
            'listen_host': '0.0.0.0', 'listen_port': '8000',
            'callsign': 'N0CALL', 'max_clients': '2',
        }
        raw['client.0'] = {'type': 'serial', 'device': '/dev/null',
                           'serial_baudrate': '9600', 'ota_baudrate': '1200'}
        raw['kiss.0'] = {'tx_delay': '40', 'persistence': '63',
                         'slot_time': '20', 'tx_tail': '30'}
        config = PortConfig(raw, ['client.0'], {0: 'kiss.0'})
        bridge = Bridge(config)
        mock_kc = Mock()
        mock_kc.online = True
        bridge.kiss_clients[0] = mock_kc
        bridge.kiss_client = mock_kc

        # Connect 2 clients (at cap)
        for _ in range(2):
            proto = AGWPEServerProtocol(bridge)
            t = Mock()
            t.is_closing.return_value = False
            proto.connection_made(t)
        assert len(bridge.clients) == 2

        # Third client should be rejected
        rejected_proto = AGWPEServerProtocol(bridge)
        rejected_transport = Mock()
        rejected_transport.is_closing.return_value = False
        rejected_proto.connection_made(rejected_transport)
        rejected_transport.close.assert_called_once()
        assert len(bridge.clients) == 2

    def test_default_max_clients(self):
        """Default max_clients is 8."""
        _, _, bridge = make_real_protocol()
        assert bridge.max_clients == 8
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestClientLimits -v`
Expected: FAIL (no max_clients attribute, no cap enforcement)

- [ ] **Step 3: Implement max_clients**

In `tncd.py`, add to `Bridge.__init__` (after line 942, after `self.verbose = verbose`):

```python
        self.max_clients = config.getint('server', 'max_clients', fallback=8)
```

In `tncd.py`, modify `AGWPEServerProtocol.connection_made` (around line 127):

```python
    def connection_made(self, transport):
        if len(self.bridge.clients) >= self.bridge.max_clients:
            logger.warning(f"Client limit ({self.bridge.max_clients}) reached, "
                           f"rejecting {transport.get_extra_info('peername')}")
            transport.close()
            return
        self.transport = transport
        logger.info(f"AGWPE client connected from {transport.get_extra_info('peername')}")
        self.bridge.add_client(self)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestClientLimits -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `pytest tests/test_tncd.py -v`
Expected: All pass.

- [ ] **Step 6: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: add configurable AGWPE client limit (max_clients)"
```

---

### Task 10: Idle timeout

**Files:**
- Modify: `tests/test_tncd.py` (add TestIdleTimeout class)
- Modify: `tncd.py` (Bridge.__init__, Bridge.start, AGWPEServerProtocol)

- [ ] **Step 1: Write failing tests for idle timeout**

Add to `tests/test_tncd.py`:

```python
class TestIdleTimeout:
    """Security audit: idle AGWPE clients are disconnected."""

    async def test_idle_client_closed(self):
        """Client with no activity past idle_timeout is closed."""
        raw = configparser.ConfigParser()
        raw['server'] = {
            'listen_host': '0.0.0.0', 'listen_port': '8000',
            'callsign': 'N0CALL', 'idle_timeout': '10',
        }
        raw['client.0'] = {'type': 'serial', 'device': '/dev/null',
                           'serial_baudrate': '9600', 'ota_baudrate': '1200'}
        raw['kiss.0'] = {'tx_delay': '40', 'persistence': '63',
                         'slot_time': '20', 'tx_tail': '30'}
        config = PortConfig(raw, ['client.0'], {0: 'kiss.0'})
        bridge = Bridge(config)
        mock_kc = Mock()
        mock_kc.online = True
        bridge.kiss_clients[0] = mock_kc
        bridge.kiss_client = mock_kc

        proto = AGWPEServerProtocol(bridge)
        transport = Mock()
        transport.is_closing.return_value = False
        proto.connection_made(transport)

        # Simulate stale last_activity
        proto.last_activity = time.monotonic() - 20
        bridge._sweep_idle_clients()
        transport.close.assert_called_once()

    async def test_active_client_not_closed(self):
        """Client with recent activity is not closed."""
        _, _, bridge = make_real_protocol()
        proto = bridge.clients[0]
        proto.last_activity = time.monotonic()
        transport = proto.transport
        bridge._sweep_idle_clients()
        transport.close.assert_not_called()

    def test_default_idle_timeout(self):
        """Default idle_timeout is 300."""
        _, _, bridge = make_real_protocol()
        assert bridge.idle_timeout == 300
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `pytest tests/test_tncd.py::TestIdleTimeout -v`
Expected: FAIL (no idle_timeout attribute, no sweep method)

- [ ] **Step 3: Implement idle timeout**

In `tncd.py`, add to `Bridge.__init__` (after the `max_clients` line):

```python
        self.idle_timeout = config.getint('server', 'idle_timeout', fallback=300)
```

Add to `Bridge` class (after `remove_client`):

```python
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
```

In `Bridge.start()`, after the `server` is created (around line 1041), schedule the sweep:

```python
        logger.info("Bridge running. Press Ctrl+C to stop.")
        loop.call_later(30, self._sweep_idle_clients)
```

In `AGWPEServerProtocol.__init__`, add:

```python
        self.last_activity = time.monotonic()
```

In `AGWPEServerProtocol.data_received`, at the top of the method (after `self.buffer += data`), add:

```python
        self.last_activity = time.monotonic()
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `pytest tests/test_tncd.py::TestIdleTimeout -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `pytest tests/test_tncd.py -v`
Expected: All pass.

- [ ] **Step 6: Commit**

```bash
git add tests/test_tncd.py tncd.py
git commit -m "security: add configurable idle timeout for AGWPE clients"
```

---

### Task 11: Retire tncd-rfcomm standalone service

**Files:**
- Delete: `tncd-rfcomm`
- Delete: `tncd-rfcomm.service`
- Modify: `packaging/PKGBUILD` (remove rfcomm lines)
- Modify: `packaging/tncd.spec` (remove rfcomm lines)
- Modify: `packaging/gentoo-overlay/net-misc/tncd/tncd-0.11.1_beta.ebuild` (remove rfcomm lines)
- Modify: `packaging/gentoo-overlay/net-misc/tncd/tncd-0.11_beta.ebuild` (remove rfcomm lines)
- Modify: `nix/default.nix` (remove rfcomm install line)
- Modify: `nix/overlay.nix` (remove rfcomm install line)
- Modify: `debian/rules` (remove rfcomm lines)
- Modify: `.github/workflows/release.yml` (remove rfcomm lines)
- Modify: `pyproject.toml` (remove script-files entry)
- Modify: `PLAN.md` (update milestones)
- Modify: `TESTING.md` (remove rfcomm reference if present)

- [ ] **Step 1: Delete tncd-rfcomm and tncd-rfcomm.service**

```bash
rm tncd-rfcomm tncd-rfcomm.service
```

- [ ] **Step 2: Remove rfcomm from packaging/PKGBUILD**

Remove lines 16, 24, 26-28 (the `install -Dm755 tncd-rfcomm` line, the `install -Dm644 tncd-rfcomm.service` line, and the sed fixup lines for rfcomm). Keep all other install lines.

- [ ] **Step 3: Remove rfcomm from packaging/tncd.spec**

Remove line 35 (`install -Dm755 tncd-rfcomm`), lines 48-50 (sed fixup for rfcomm.service), update `%systemd_post`/`%systemd_preun`/`%systemd_postun_with_restart` to reference only `tncd.service`, and remove lines 64 and 67 (`%{_bindir}/tncd-rfcomm` and `%{_unitdir}/tncd-rfcomm.service`).

- [ ] **Step 4: Remove rfcomm from both Gentoo ebuilds**

In both `tncd-0.11.1_beta.ebuild` and `tncd-0.11_beta.ebuild`:
- Remove `tncd-rfcomm` from the `python_fix_shebang` line (keep `tncd.py`)
- Remove the `newexe tncd-rfcomm tncd-rfcomm` line
- Remove the `systemd_dounit tncd-rfcomm.service` line and its sed fixup lines

- [ ] **Step 5: Remove rfcomm from nix/default.nix and nix/overlay.nix**

In `nix/default.nix`: remove line 27 (`install -Dm755 tncd-rfcomm  $out/bin/tncd-rfcomm`).
In `nix/overlay.nix`: remove line 23 (`install -Dm755 tncd-rfcomm  $out/bin/tncd-rfcomm`).

- [ ] **Step 6: Remove rfcomm from debian/rules**

Remove lines 10, 18, and 20-22 (the install and sed lines for tncd-rfcomm).

- [ ] **Step 7: Remove rfcomm from .github/workflows/release.yml**

Remove lines 95-104 (rfcomm manager copy and wrapper script) and lines 115-117 (rfcomm service sed fixup).

- [ ] **Step 8: Remove rfcomm from pyproject.toml**

Remove lines 26-29 (the `script-files` comment and entry).

- [ ] **Step 9: Update PLAN.md**

Mark Milestone 2 as retired. Change "COMPLETE" to "RETIRED — replaced by in-process Bluetooth D-Bus support" and remove or strike through the `tncd-rfcomm` bullet points. Update Milestone 4 line 74 to note rfcomm helper has been removed.

- [ ] **Step 10: Run full test suite to check for regressions**

Run: `pytest tests/test_tncd.py -v`
Expected: All pass. The test file imports `BluetoothKISS` which still exists in tncd.py, so no import breakage.

- [ ] **Step 11: Commit**

```bash
git add -A
git commit -m "chore: retire tncd-rfcomm standalone service (replaced by in-process BT)"
```

---

### Task 12: Final integration test run

**Files:** None (verification only)

- [ ] **Step 1: Run full test suite**

Run: `pytest -v`
Expected: All tests pass.

- [ ] **Step 2: Run e2e tests if available**

Run: `pytest tests/test_e2e.py -v` (skip if e2e tests require hardware)

- [ ] **Step 3: Verify no regressions in existing connected-mode tests**

Run: `pytest tests/test_tncd.py::TestConnectedMode tests/test_tncd.py::TestConnectedModeReceivePath -v`
Expected: All pass.
