# Security Audit Fixes — Design Spec

Date: 2026-05-20

## Goal

Address all findings from SECURITY-AUDIT.md and close the corresponding items in PROTOCOL-AUDIT-CHECKLIST.md. Retire the deprecated `tncd-rfcomm` standalone service. The in-process Bluetooth D-Bus support in `tncd.py` is unaffected.

## 1. Owner enforcement on existing-session operations

### D, d, K, Y handlers

Add `conn.owner is not self` guard. Non-owner requests are logged at WARNING and silently dropped (AGWPE has no error response frame for these types).

```python
# In D handler, after fetching conn:
if conn.owner is not self:
    logger.warning("Rejecting non-owner 'D' for %s->%s", from_str, to_str)
    return
```

Same pattern for `d`, `K` (check whether a connection exists for the callsign pair; if it does and caller isn't owner, reject), and `Y` (return 0 for non-owner queries rather than leaking queue depth).

### C, c, v handlers

If `get_or_create_connection` returns an existing connection in `CONNECTED` state and `conn.owner is not self`, respond with `*** BUSY` and return without disturbing the existing session:

```python
conn = self.bridge.get_or_create_connection(port, from_str, to_str)
if not conn:
    # ... existing BUSY for connection limit
elif conn.state == 'CONNECTED' and conn.owner is not self:
    busy_msg = f'*** BUSY From {from_str}\r'.encode()
    self.send_frame(port, ord('d'), call_from, call_to, busy_msg)
    return
```

Addresses: Findings 1 and 4.

## 2. Callsign registration uniqueness and case normalization

### X handler

- Normalize callsign to uppercase before storing: `from_str.upper()`.
- Check whether any other client has already registered the same callsign. If so, reject with data byte `\x00` (failure).
- Same client re-registering the same callsign is idempotent (success, no-op).

```python
call = from_str.upper()
for other in self.bridge.clients:
    if other is not self and call in other.registered_calls:
        self.send_frame(port, ord(b'X'), call_from, b'', b'\x00')
        return
self.registered_calls.add(call)
self.send_frame(port, ord(b'X'), call_from, b'', b'\x01')
```

### x handler

Normalize to uppercase before discarding: `self.registered_calls.discard(from_str.upper())`.

### _dispatch_sabm — scoped notifications

Send incoming `C` notification only to `conn.owner`, not to all clients:

```python
if conn.owner:
    conn.owner.send_frame(port, ord('C'), src.encode(), dst.encode(), msg)
```

Addresses: Findings 3 and 6.

## 3. Client lifecycle cleanup

### remove_client

When an AGWPE client's TCP connection drops, iterate `self.connections` for connections owned by the departing client. For each:

1. Send DISC over the air (graceful teardown with remote station).
2. Remove the connection from `self.connections`.
3. Cancel T1/T2/T3 timers.

No `*** DISCONNECTED` notification is needed since the owning client's transport is already gone.

Also clear the client's registered callsigns so those callsigns become available for other clients.

```python
def remove_client(self, client):
    if client in self.clients:
        self.clients.remove(client)
    # Clean up owned connections
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
```

Addresses: Finding 5.

## 4. AGWPE client limits and idle timeout

### max_clients

Configurable via `[server] max_clients` (default 8). Enforced in `connection_made`:

```python
def connection_made(self, transport):
    if len(self.bridge.clients) >= self.bridge.max_clients:
        logger.warning("Client limit reached, rejecting connection")
        transport.close()
        return
    # ... existing logic
```

### idle_timeout

Configurable via `[server] idle_timeout` (default 300 seconds, 0 = disabled). Each `AGWPEServerProtocol` tracks `self.last_activity` (monotonic time), updated in `data_received`. Bridge runs an asyncio periodic task (every 30s) that closes clients exceeding the timeout.

```python
# In Bridge.start(), after creating the server:
self._idle_sweep_handle = loop.call_later(30, self._sweep_idle_clients)

def _sweep_idle_clients(self):
    if self.idle_timeout <= 0:
        return
    now = time.monotonic()
    for client in list(self.clients):
        if now - client.last_activity > self.idle_timeout:
            logger.info("Closing idle AGWPE client")
            client.transport.close()
    loop = asyncio.get_running_loop()
    self._idle_sweep_handle = loop.call_later(30, self._sweep_idle_clients)
```

Addresses: Finding 2.

## 5. Retire tncd-rfcomm standalone service

### Files to delete

- `tncd-rfcomm` (standalone Python script)
- `tncd-rfcomm.service` (systemd unit)

### References to remove

Strip `tncd-rfcomm` and `tncd-rfcomm.service` references from:

- `packaging/PKGBUILD`
- `packaging/tncd.spec`
- `packaging/gentoo-overlay/net-misc/tncd/*.ebuild`
- `nix/default.nix`
- `nix/overlay.nix`
- `debian/rules`
- `.github/workflows/release.yml`
- `README.md`
- `TESTING.md`
- `PLAN.md`
- `website/` pages as applicable

The in-process Bluetooth D-Bus code in `tncd.py` (`BluetoothKISS`, `SPPProfile`, `_bt_*`) is unchanged.

Addresses: Hardening Notes — Deprecated root helper service.

## 6. K handler ownership check

The `K` (raw AX.25) handler currently sends raw bytes to KISS unconditionally. After this change, if a connection exists for the callsign pair in the frame and the sender is not the owner, the frame is rejected. If no connection exists (e.g., UI frame or unconnected raw send), the frame is allowed through — `K` is also used for raw UI and other non-session traffic.

## 7. Tests

All new behavior gets regression tests:

| Test | Validates |
|------|-----------|
| Non-owner `D` rejected | Finding 1 |
| Non-owner `d` rejected | Finding 1 |
| Non-owner `K` rejected (when connection exists) | Finding 1 |
| Non-owner `Y` returns 0 | Finding 1 |
| Non-owner `C` on active session gets BUSY | Finding 4 |
| Duplicate `X` from second client rejected | Finding 3 |
| Same-client `X` re-registration is idempotent | Finding 3 |
| Callsign case normalized on `X` | Finding 6 |
| Incoming SABM notification scoped to owner | Finding 3 |
| Client disconnect sends DISC and cleans up connections | Finding 5 |
| Orphaned session does not ACK after owner disconnect | Finding 5 |
| Client cap enforced (connection closed) | Finding 2 |
| Idle timeout closes silent clients | Finding 2 |

## Out of scope

- Systemd sandboxing for `tncd.service` (hardening note, not a vulnerability — separate task)
- AX.25 state machine scenario tests from PROTOCOL-AUDIT-CHECKLIST.md (separate effort)
- KISS channel demux and monitoring scope (separate effort)
- Documentation alignment (separate effort)
