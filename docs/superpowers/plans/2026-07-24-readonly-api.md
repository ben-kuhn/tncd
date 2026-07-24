# Read-only JSON/SSE API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only HTTP API (`/api/status`, `/api/connections`, SSE `/api/events`) exposing tncd's live state and traffic as JSON, built on the frontend subscriber bus.

**Architecture:** A stdlib `net/http` server in a new `internal/frontend/api` package. The subscriber bus gains TX-frame and connection-lifecycle events; the API registers as a sink and fans events to SSE clients. GET handlers snapshot loop-confined bridge/l2 state via `eng.Do`, then serialize off-loop. Disabled by default.

**Tech Stack:** Go stdlib only — `net/http`, `encoding/json`, `encoding/base64`, `net/http/httptest`. No new dependencies.

## Global Constraints

- Module path `github.com/ben-kuhn/tncd/v2`. `go` at `/nix/store/gb0njhqswlc5n127ikgyikvq39r40l6f-go-1.26.4/bin/go` if not on PATH. No gcc — use `CGO_ENABLED=0` for `go test`/`go vet`.
- Branch `feature/api` off `main`. Commit after each task. No version bump (a later release cuts the tag).
- **No new third-party dependencies** — stdlib only.
- Engine-loop discipline: all bridge/l2 state (`ports`, sinks, counters, `l2` conns) is touched only on the engine loop. Frontend goroutines marshal via `eng.Do`. Sink registration happens during setup (before `engine.Run`) or on the loop.
- **Read-only**: no write/control endpoints. **Disabled by default**; when enabled, bind `127.0.0.1:8002`, `max_clients=16`. No auth/TLS (deliberate — read-only + localhost + off-by-default is the security posture).
- SSE event tokens and the `/api/connections` `state` field are **lowercase** (`rx`/`tx`/`connect`/`disconnect`; `connected`/`connecting`).
- The AGWPE monitor (`MonitorSink`) path must stay byte-identical — do not modify it.

---

### Task 1: Bus extension — TX-frame + connection events

**Files:**
- Modify: `internal/bridge/events.go` (add sink interfaces, `ConnEvent`, registry, emitters)
- Modify: `internal/bridge/bridge.go` (`Bridge` struct fields; emit in `SendToKISS`, `notifyConnected`, `notifyDisconnected`)
- Test: `internal/bridge/events_test.go`

**Interfaces:**
- Produces:
  - `bridge.TxFrameSink interface { OnTXFrame(port int, f *ax25.Frame) }`
  - `bridge.ConnSink interface { OnConn(e ConnEvent) }`
  - `bridge.ConnEvent struct { Port int; Local, Remote string; State string; Incoming bool }` — `State` is `"connected"` or `"disconnected"`.
  - `(*Bridge) RegisterTxFrameSink(s TxFrameSink)` / `UnregisterTxFrameSink(s TxFrameSink)`
  - `(*Bridge) RegisterConnSink(s ConnSink)` / `UnregisterConnSink(s ConnSink)`
- Consumes: existing `ax25.Parse`, `Bridge.SendToKISS`, `notifyConnected(c *l2pkg.Conn, incoming bool)`, `notifyDisconnected(c *l2pkg.Conn)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/bridge/events_test.go`:

```go
type recTx struct{ ports []int; frames []*ax25.Frame }
func (r *recTx) OnTXFrame(port int, f *ax25.Frame) { r.ports = append(r.ports, port); r.frames = append(r.frames, f) }

type recConn struct{ evs []ConnEvent }
func (r *recConn) OnConn(e ConnEvent) { r.evs = append(r.evs, e) }

func TestTxAndConnSinkRegistration(t *testing.T) {
	b := &Bridge{}
	tx := &recTx{}
	cn := &recConn{}
	b.RegisterTxFrameSink(tx)
	b.RegisterConnSink(cn)
	b.emitTXFrame(1, &ax25.Frame{Type: ax25.UI})
	b.emitConn(ConnEvent{Port: 1, State: "connected"})
	if len(tx.ports) != 1 || tx.ports[0] != 1 {
		t.Fatalf("tx sink not called: %v", tx.ports)
	}
	if len(cn.evs) != 1 || cn.evs[0].State != "connected" {
		t.Fatalf("conn sink not called: %v", cn.evs)
	}
	b.UnregisterTxFrameSink(tx)
	b.UnregisterConnSink(cn)
	b.emitTXFrame(1, &ax25.Frame{})
	b.emitConn(ConnEvent{})
	if len(tx.ports) != 1 || len(cn.evs) != 1 {
		t.Fatalf("unregister failed: tx=%d conn=%d", len(tx.ports), len(cn.evs))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -run TxAndConnSink -v`
Expected: FAIL — the sink types, register methods, and `emit*` are undefined.

- [ ] **Step 3: Implement the bus additions**

In `internal/bridge/events.go`, add:

```go
// TxFrameSink receives decoded frames tncd transmits (all TX: L2, AGWPE, kisstcp).
type TxFrameSink interface {
	OnTXFrame(port int, f *ax25.Frame)
}

// ConnSink receives connection lifecycle events.
type ConnSink interface {
	OnConn(e ConnEvent)
}

// ConnEvent is a connection lifecycle change. State is "connected" or "disconnected".
type ConnEvent struct {
	Port          int
	Local, Remote string
	State         string
	Incoming      bool // meaningful for "connected"
}

func (b *Bridge) RegisterTxFrameSink(s TxFrameSink) { b.txSinks = append(b.txSinks, s) }
func (b *Bridge) RegisterConnSink(s ConnSink)       { b.connSinks = append(b.connSinks, s) }

func (b *Bridge) UnregisterTxFrameSink(s TxFrameSink) {
	for i, x := range b.txSinks {
		if x == s {
			b.txSinks = append(b.txSinks[:i], b.txSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) UnregisterConnSink(s ConnSink) {
	for i, x := range b.connSinks {
		if x == s {
			b.connSinks = append(b.connSinks[:i], b.connSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) emitTXFrame(port int, f *ax25.Frame) {
	for _, s := range b.txSinks {
		s.OnTXFrame(port, f)
	}
}

func (b *Bridge) emitConn(e ConnEvent) {
	for _, s := range b.connSinks {
		s.OnConn(e)
	}
}
```

Add fields to the `Bridge` struct in `internal/bridge/bridge.go` (next to `rawSinks`/`monitorSinks`):

```go
	txSinks   []TxFrameSink
	connSinks []ConnSink
```

- [ ] **Step 4: Run to verify registry passes**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -run TxAndConnSink -v` — Expected: PASS.

- [ ] **Step 5: Emit TX frames from `SendToKISS`**

In `internal/bridge/bridge.go` `SendToKISS`, after `p.Send(raw)` (the frame is now sent), emit a decoded copy to TX sinks. Parse the raw bytes (like `OnKISSFrame` does); skip emission on parse error:

```go
	p.Send(raw)

	// Emit a decoded copy to TX-frame sinks (API monitor). Our own TX is
	// normally well-formed; a parse failure just skips emission.
	if len(b.txSinks) > 0 {
		if f, err := ax25.Parse(raw); err == nil {
			b.emitTXFrame(port, f)
		}
	}
```

- [ ] **Step 6: Emit connection events**

In `notifyConnected` (add at the end of the function body):

```go
	b.emitConn(ConnEvent{Port: c.Port, Local: c.Local, Remote: c.Remote, State: "connected", Incoming: incoming})
```

In `notifyDisconnected` (add at the end):

```go
	b.emitConn(ConnEvent{Port: c.Port, Local: c.Local, Remote: c.Remote, State: "disconnected"})
```

- [ ] **Step 7: Add TX + conn emission tests**

Add to `internal/bridge/events_test.go` (uses the bridge test helpers `makeBridge`, `newFakePort`, `onLoop`, `makeUIFrame` already in `bridge_test.go`):

```go
func TestSendToKISSEmitsTXFrame(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	tx := &recTx{}
	var b *Bridge
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
		b.RegisterTxFrameSink(tx)
		b.SendToKISS(0, makeUIFrame("KU0HN", "CQ", []byte("hi")))
	})
	if len(tx.frames) != 1 || tx.frames[0].Type != ax25.UI || tx.frames[0].Src.String() != "KU0HN" {
		t.Fatalf("TX frame not emitted correctly: %+v", tx.frames)
	}
}
```

- [ ] **Step 8: Run full bridge suite**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -v` — Expected: PASS (new tests + all existing).

- [ ] **Step 9: Commit**

```bash
git add internal/bridge/events.go internal/bridge/events_test.go internal/bridge/bridge.go
git commit -m "feat(bridge): bus TX-frame + connection-lifecycle events"
```

---

### Task 2: l2 connection snapshot + `incoming` field

**Files:**
- Modify: `ax25/l2/conn.go` (add `incoming` field)
- Modify: `ax25/l2/l2.go` (set `incoming` at connect; add `ConnInfo` + `Table.Snapshot`)
- Test: `ax25/l2/l2_test.go` (or a new `ax25/l2/snapshot_test.go`)

**Interfaces:**
- Produces:
  - `l2.ConnInfo` struct (fields + json tags below)
  - `(*l2.Table) Snapshot() []ConnInfo` — active (Connecting/Connected) connections only; called on the engine loop.
- Consumes: `Conn` fields (`Port`, `Local`, `Remote`, `Via`, `State`, `modulo`, `sendSeq`, `recvSeq`, `unacked`, `outQueue`, `t1Polls`, `remoteBusy`, `srtt`, `srejEnabled`), `Table.conns` map, `ConnState.String()`.

- [ ] **Step 1: Write the failing test**

Add `ax25/l2/snapshot_test.go`:

```go
package l2

import (
	"testing"
	"time"
)

func TestSnapshotActiveOnly(t *testing.T) {
	tbl := &Table{conns: map[connKey]*Conn{}}
	// One connected, one disconnected.
	c := newConn(0, "KU0HN-10", "W0NE-10")
	c.State = Connected
	c.incoming = true
	c.modulo = 128
	c.sendSeq = 5
	c.recvSeq = 3
	c.unacked = 2
	c.outQueue = []outEntry{{pid: 0xF0, data: []byte{1}}}
	c.t1Polls = 1
	c.remoteBusy = true
	c.srtt = 1200 * time.Millisecond
	c.srejEnabled = true
	c.Via = []string{"W0NE-7"}
	tbl.conns[makeKey(0, "KU0HN-10", "W0NE-10")] = c

	d := newConn(0, "AAA", "BBB") // Disconnected (default)
	tbl.conns[makeKey(0, "AAA", "BBB")] = d

	snap := tbl.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1 (active only)", len(snap))
	}
	got := snap[0]
	if got.Local != "KU0HN-10" || got.Remote != "W0NE-10" || got.State != "connected" ||
		!got.Incoming || got.Modulo != 128 || got.SendSeq != 5 || got.RecvSeq != 3 ||
		got.Unacked != 2 || got.SendQueue != 1 || got.T1Retries != 1 || !got.RemoteBusy ||
		got.SRTTms != 1200 || !got.SREJ || len(got.Via) != 1 || got.Via[0] != "W0NE-7" {
		t.Fatalf("snapshot fields wrong: %+v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./ax25/l2/ -run SnapshotActiveOnly -v`
Expected: FAIL — `Conn.incoming` and `Table.Snapshot`/`ConnInfo` undefined.

- [ ] **Step 3: Add the `incoming` field and set it at connect**

In `ax25/l2/conn.go`, add to the `Conn` struct (near `State`):

```go
	incoming bool // true if this connection was remote-initiated
```

In `ax25/l2/l2.go`, set it with a single uniform rule: **immediately before every
`t.hooks.Connected(c, X)` call, add `c.incoming = X`.** `grep -n "hooks.Connected(" ax25/l2/l2.go`
finds all four sites (dispatchSABM ~883 `true`, dispatchSABME ~931 `true` and ~952 `false`,
and the outgoing UA path ~1091 `false`). This makes `c.incoming` reflect the direction the
hook was told, with no site-specific logic. (`newConn` zero-initializes `incoming = false`,
so a Connecting-state outgoing conn already reports `false` before the hook fires.)

- [ ] **Step 4: Add `ConnInfo` + `Snapshot`**

In `ax25/l2/l2.go` (or a new `ax25/l2/snapshot.go`):

```go
// ConnInfo is a read-only snapshot of one active connection, for the API.
type ConnInfo struct {
	Port       int      `json:"port"`
	Local      string   `json:"local"`
	Remote     string   `json:"remote"`
	Via        []string `json:"via"`
	State      string   `json:"state"` // lowercase: "connected" / "connecting"
	Incoming   bool     `json:"incoming"`
	Modulo     uint8    `json:"modulo"`
	SendSeq    uint8    `json:"send_seq"`
	RecvSeq    uint8    `json:"recv_seq"`
	Unacked    uint8    `json:"unacked"`
	SendQueue  int      `json:"send_queue"`
	T1Retries  int      `json:"t1_retries"`
	RemoteBusy bool     `json:"remote_busy"`
	SRTTms     int64    `json:"srtt_ms"`
	SREJ       bool     `json:"srej"`
}

// Snapshot returns info for every active (Connecting/Connected) connection.
// Must be called on the engine loop.
func (t *Table) Snapshot() []ConnInfo {
	out := make([]ConnInfo, 0, len(t.conns))
	for _, c := range t.conns {
		if c.State != Connecting && c.State != Connected {
			continue
		}
		via := append([]string(nil), c.Via...)
		out = append(out, ConnInfo{
			Port: c.Port, Local: c.Local, Remote: c.Remote, Via: via,
			State:    strings.ToLower(c.State.String()),
			Incoming: c.incoming, Modulo: c.modulo,
			SendSeq: c.sendSeq, RecvSeq: c.recvSeq, Unacked: c.unacked,
			SendQueue: len(c.outQueue), T1Retries: c.t1Polls,
			RemoteBusy: c.remoteBusy, SRTTms: c.srtt.Milliseconds(), SREJ: c.srejEnabled,
		})
	}
	return out
}
```

Add `"strings"` to the imports of whichever file holds `Snapshot`.

- [ ] **Step 5: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./ax25/l2/ -run SnapshotActiveOnly -v` — Expected: PASS. Then the full l2 suite: `CGO_ENABLED=0 go test ./ax25/l2/ -v` (the `incoming` sets must not break existing connect tests).

- [ ] **Step 6: Commit**

```bash
git add ax25/l2/conn.go ax25/l2/l2.go ax25/l2/snapshot.go ax25/l2/snapshot_test.go
git commit -m "feat(l2): ConnInfo snapshot of active connections; Conn.incoming"
```
(Adjust `git add` paths to where you actually put `Snapshot`/`ConnInfo`.)

---

### Task 3: Bridge per-port counters + status/connections snapshot

**Files:**
- Modify: `internal/bridge/bridge.go` (counter fields; increment in `OnKISSFrame`/`SendToKISS`; reset in `portWentOffline`; add `PortStatus`, `StatusPorts()`, `ConnectionSnapshot()`)
- Test: `internal/bridge/bridge_test.go`

**Interfaces:**
- Produces:
  - `bridge.PortStatus struct { Port int; Name, Type string; Online bool; RxFrames, TxFrames uint64 }` (json tags below)
  - `(*Bridge) StatusPorts() []PortStatus` — per-port snapshot; called on the loop.
  - `(*Bridge) ConnectionSnapshot() []l2.ConnInfo` — delegates to `b.l2.Snapshot()`; called on the loop.
- Consumes: `b.ports`, `b.cfg.Ports`, `PortOnline`, `l2.Table.Snapshot`.

- [ ] **Step 1: Write the failing test**

Add to `internal/bridge/bridge_test.go`:

```go
func TestStatusPortsCounters(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	var ports []PortStatus
	onLoop(t, eng, func() {
		b := makeBridge(t, eng, fp)
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: makeUIFrame("A", "B", []byte("hi"))})
		b.SendToKISS(0, makeUIFrame("A", "B", []byte("hi")))
		ports = b.StatusPorts()
	})
	if len(ports) != 1 {
		t.Fatalf("ports len = %d, want 1", len(ports))
	}
	if ports[0].RxFrames != 1 || ports[0].TxFrames != 1 || !ports[0].Online {
		t.Fatalf("counters wrong: %+v", ports[0])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -run StatusPortsCounters -v`
Expected: FAIL — `PortStatus`/`StatusPorts` undefined.

- [ ] **Step 3: Add counters + snapshot accessors**

Add fields to the `Bridge` struct:

```go
	rxFrames []uint64 // per-port, live-scoped (reset on offline)
	txFrames []uint64
```

These must be sized to the number of ports. Size them where `b.ports` is populated (in `InjectPorts` and in `Start`/`buildPorts` — grep `b.ports =`): after `b.ports` is assigned, add `b.rxFrames = make([]uint64, len(b.ports)); b.txFrames = make([]uint64, len(b.ports))`. Guard increments against a short/nil slice.

In `OnKISSFrame`, after the echo-suppression early return passes (i.e. the frame is a real RX we keep), before or after emit — increment rx:

```go
	if f.Port >= 0 && f.Port < len(b.rxFrames) {
		b.rxFrames[f.Port]++
	}
```

In `SendToKISS`, after `p.Send(raw)`:

```go
	if port >= 0 && port < len(b.txFrames) {
		b.txFrames[port]++
	}
```

In `portWentOffline`, reset that port's counters (live-scoped):

```go
	if portNum >= 0 && portNum < len(b.rxFrames) {
		b.rxFrames[portNum] = 0
		b.txFrames[portNum] = 0
	}
```

Add the snapshot accessors:

```go
// PortStatus is a read-only per-port snapshot for /api/status.
type PortStatus struct {
	Port     int    `json:"port"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Online   bool   `json:"online"`
	RxFrames uint64 `json:"rx_frames"`
	TxFrames uint64 `json:"tx_frames"`
}

// StatusPorts returns a per-port snapshot. Must be called on the engine loop.
func (b *Bridge) StatusPorts() []PortStatus {
	out := make([]PortStatus, 0, len(b.ports))
	for i := range b.ports {
		ps := PortStatus{Port: i, Online: b.PortOnline(i)}
		if i < len(b.cfg.Ports) {
			ps.Name = b.cfg.Ports[i].Name
			ps.Type = b.cfg.Ports[i].Type
		}
		if i < len(b.rxFrames) {
			ps.RxFrames = b.rxFrames[i]
			ps.TxFrames = b.txFrames[i]
		}
		out = append(out, ps)
	}
	return out
}

// ConnectionSnapshot returns active AX.25 connections. Must be called on the loop.
func (b *Bridge) ConnectionSnapshot() []l2pkg.ConnInfo { return b.l2.Snapshot() }
```

(`l2pkg` is the existing import alias for `github.com/ben-kuhn/tncd/v2/ax25/l2` in bridge.go.)

- [ ] **Step 4: Verify + guard `makeBridge`**

`makeBridge` sets `b.ports = []PortSender{fp}` directly, so it bypasses the sizing site. In `makeBridge` (bridge_test.go), after `b.ports = []PortSender{fp}`, add `b.rxFrames = make([]uint64, 1); b.txFrames = make([]uint64, 1)` so counter tests work. Run: `CGO_ENABLED=0 go test ./internal/bridge/ -run StatusPortsCounters -v` — Expected: PASS.

- [ ] **Step 5: Run full bridge suite**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -v` — Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/bridge/bridge.go internal/bridge/bridge_test.go
git commit -m "feat(bridge): per-port frame counters + status/connections snapshot"
```

---

### Task 4: `[api]` config section

**Files:**
- Modify: `internal/config/config.go`, `internal/config/example.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.APIConfig struct { Enabled bool; ListenHost string; ListenPort int; MaxClients int }`; `Config.API APIConfig`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestAPISectionParsed(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n\n[api]\nenabled=true\nlisten_port=9002\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.API.Enabled || cfg.API.ListenPort != 9002 || cfg.API.ListenHost != "127.0.0.1" || cfg.API.MaxClients != 16 {
		t.Fatalf("API cfg wrong: %+v", cfg.API)
	}
}

func TestAPIAbsentDisabled(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n"), 0o644)
	cfg, _ := Load(path)
	if cfg.API.Enabled {
		t.Fatal("API should default disabled")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/config/ -run API -v` — Expected: FAIL (`cfg.API` undefined).

- [ ] **Step 3: Implement**

In `internal/config/config.go` add the struct (near `KISSTCP`):

```go
// APIConfig holds the [api] section: a read-only HTTP monitoring API.
type APIConfig struct {
	Enabled    bool   // default false
	ListenHost string // default "127.0.0.1"
	ListenPort int    // default 8002
	MaxClients int    // default 16
}
```

Add to `Config`: `API APIConfig`. Add known keys:

```go
var knownAPIKeys = []string{"enabled", "listen_host", "listen_port", "max_clients"}
```

In `Load`, after the `[kisstcp]` parse block:

```go
	apiSec := f.Section("api")
	warnUnknownKeys(apiSec, knownAPIKeys)
	cfg.API = APIConfig{
		Enabled:    getBool(apiSec, "enabled", false),
		ListenHost: getString(apiSec, "listen_host", "127.0.0.1"),
		ListenPort: getInt(apiSec, "listen_port", 8002),
		MaxClients: getInt(apiSec, "max_clients", 16),
	}
```

- [ ] **Step 4: Verify + example block**

Run: `CGO_ENABLED=0 go test ./internal/config/ -run API -v` — Expected: PASS.
Add a commented `[api]` block to `internal/config/example.go` (all lines `#`-prefixed; `TestExampleLoads` still passes since comments don't affect parsing):

```
# [api]
# Read-only HTTP monitoring API (JSON + Server-Sent Events). Disabled by default.
# UNAUTHENTICATED — enable only on a trusted host; it exposes traffic metadata.
# enabled = false
# listen_host = 127.0.0.1
# listen_port = 8002
# max_clients = 16
```

Run: `CGO_ENABLED=0 go test ./internal/config/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/config/example.go
git commit -m "feat(config): [api] read-only API section (disabled by default)"
```

---

### Task 5: API server — encoding + handlers + SSE

**Files:**
- Create: `internal/frontend/api/encode.go` (frame/event JSON)
- Create: `internal/frontend/api/server.go` (Serve, Server, sinks, handlers, SSE)
- Test: `internal/frontend/api/encode_test.go`, `internal/frontend/api/server_test.go`

**Interfaces:**
- Consumes: `bridge.MonitorSink`/`TxFrameSink`/`ConnSink`/`ConnEvent`, `Bridge.Register*Sink`/`Unregister*Sink`, `Bridge.StatusPorts`, `Bridge.ConnectionSnapshot`, `engine.Engine.Do`, `internal/version.Version`, `ax25.Frame`.
- Produces:
  - `api.Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int) (*Server, error)`
  - `(*Server) Close()`
  - `Server` implements `bridge.MonitorSink` (`OnRXFrame`), `bridge.TxFrameSink` (`OnTXFrame`), `bridge.ConnSink` (`OnConn`).

- [ ] **Step 1: Write the failing encoding test**

Create `internal/frontend/api/encode_test.go`:

```go
package api

import (
	"encoding/json"
	"testing"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

func TestEncodeFrameUIHasBase64Data(t *testing.T) {
	f := &ax25.Frame{Type: ax25.UI, PID: 0xF0,
		Src: ax25.Address{Call: "KU0HN"}, Dst: ax25.Address{Call: "CQ"},
		Via: []ax25.Address{{Call: "W0NE", SSID: 7}}, Info: []byte("hi")}
	b, _ := json.Marshal(encodeFrame(0, f))
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["type"] != "UI" || m["from"] != "KU0HN" || m["to"] != "CQ" || m["data"] != "aGk=" {
		t.Fatalf("UI encode wrong: %s", b)
	}
	if m["len"].(float64) != 2 {
		t.Fatalf("len wrong: %s", b)
	}
	via := m["via"].([]any)
	if len(via) != 1 || via[0] != "W0NE-7" {
		t.Fatalf("via wrong: %s", b)
	}
}

func TestEncodeFrameSFrameNoData(t *testing.T) {
	f := &ax25.Frame{Type: ax25.RR, Src: ax25.Address{Call: "A"}, Dst: ax25.Address{Call: "B"}}
	b, _ := json.Marshal(encodeFrame(0, f))
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["data"]; ok {
		t.Fatalf("S-frame must omit data: %s", b)
	}
	if _, ok := m["pid"]; ok {
		t.Fatalf("S-frame must omit pid: %s", b)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/frontend/api/ -run EncodeFrame -v`
Expected: FAIL — package/`encodeFrame` do not exist.

- [ ] **Step 3: Implement the encoder**

Create `internal/frontend/api/encode.go`:

```go
package api

import (
	"encoding/base64"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

// frameEvent is the JSON body of an rx/tx SSE event.
type frameEvent struct {
	Port int      `json:"port"`
	From string   `json:"from"`
	To   string   `json:"to"`
	Type string   `json:"type"`
	PID  *uint8   `json:"pid,omitempty"`
	Len  int      `json:"len"`
	Via  []string `json:"via"`
	Data string   `json:"data,omitempty"`
}

// encodeFrame turns a decoded AX.25 frame into an rx/tx event body. PID and
// base64 Data are included only for info-bearing frames (I, UI).
func encodeFrame(port int, f *ax25.Frame) frameEvent {
	via := make([]string, 0, len(f.Via))
	for _, a := range f.Via {
		via = append(via, a.String())
	}
	ev := frameEvent{
		Port: port, From: f.Src.String(), To: f.Dst.String(),
		Type: f.Type.String(), Len: len(f.Info), Via: via,
	}
	if f.Type == ax25.I || f.Type == ax25.UI {
		pid := f.PID
		ev.PID = &pid
		ev.Data = base64.StdEncoding.EncodeToString(f.Info)
	}
	return ev
}

type connectEvent struct {
	Port     int    `json:"port"`
	Local    string `json:"local"`
	Remote   string `json:"remote"`
	Incoming bool   `json:"incoming"`
}

type disconnectEvent struct {
	Port   int    `json:"port"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
}
```

- [ ] **Step 4: Verify encoding passes**

Run: `CGO_ENABLED=0 go test ./internal/frontend/api/ -run EncodeFrame -v` — Expected: PASS.

- [ ] **Step 5: Write the failing server test**

Create `internal/frontend/api/server_test.go`. Build a bridge with an injected fake port (mirror `internal/frontend/kisstcp/kisstcp_test.go`'s `newBridge`/`fakeSender` — copy that helper here), start `Serve`, and hit the endpoints with `net/http`:

```go
package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/ax25"
)

type fakeSender struct{ online bool }
func (fakeSender) Send([]byte)                   {}
func (fakeSender) SendCommand(uint8, []byte)     {}
func (f fakeSender) Online() bool                { return f.online }

func newBridge(t *testing.T, eng *engine.Engine) *bridge.Bridge {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}
	b := bridge.New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	bridge.InjectPorts(b, eng, params, []bridge.PortSender{fakeSender{online: true}})
	return b
}

func TestStatusEndpoint(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Version string               `json:"version"`
		Ports   []bridge.PortStatus  `json:"ports"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	if len(st.Ports) != 1 || !st.Ports[0].Online {
		t.Fatalf("status ports wrong: %+v", st)
	}
}

func TestEventsSSE(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Give the handler a moment to register, then drive an RX frame on the loop.
	time.Sleep(50 * time.Millisecond)
	f := &ax25.Frame{Type: ax25.UI, PID: 0xF0, Src: ax25.Address{Call: "A"}, Dst: ax25.Address{Call: "B"}, Info: []byte("hi")}
	eng.Do(func() { srv.OnRXFrame(0, f) })

	// Read one SSE event.
	// Read lines until the first complete event. The RX frame was pushed above;
	// the http client blocks on ReadString until the server flushes it. A
	// watchdog goroutine closes the body if nothing arrives, bounding the test.
	go func() { time.Sleep(3 * time.Second); resp.Body.Close() }()
	rd := bufio.NewReader(resp.Body)
	var evType, data string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "event: ") {
			evType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if evType != "rx" || !strings.Contains(data, `"from":"A"`) {
		t.Fatalf("SSE event wrong: type=%q data=%q", evType, data)
	}
}

// closeOnLoop closes the server on the engine loop (Close touches loop state).
func closeOnLoop(eng *engine.Engine, srv *Server) {
	done := make(chan struct{})
	eng.Do(func() { srv.Close(); close(done) })
	<-done
}
```

*(Implementer note: the `resp.Body.(interface{ SetReadDeadline… })` line is a no-op artifact — delete it; instead rely on the overall 2s loop deadline. If the SSE read blocks, the test's own timeout via `deadline` bounds it; keep the read simple.)*

- [ ] **Step 6: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/frontend/api/ -run 'StatusEndpoint|EventsSSE' -v`
Expected: FAIL — `Serve`/`Server`/`OnRXFrame` undefined.

- [ ] **Step 7: Implement the server**

Create `internal/frontend/api/server.go`:

```go
// Package api implements a read-only HTTP monitoring API (JSON + SSE).
// It registers as a bridge sink for RX/TX frames and connection events, and
// snapshots bridge/l2 state on the engine loop for the GET endpoints.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/internal/version"
)

type Server struct {
	eng        *engine.Engine
	b          *bridge.Bridge
	ln         net.Listener
	httpSrv    *http.Server
	maxClients int
	clients    map[*sseClient]struct{} // engine-loop only
}

type sseClient struct {
	ch chan []byte // buffered SSE frames
}

// Serve starts the API server and registers its sinks. Registration is
// marshalled onto the engine loop (safe during setup or while running).
func Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int) (*Server, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("api: listen %s: %w", addr, err)
	}
	s := &Server{eng: eng, b: b, ln: ln, maxClients: maxClients, clients: make(map[*sseClient]struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/connections", s.handleConnections)
	mux.HandleFunc("/api/events", s.handleEvents)
	s.httpSrv = &http.Server{Handler: mux}

	eng.Do(func() {
		b.RegisterMonitorSink(s)
		b.RegisterTxFrameSink(s)
		b.RegisterConnSink(s)
	})
	log.Printf("api: listening on %s", ln.Addr())
	go s.httpSrv.Serve(ln)
	return s, nil
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close unregisters sinks, stops the HTTP server, and drops SSE clients.
// MUST be called on the engine loop (touches sink registry + clients map).
func (s *Server) Close() {
	s.b.UnregisterMonitorSink(s)
	s.b.UnregisterTxFrameSink(s)
	s.b.UnregisterConnSink(s)
	s.httpSrv.Close()
	for c := range s.clients {
		close(c.ch)
	}
	s.clients = map[*sseClient]struct{}{}
}

// --- GET handlers (snapshot on the loop, serialize off it) ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var ports []bridge.PortStatus
	done := make(chan struct{})
	s.eng.Do(func() { ports = s.b.StatusPorts(); close(done) })
	<-done
	writeJSON(w, map[string]any{"version": version.Version, "ports": ports})
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	var conns any
	done := make(chan struct{})
	s.eng.Do(func() { conns = s.b.ConnectionSnapshot(); close(done) })
	<-done
	writeJSON(w, map[string]any{"connections": conns})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// --- SSE ---

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	c := &sseClient{ch: make(chan []byte, 256)}
	admitted := make(chan bool, 1)
	s.eng.Do(func() {
		if s.maxClients > 0 && len(s.clients) >= s.maxClients {
			admitted <- false
			return
		}
		s.clients[c] = struct{}{}
		admitted <- true
	})
	if !<-admitted {
		http.Error(w, "too many clients", http.StatusServiceUnavailable)
		return
	}
	defer s.eng.Do(func() { delete(s.clients, c) })

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	for {
		select {
		case msg, open := <-c.ch:
			if !open {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// enqueue sends a pre-framed SSE message to a client, non-blocking. Called on
// the loop. A full channel drops the message (slow client), never blocks.
func (s *Server) push(eventType string, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data))
	for c := range s.clients {
		select {
		case c.ch <- msg:
		default:
		}
	}
}

// --- bridge sink implementations (all called on the engine loop) ---

func (s *Server) OnRXFrame(port int, f *ax25.Frame) { s.push("rx", encodeFrame(port, f)) }
func (s *Server) OnTXFrame(port int, f *ax25.Frame) { s.push("tx", encodeFrame(port, f)) }

func (s *Server) OnConn(e bridge.ConnEvent) {
	if e.State == "connected" {
		s.push("connect", connectEvent{Port: e.Port, Local: e.Local, Remote: e.Remote, Incoming: e.Incoming})
	} else {
		s.push("disconnect", disconnectEvent{Port: e.Port, Local: e.Local, Remote: e.Remote})
	}
}
```

- [ ] **Step 8: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./internal/frontend/api/ -v`
Expected: PASS (encoding + status + SSE tests). If the SSE test flakes on timing, increase the post-connect `time.Sleep` slightly — the handler registers via `eng.Do` before events can reach it.

- [ ] **Step 9: Commit**

```bash
git add internal/frontend/api/
git commit -m "feat(api): read-only HTTP server — status, connections, SSE events"
```

---

### Task 6: Wire the API into main + shutdown

**Files:**
- Modify: `cmd/tncd/main.go`

**Interfaces:**
- Consumes: `api.Serve`, `(*api.Server) Close`, `cfg.API`.

- [ ] **Step 1: Start the API when enabled**

Add the import `apiserver "github.com/ben-kuhn/tncd/v2/internal/frontend/api"`. After the kisstcp start block, add:

```go
	var apiSrv *apiserver.Server
	if cfg.API.Enabled {
		apiSrv, err = apiserver.Serve(eng, b, cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.MaxClients)
		if err != nil {
			slog.Error("api server failed to start", "err", err)
			os.Exit(1)
		}
		slog.Info("read-only API started",
			"listen", fmt.Sprintf("%s:%d", cfg.API.ListenHost, cfg.API.ListenPort))
	}
```

(The API `Serve` registers its three sinks internally via `eng.Do`, so no separate `RegisterMonitorSink` call is needed here — unlike the AGWPE monitor which is registered explicitly.)

- [ ] **Step 2: Add to shutdown**

In the signal-handler `eng.Do(func(){...})` block, after `kissSrv.Close()` and before `b.Shutdown()`:

```go
			if apiSrv != nil {
				apiSrv.Close()
			}
```

- [ ] **Step 3: Build + full suite + vet**

Run:
```
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go vet ./...
```
Expected: builds; all tests pass; vet clean.

- [ ] **Step 4: Commit**

```bash
git add cmd/tncd/main.go
git commit -m "feat(cmd): start read-only API when [api] enabled"
```

---

### Task 7: e2e coverage

**Files:**
- Modify: the e2e harness (`e2e/` — find the Direwolf/PAT connected-mode test, e.g. `e2e/test_e2e.py` or `e2e/test_ax25_v22.py`)

**Interfaces:** none (Python e2e).

- [ ] **Step 1: Locate the harness + config writer**

`grep -rn "write_tncd_config\|def test_.*p2p\|agwpe" e2e/*.py`. Find the connected-mode P2P test that already brings up tncd + Direwolf + PAT. Note how it writes the tncd `.ini` (the `write_tncd_config` helper) and where the tncd port is known.

- [ ] **Step 2: Enable the API in the test's tncd config**

Extend the config writer used by the target test to append an `[api]` section:
```
[api]
enabled = true
listen_host = 127.0.0.1
listen_port = 8002
```
(Add an `api_port` kwarg defaulting to 8002 to the helper, mirroring how it already parameterizes listen ports; free-port it if the harness uses dynamic ports.)

- [ ] **Step 3: Add API assertions mid-session**

In the connected-mode test, after the PAT connection is established (a live AX.25 connection exists), add (using `urllib.request`, no new deps):

```python
import json, urllib.request

def _api_get(port, path):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=5) as r:
        return json.load(r)

# ... after the connection is up ...
status = _api_get(api_port, "/api/status")
assert status["ports"][0]["online"] is True
assert status["ports"][0]["rx_frames"] > 0 or status["ports"][0]["tx_frames"] > 0

conns = _api_get(api_port, "/api/connections")
assert any(c["state"] == "connected" for c in conns["connections"]), conns
```

And an SSE smoke check (read a couple of events with a timeout):
```python
def _sse_first_event(port, timeout=10):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/events", timeout=timeout) as r:
        etype = None
        for raw in r:
            line = raw.decode().rstrip("\r\n")
            if line.startswith("event: "):
                etype = line[len("event: "):]
            elif line.startswith("data: ") and etype:
                return etype, json.loads(line[len("data: "):])
    return None, None
```
Assert it yields an event whose type is one of `rx`/`tx`/`connect`/`disconnect` while traffic flows. (Trigger traffic first, e.g. right as the connection or a message transfer happens; keep the timeout generous.)

- [ ] **Step 4: Run the e2e test**

This requires the local audio/Direwolf bench and is **run by the user/controller, not auto-run** (it rewires PipeWire). Document the exact command in the report (e.g. `pytest e2e/test_e2e.py::test_... -v` in the e2e venv) and mark it PENDING-BENCH if the audio path is in use. The Go unit suite is the CI gate; this e2e is the integration gate to run on the bench.

- [ ] **Step 5: Commit**

```bash
git add e2e/
git commit -m "test(e2e): assert read-only API reflects a live Direwolf/PAT session"
```

---

## Final verification

- [ ] `CGO_ENABLED=0 go test ./...` — all pass.
- [ ] `CGO_ENABLED=0 go vet ./...` — clean.
- [ ] `[api]` absent/`enabled=false` ⇒ no HTTP surface; behavior identical to today.
- [ ] `/api/status` + `/api/connections` return the schemas from a live snapshot; counters live-scoped (reset on port offline).
- [ ] `/api/events` streams `rx`/`tx`/`connect`/`disconnect`; base64 payload on I/UI, omitted on S-frames.
- [ ] AGWPE monitor output byte-identical (MonitorSink untouched).
- [ ] SSE: slow client dropped, disconnect unregisters; `Server.Close` on the loop.
- [ ] e2e API assertions written (bench run may be PENDING).
- [ ] Merge `feature/api` → `main` with `--no-ff` after final review. No version bump.
