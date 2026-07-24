# KISS-over-TCP Passthrough + Frontend Subscriber Bus Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a Direwolf-8001-style KISS-over-TCP passthrough frontend so KISS-native apps share one TNC alongside AGWPE clients, built on a new generalized frontend subscriber bus in the bridge.

**Architecture:** The bridge emits typed events (raw RX, decoded RX) on the engine loop to registered narrow sink interfaces; frontends consume what they need. The AGWPE monitor migrates onto this bus (byte-identical). The KISS-TCP frontend registers a raw-RX sink for fan-out and calls the existing `SendToKISS` / a new `SendKISSCommand` for TX, mirroring the AGWPE frontend's goroutine↔engine-loop pattern.

**Tech Stack:** Go, `net`, `gopkg.in/ini.v1`. Reuses `kiss.Decoder` (KISS reassembly), `kiss.WrapData` (framing), the `engine.Engine` single-loop (`eng.Do`).

## Global Constraints

- Module path `github.com/ben-kuhn/tncd/v2`. `go` at `/nix/store/gb0njhqswlc5n127ikgyikvq39r40l6f-go-1.26.4/bin/go` if not on PATH. No gcc — use `CGO_ENABLED=0` for `go test`/`go vet`.
- Branch `feature/kisstcp` off `main`. Commit after each task with `--no-ff`-style task commits. No version bump (that's the phase-4 release).
- All bridge state-touching methods (`OnKISSFrame`, `SendToKISS`, `SendKISSCommand`, `Clients`, `Register*Sink`) MUST be called on the engine loop, except sink registration done during setup before `engine.Run` (single-threaded phase). Frontend goroutines marshal onto the loop via `eng.Do(...)`.
- **AGWPE monitor output bytes must stay byte-identical** after migration — the format is `pyham-pe`/PAT-validated and OTA-proven.
- Echo: **no loopback** — a frontend's TX is never re-delivered as RX (falls out of existing echo-suppression; do not add any re-injection).
- KISS command frames from clients: forward timing (low nibble 1–5) and SetHardware (6) to the physical TNC; **never** forward exit-KISS (0x0F); drop unknown/out-of-range with a debug log.
- `[kisstcp]` disabled by default; when enabled, default `127.0.0.1:8001`, `max_clients=16`. A config without `[kisstcp]` behaves exactly as today.

---

### Task 1: Frontend subscriber bus + AGWPE monitor migration

**Files:**
- Create: `internal/bridge/events.go`
- Delete: `internal/bridge/monitor.go`
- Create: `internal/frontend/agwpe/monitor.go`
- Modify: `internal/bridge/bridge.go` (`OnKISSFrame`, ~259–298; add sink fields to `Bridge` struct ~44–63)
- Test: `internal/bridge/events_test.go`, `internal/frontend/agwpe/monitor_test.go`

**Interfaces:**
- Produces:
  - `bridge.RawRXSink interface { OnRawRX(port int, raw []byte) }`
  - `bridge.MonitorSink interface { OnRXFrame(port int, f *ax25.Frame) }`
  - `(*bridge.Bridge) RegisterRawRXSink(s RawRXSink)` / `UnregisterRawRXSink(s RawRXSink)`
  - `(*bridge.Bridge) RegisterMonitorSink(s MonitorSink)` / `UnregisterMonitorSink(s MonitorSink)`
  - `agwpe.NewMonitorSink(b *bridge.Bridge) bridge.MonitorSink`
- Consumes: existing `Bridge.Clients() []Client`, `Client.Monitoring()`, `Client.SendAGWPE(...)`.

- [ ] **Step 1: Write the failing registry test**

Create `internal/bridge/events_test.go`:

```go
package bridge

import "testing"

type recRaw struct{ ports []int; raws [][]byte }
func (r *recRaw) OnRawRX(port int, raw []byte) {
	r.ports = append(r.ports, port)
	r.raws = append(r.raws, append([]byte{}, raw...))
}

func TestRawRXSinkRegistration(t *testing.T) {
	b := &Bridge{}
	s := &recRaw{}
	b.RegisterRawRXSink(s)
	b.emitRawRX(2, []byte{0x01, 0x02})
	if len(s.ports) != 1 || s.ports[0] != 2 {
		t.Fatalf("after register: ports=%v want [2]", s.ports)
	}
	b.UnregisterRawRXSink(s)
	b.emitRawRX(2, []byte{0x03})
	if len(s.ports) != 1 {
		t.Fatalf("after unregister: got %d deliveries, want 1", len(s.ports))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -run RawRXSink -v`
Expected: FAIL — `RegisterRawRXSink`, `emitRawRX`, `UnregisterRawRXSink` undefined.

- [ ] **Step 3: Implement the bus**

Create `internal/bridge/events.go`:

```go
// events.go — the frontend subscriber bus. Sinks are invoked on the engine
// loop. Registration happens during setup (before engine.Run) or on the loop.
package bridge

import "github.com/ben-kuhn/tncd/v2/ax25"

// RawRXSink receives raw AX.25 bytes heard from the air (post echo-suppression),
// e.g. the KISS-over-TCP passthrough.
type RawRXSink interface {
	OnRawRX(port int, raw []byte)
}

// MonitorSink receives decoded received frames for monitoring, e.g. the AGWPE
// monitor and (later) the read-only API.
type MonitorSink interface {
	OnRXFrame(port int, f *ax25.Frame)
}

func (b *Bridge) RegisterRawRXSink(s RawRXSink)     { b.rawSinks = append(b.rawSinks, s) }
func (b *Bridge) RegisterMonitorSink(s MonitorSink) { b.monitorSinks = append(b.monitorSinks, s) }

func (b *Bridge) UnregisterRawRXSink(s RawRXSink) {
	for i, x := range b.rawSinks {
		if x == s {
			b.rawSinks = append(b.rawSinks[:i], b.rawSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) UnregisterMonitorSink(s MonitorSink) {
	for i, x := range b.monitorSinks {
		if x == s {
			b.monitorSinks = append(b.monitorSinks[:i], b.monitorSinks[i+1:]...)
			return
		}
	}
}

func (b *Bridge) emitRawRX(port int, raw []byte) {
	for _, s := range b.rawSinks {
		s.OnRawRX(port, raw)
	}
}

func (b *Bridge) emitMonitor(port int, f *ax25.Frame) {
	for _, s := range b.monitorSinks {
		s.OnRXFrame(port, f)
	}
}
```

Add the two fields to the `Bridge` struct in `internal/bridge/bridge.go` (after `clients []Client`, ~line 49):

```go
	rawSinks     []RawRXSink
	monitorSinks []MonitorSink
```

- [ ] **Step 4: Run to verify registry passes**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -run RawRXSink -v` — Expected: PASS.

- [ ] **Step 5: Move the AGWPE monitor into the agwpe package**

Create `internal/frontend/agwpe/monitor.go` with the exact formatting from the old `bridge/monitor.go`, now iterating `b.Clients()`:

```go
// monitor.go — AGWPE monitor frame distribution (moved from bridge/monitor.go).
// Registered as a bridge.MonitorSink; output bytes are unchanged.
package agwpe

import (
	"fmt"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
)

type monitorSink struct{ b *bridge.Bridge }

// NewMonitorSink returns a MonitorSink that formats decoded RX frames as AGWPE
// 'U'/'I'/'S' monitor frames and delivers them to monitoring AGWPE clients.
func NewMonitorSink(b *bridge.Bridge) bridge.MonitorSink { return &monitorSink{b: b} }

func (m *monitorSink) OnRXFrame(port int, f *ax25.Frame) {
	src := f.Src.String()
	dst := f.Dst.String()
	ts := time.Now().Format("15:04:05")

	switch {
	case f.Type == ax25.UI:
		data := f.Info
		if data == nil {
			data = []byte{}
		}
		pid := f.PID
		header := fmt.Sprintf("Fm %s To %s <UI pid=%02X Len=%d >[%s]\r", src, dst, pid, len(data), ts)
		payload := append([]byte(header), data...)
		for _, c := range m.b.Clients() {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'U', pid, src, dst, payload)
			}
		}
	case f.Type.IsI():
		data := f.Info
		if data == nil {
			data = []byte{}
		}
		pid := f.PID
		header := fmt.Sprintf("Fm %s To %s <I pid=%02X Len=%d >[%s]\r", src, dst, pid, len(data), ts)
		payload := append([]byte(header), data...)
		for _, c := range m.b.Clients() {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'I', pid, src, dst, payload)
			}
		}
	case f.Type.IsS():
		name := f.Type.String()
		nr := f.NR
		payload := []byte(fmt.Sprintf("Fm %s To %s <%s R%d >[%s]\r", src, dst, name, nr, ts))
		for _, c := range m.b.Clients() {
			if c.Monitoring() {
				c.SendAGWPE(uint8(port), 'S', 0, src, dst, payload)
			}
		}
	}
}
```

Then **delete** `internal/bridge/monitor.go`.

- [ ] **Step 6: Rewire `OnKISSFrame` to emit to the bus**

In `internal/bridge/bridge.go` `OnKISSFrame`, replace the final line
`distributeMonitor(b.clients, f.Port, frame)` with:

```go
	// Fan out to the frontend subscriber bus.
	b.emitRawRX(f.Port, raw)
	b.emitMonitor(f.Port, frame)
```

(`raw := f.Data` is already the first line of `OnKISSFrame`.)

- [ ] **Step 7a: Convert the bridge-level monitor test**

`internal/bridge/bridge_test.go` has `TestMonitorDistribution` (~line 244): it calls `b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: raw})` and asserts a monitoring `fakeClient` receives a `'U'` frame with prefix `"Fm A To B <UI pid=F0 Len=5 >["`. **This breaks after migration** — `OnKISSFrame` now emits to `MonitorSink`s, and `package bridge` tests cannot import the agwpe package (import cycle), so the real formatter is unavailable here. Convert it to assert bus emission with a fake sink; the `'U'` wire-format assertion moves to the agwpe test in Step 7b.

Replace the body of `TestMonitorDistribution` (keep whatever `raw` UI-frame it builds) with a fake `MonitorSink` that captures the emitted decoded frame:

```go
type fakeMonitorSink struct {
	port int
	typ  ax25.FrameType
	src  string
	dst  string
	n    int
}
func (s *fakeMonitorSink) OnRXFrame(port int, f *ax25.Frame) {
	s.n++; s.port = port; s.typ = f.Type; s.src = f.Src.String(); s.dst = f.Dst.String()
}

func TestMonitorDistribution(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	var b *Bridge
	sink := &fakeMonitorSink{}
	raw := makeUIFrame("A", "B", []byte("hello")) // existing helper; 5-byte payload
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
		b.RegisterMonitorSink(sink)
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: raw})
	})
	if sink.n != 1 || sink.typ != ax25.UI || sink.src != "A" || sink.dst != "B" {
		t.Fatalf("emit = {n:%d typ:%v src:%s dst:%s}, want 1 UI A→B", sink.n, sink.typ, sink.src, sink.dst)
	}
}
```

(If `makeUIFrame`'s signature differs, use the existing call already in the test. Ensure `ax25` is imported in `bridge_test.go` — it is used elsewhere in the file.)

- [ ] **Step 7b: Add the AGWPE wire-format test in the agwpe package**

The exact `'U'`/`'I'`/`'S'` formatting is now tested where it lives. Add to `internal/frontend/agwpe/monitor_test.go` (a fake AGWPE client capturing `SendAGWPE` calls; verifies the exact UI header format):

```go
package agwpe

import (
	"strings"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

type capClient struct {
	mon  bool
	last struct{ kind byte; from, to string; data []byte }
	n    int
}
func (c *capClient) SendAGWPE(_ uint8, kind byte, _ uint8, from, to string, data []byte) {
	c.n++; c.last.kind = kind; c.last.from = from; c.last.to = to; c.last.data = data
}
func (c *capClient) Monitoring() bool                { return c.mon }
func (c *capClient) RegisteredCalls() map[string]bool { return map[string]bool{} }
func (c *capClient) LastActivity() time.Time         { return time.Now() }
func (c *capClient) CloseTransport()                 {}

func TestMonitorSinkUIFormat(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	b := bridge.New(eng, &config.Config{Server: config.Server{MaxClients: 8}})
	cc := &capClient{mon: true}
	done := make(chan struct{})
	eng.Do(func() { b.AddClient(cc); close(done) })
	<-done

	f := &ax25.Frame{Type: ax25.UI, PID: 0xF0,
		Src: ax25.Address{Call: "KU0HN"}, Dst: ax25.Address{Call: "CQ"}, Info: []byte("hi")}
	sink := NewMonitorSink(b)
	d2 := make(chan struct{})
	eng.Do(func() { sink.OnRXFrame(0, f); close(d2) })
	<-d2

	if cc.n != 1 || cc.last.kind != 'U' {
		t.Fatalf("got n=%d kind=%c, want 1 'U'", cc.n, cc.last.kind)
	}
	if !strings.HasPrefix(string(cc.last.data), "Fm KU0HN To CQ <UI pid=F0 Len=2 >[") {
		t.Fatalf("bad UI header: %q", cc.last.data)
	}
	if !strings.HasSuffix(string(cc.last.data), "hi") {
		t.Fatalf("UI payload missing data: %q", cc.last.data)
	}
}
```

- [ ] **Step 8: Run full bridge + agwpe suites**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ ./internal/frontend/agwpe/ -v`
Expected: PASS — the converted `TestMonitorDistribution` (bus emission), the new registry test, the agwpe wire-format test, and all pre-existing tests. Grep to confirm no remaining reference to `distributeMonitor`: `grep -rn distributeMonitor .` should return nothing.

- [ ] **Step 9: Commit**

```bash
git add internal/bridge/events.go internal/bridge/events_test.go internal/bridge/bridge.go \
  internal/bridge/bridge_test.go \
  internal/frontend/agwpe/monitor.go internal/frontend/agwpe/monitor_test.go
git rm internal/bridge/monitor.go
git commit -m "feat(bridge): frontend subscriber bus; migrate AGWPE monitor onto it"
```

---

### Task 2: KISS command-send plumbing (bridge → TNC)

**Files:**
- Modify: `kiss/framing.go` (add `WrapCommandBytes`)
- Modify: `kiss/port.go` (add `Port.SendCommand`)
- Modify: `internal/bridge/bridge.go` (`PortSender` interface ~34–37; add `SendKISSCommand`)
- Modify: `internal/bridge/bridge_test.go` (`fakePort` gains `SendCommand`)
- Test: `kiss/framing_test.go`, `kiss/port_test.go`, `internal/bridge/bridge_test.go`

**Interfaces:**
- Produces:
  - `kiss.WrapCommandBytes(kissPort uint8, cmd uint8, value []byte) []byte`
  - `(*kiss.Port) SendCommand(cmdType uint8, value []byte)`
  - `bridge.PortSender` gains method `SendCommand(cmdType uint8, value []byte)`
  - `(*bridge.Bridge) SendKISSCommand(port int, cmdType uint8, value []byte)`
- Consumes: existing `WrapData`, `WrapCommand`, the `Port.txCh` write path.

- [ ] **Step 1: Write the failing framing test**

Add to `kiss/framing_test.go`:

```go
func TestWrapCommandBytesMultiByteAndEscape(t *testing.T) {
	// SetHardware (cmd 6) on port 0 with a value containing FEND(0xC0) and FESC(0xDB).
	got := WrapCommandBytes(0, 0x06, []byte{0x01, FEND, FESC, 0x02})
	want := []byte{FEND, 0x06, 0x01, FESC, TFEND, FESC, TFESC, 0x02, FEND}
	if !bytes.Equal(got, want) {
		t.Fatalf("WrapCommandBytes = % x, want % x", got, want)
	}
}
```

(Ensure `bytes` is imported in the test file.)

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./kiss/ -run WrapCommandBytes -v`
Expected: FAIL — `WrapCommandBytes` undefined.

- [ ] **Step 3: Implement `WrapCommandBytes`**

Add to `kiss/framing.go`:

```go
// WrapCommandBytes builds a KISS command frame with a multi-byte value (e.g.
// SetHardware, cmd 6). cmd goes in the low nibble, kissPort in the high nibble;
// every value byte is escaped. WrapCommand remains the single-value convenience.
func WrapCommandBytes(kissPort uint8, cmd uint8, value []byte) []byte {
	result := []byte{FEND, (kissPort << 4) | cmd}
	for _, b := range value {
		switch b {
		case FEND:
			result = append(result, FESC, TFEND)
		case FESC:
			result = append(result, FESC, TFESC)
		default:
			result = append(result, b)
		}
	}
	return append(result, FEND)
}
```

- [ ] **Step 4: Verify framing test passes**

Run: `CGO_ENABLED=0 go test ./kiss/ -run WrapCommandBytes -v` — Expected: PASS.

- [ ] **Step 5: Write the failing `Port.SendCommand` test**

`kiss/port_test.go` already has `pipeTransport` (a `net.Conn`-backed `Transport`; see its definition ~line 12 and use in `NewPort(1, &pipeTransport{c: a}, ...)`). Reuse it with a `net.Pipe`: the port's writer writes to end `a`, the test reads the KISS bytes from end `b`. Add:

```go
func TestPortSendCommandWritesToTransport(t *testing.T) {
	a, b := net.Pipe()
	p := NewPort(0, &pipeTransport{c: a}, Params{}, func(RXFrame) {}, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Read the framed command bytes from the far end while SendCommand writes.
	got := make([]byte, 8)
	readDone := make(chan int, 1)
	go func() {
		n, _ := b.Read(got)
		readDone <- n
	}()
	p.SendCommand(0x01, []byte{40}) // TXDELAY=40 → FEND 01 28 FEND

	select {
	case n := <-readDone:
		want := []byte{FEND, 0x01, 40, FEND}
		if !bytes.Equal(got[:n], want) {
			t.Fatalf("far end got % x, want % x", got[:n], want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for command bytes")
	}
}
```

(Ensure `net`, `bytes`, and `time` are imported in `kiss/port_test.go`.)

- [ ] **Step 6: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./kiss/ -run PortSendCommand -v`
Expected: FAIL — `SendCommand` undefined.

- [ ] **Step 7: Implement `Port.SendCommand`**

Add to `kiss/port.go` (below `Send`):

```go
// SendCommand queues a KISS command frame (cmdType in 1..6) for transmission on
// this port's TNC. The wire port nibble is 0 (one physical TNC per Port).
// Dropped with a log if the TX queue is full.
func (p *Port) SendCommand(cmdType uint8, value []byte) {
	frame := WrapCommandBytes(0, cmdType, value)
	select {
	case p.txCh <- frame:
	default:
		log.Printf("kiss: port %d TX queue full, dropping command frame", p.num)
	}
}
```

- [ ] **Step 8: Verify port test passes**

Run: `CGO_ENABLED=0 go test ./kiss/ -run PortSendCommand -v` — Expected: PASS.

- [ ] **Step 9: Write the failing `Bridge.SendKISSCommand` test**

First extend `fakePort` in `internal/bridge/bridge_test.go` to implement the new `PortSender` method (records commands):

```go
// add field to fakePort struct: commands []struct{ cmd uint8; val []byte }
func (p *fakePort) SendCommand(cmdType uint8, value []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commands = append(p.commands, struct{ cmd uint8; val []byte }{cmdType, append([]byte{}, value...)})
}
func (p *fakePort) getCommands() []struct{ cmd uint8; val []byte } {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]struct{ cmd uint8; val []byte }, len(p.commands))
	copy(out, p.commands)
	return out
}
```

Then add the test:

```go
func TestSendKISSCommandRoutesToPort(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	var b *Bridge
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
		b.SendKISSCommand(0, 0x01, []byte{40})
		b.SendKISSCommand(5, 0x01, []byte{40}) // out of range → no-op
	})
	cmds := fp.getCommands()
	if len(cmds) != 1 || cmds[0].cmd != 0x01 || len(cmds[0].val) != 1 || cmds[0].val[0] != 40 {
		t.Fatalf("commands = %+v, want one TXDELAY=40", cmds)
	}
}
```

- [ ] **Step 10: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ -run SendKISSCommand -v`
Expected: FAIL — `PortSender` has no `SendCommand` (compile error) / `SendKISSCommand` undefined.

- [ ] **Step 11: Implement the interface method + `SendKISSCommand`**

In `internal/bridge/bridge.go`, add to the `PortSender` interface:

```go
type PortSender interface {
	Send([]byte)
	SendCommand(cmdType uint8, value []byte)
	Online() bool
}
```

And add the bridge method (near `SendToKISS`):

```go
// SendKISSCommand forwards a KISS command frame (timing params 1..6) to the
// given port's TNC. No-op for an out-of-range or offline port.
// Must be called on the engine loop.
func (b *Bridge) SendKISSCommand(port int, cmdType uint8, value []byte) {
	if port < 0 || port >= len(b.ports) {
		return
	}
	p := b.ports[port]
	if !p.Online() {
		return
	}
	p.SendCommand(cmdType, value)
}
```

Note: the real `*kiss.Port` now satisfies the extended `PortSender` (it has `SendCommand` from Step 7). If `internal/bridge/bridge.go`'s `offlineSentinel` type implements `PortSender`, add a no-op `SendCommand` to it too (grep `func (*offlineSentinel)`).

- [ ] **Step 12: Run bridge + kiss suites**

Run: `CGO_ENABLED=0 go test ./internal/bridge/ ./kiss/ -v` — Expected: PASS (new tests + all existing; the `offlineSentinel` and any other `PortSender` implementers compile).

- [ ] **Step 13: Commit**

```bash
git add kiss/framing.go kiss/framing_test.go kiss/port.go kiss/port_test.go internal/bridge/bridge.go internal/bridge/bridge_test.go
git commit -m "feat(kiss): forward KISS command frames to the TNC (SendCommand/SendKISSCommand)"
```

---

### Task 3: `[kisstcp]` config section

**Files:**
- Modify: `internal/config/config.go` (struct, known-keys, `Load` parse)
- Modify: `internal/config/example.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.KISSTCP struct { Enabled bool; ListenHost string; ListenPort int; MaxClients int }`; `Config.KISSTCP KISSTCP`.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go` (write the INI to a temp file; follow the file's existing temp-file test pattern):

```go
func TestKISSTCPSectionParsed(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte(`
[client.0]
type = serial
device = /dev/null

[kisstcp]
enabled = true
listen_port = 8010
`), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.KISSTCP.Enabled || cfg.KISSTCP.ListenPort != 8010 {
		t.Fatalf("KISSTCP = %+v, want enabled + port 8010", cfg.KISSTCP)
	}
	if cfg.KISSTCP.ListenHost != "127.0.0.1" || cfg.KISSTCP.MaxClients != 16 {
		t.Fatalf("defaults wrong: %+v", cfg.KISSTCP)
	}
}

func TestKISSTCPAbsentDisabled(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype = serial\ndevice = /dev/null\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KISSTCP.Enabled {
		t.Fatal("KISSTCP should default disabled when section absent")
	}
}
```

(Ensure `os` is imported in the test file.)

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/config/ -run KISSTCP -v`
Expected: FAIL — `cfg.KISSTCP` undefined.

- [ ] **Step 3: Implement the struct + parse**

In `internal/config/config.go`:

Add the struct (near `Server`):

```go
// KISSTCP holds the [kisstcp] section: a KISS-over-TCP passthrough listener.
type KISSTCP struct {
	Enabled    bool   // default false
	ListenHost string // default "127.0.0.1"
	ListenPort int    // default 8001
	MaxClients int    // default 16
}
```

Add the field to `Config`:

```go
type Config struct {
	Server  Server
	AX25    AX25
	KISSTCP KISSTCP
	Ports   []Port
}
```

Add known keys:

```go
var knownKISSTCPKeys = []string{"enabled", "listen_host", "listen_port", "max_clients"}
```

In `Load`, after the `[ax25]` parse block, add:

```go
	// --- Parse [kisstcp] ---
	kisstcpSec := f.Section("kisstcp")
	warnUnknownKeys(kisstcpSec, knownKISSTCPKeys)
	cfg.KISSTCP = KISSTCP{
		Enabled:    getBool(kisstcpSec, "enabled", false),
		ListenHost: getString(kisstcpSec, "listen_host", "127.0.0.1"),
		ListenPort: getInt(kisstcpSec, "listen_port", 8001),
		MaxClients: getInt(kisstcpSec, "max_clients", 16),
	}
```

- [ ] **Step 4: Verify config tests pass**

Run: `CGO_ENABLED=0 go test ./internal/config/ -v` — Expected: PASS.

- [ ] **Step 5: Add a commented `[kisstcp]` block to the example config**

In `internal/config/example.go` `Example()`, append after the `[ax25]` block (match the file's existing string-building style):

```
# [kisstcp]
# KISS-over-TCP passthrough: lets KISS-native apps (woad, mobile clients)
# share the TNC alongside AGWPE clients. Disabled by default.
# enabled = false
# listen_host = 127.0.0.1
# listen_port = 8001
# max_clients = 16
```

The only test over `Example()` is `TestExampleLoads` (config_test.go:84), which just checks the output parses via `Load`. An all-commented `[kisstcp]` block does not affect parsing, so no test update is needed. Confirm: `CGO_ENABLED=0 go test ./internal/config/ -v`.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/config/example.go
git commit -m "feat(config): [kisstcp] passthrough section (disabled by default)"
```

---

### Task 4: KISS-TCP server + client (RX fan-out + TX/command dispatch)

**Files:**
- Create: `internal/frontend/kisstcp/server.go`
- Create: `internal/frontend/kisstcp/client.go`
- Test: `internal/frontend/kisstcp/kisstcp_test.go`

**Interfaces:**
- Consumes: `bridge.RawRXSink`, `Bridge.RegisterRawRXSink`/`UnregisterRawRXSink`, `Bridge.SendToKISS`, `Bridge.SendKISSCommand`, `kiss.Decoder`, `kiss.WrapData`, `engine.Engine.Do`.
- Produces:
  - `kisstcp.Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int) (*Server, error)`
  - `(*Server) Close()`
  - `(*Server) OnRawRX(port int, raw []byte)` (implements `bridge.RawRXSink`)

- [ ] **Step 1: Write the failing integration test**

Create `internal/frontend/kisstcp/kisstcp_test.go`. It starts a real `Server` against a bridge with a fake port, connects a TCP client, sends a KISS data frame, and asserts it reaches the port; then drives RX and asserts the client receives a wrapped frame. Reuse the bridge test's `makeBridge`/`fakePort` pattern via a local minimal bridge builder (the bridge package's helpers are unexported, so build a real `bridge.New` with an injected fake port through `bridge.InjectPorts`, mirroring `bridge_test.go:445-454`).

```go
package kisstcp

import (
	"net"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// fakeSender implements bridge.PortSender, recording data + command sends.
type fakeSender struct {
	ch   chan []byte
	cmds chan struct{ cmd uint8; val []byte }
}
func newFakeSender() *fakeSender {
	return &fakeSender{ch: make(chan []byte, 8), cmds: make(chan struct{ cmd uint8; val []byte }, 8)}
}
func (f *fakeSender) Send(raw []byte)                        { f.ch <- append([]byte{}, raw...) }
func (f *fakeSender) SendCommand(cmd uint8, val []byte)      { f.cmds <- struct{ cmd uint8; val []byte }{cmd, append([]byte{}, val...)} }
func (f *fakeSender) Online() bool                           { return true }

func newBridge(t *testing.T, eng *engine.Engine, fs *fakeSender) *bridge.Bridge {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}
	b := bridge.New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	bridge.InjectPorts(b, eng, params, []bridge.PortSender{fs})
	return b
}

func TestKISSTCPRoundTrip(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fs := newFakeSender()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng, fs); close(done) })
	<-done

	srv, err := Serve(eng, b, "127.0.0.1", 0, 16) // port 0 = OS-assigned
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// TX: client sends a KISS data frame with AX.25 payload {0xAA,0xBB}.
	conn.Write(kiss.WrapData(0, []byte{0xAA, 0xBB}))
	select {
	case got := <-fs.ch:
		if len(got) != 2 || got[0] != 0xAA || got[1] != 0xBB {
			t.Fatalf("port got % x, want AA BB", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for TX to reach port")
	}

	// RX: bridge fans a raw frame out to the client.
	eng.Do(func() { srv.OnRawRX(0, []byte{0x11, 0x22}) })
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	rbuf := make([]byte, 64)
	n, err := conn.Read(rbuf)
	if err != nil {
		t.Fatal(err)
	}
	want := kiss.WrapData(0, []byte{0x11, 0x22})
	if string(rbuf[:n]) != string(want) {
		t.Fatalf("client RX = % x, want % x", rbuf[:n], want)
	}
}

func TestKISSTCPExitKISSDropped(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fs := newFakeSender()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng, fs); close(done) })
	<-done
	srv, _ := Serve(eng, b, "127.0.0.1", 0, 16)
	defer srv.Close()
	conn, _ := net.Dial("tcp", srv.Addr())
	defer conn.Close()

	conn.Write([]byte{kiss.FEND, 0xFF, kiss.FEND}) // exit-KISS
	conn.Write(kiss.WrapCommandBytes(0, 0x01, []byte{40})) // TXDELAY should forward
	select {
	case c := <-fs.cmds:
		if c.cmd != 0x01 {
			t.Fatalf("got cmd %#x, want TXDELAY", c.cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout; TXDELAY not forwarded")
	}
	select {
	case <-fs.cmds:
		t.Fatal("exit-KISS must not be forwarded as a command")
	case <-fs.ch:
		t.Fatal("exit-KISS must not be forwarded as data")
	case <-time.After(200 * time.Millisecond):
		// good — nothing extra forwarded
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/frontend/kisstcp/ -v`
Expected: FAIL — package/`Serve`/`Server` do not exist.

- [ ] **Step 3: Implement the server**

Create `internal/frontend/kisstcp/server.go`:

```go
// Package kisstcp implements a Direwolf-8001-style KISS-over-TCP passthrough.
// Connected clients hear every frame received from the air and can transmit;
// TX shares the per-port queue with the L2 engine. Registered as a
// bridge.RawRXSink. Mirrors the AGWPE frontend's goroutine↔engine-loop pattern.
package kisstcp

import (
	"fmt"
	"log"
	"net"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// Server owns the listener and the set of connected clients. The client set is
// mutated and read only on the engine loop.
type Server struct {
	eng        *engine.Engine
	b          *bridge.Bridge
	ln         net.Listener
	maxClients int
	clients    map[*client]struct{} // engine-loop only
}

// Serve starts the listener and registers the server as a RawRXSink.
// Registration is marshalled onto the engine loop (safe whether Serve is called
// during setup before engine.Run — where it simply queues — or after).
func Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int) (*Server, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("kisstcp: listen %s: %w", addr, err)
	}
	s := &Server{eng: eng, b: b, ln: ln, maxClients: maxClients, clients: make(map[*client]struct{})}
	eng.Do(func() { b.RegisterRawRXSink(s) })
	log.Printf("kisstcp: listening on %s", ln.Addr())

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go newClient(s, conn).run()
		}
	}()
	return s, nil
}

// Addr returns the listener's actual address (useful when port 0 was requested).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// OnRawRX wraps a heard frame as a KISS data frame (port nibble = port) and
// enqueues it to every connected client. Called on the engine loop.
func (s *Server) OnRawRX(port int, raw []byte) {
	frame := kiss.WrapData(uint8(port), raw)
	for c := range s.clients {
		c.enqueue(frame)
	}
}

// add/remove run on the engine loop (via eng.Do from client goroutines).
func (s *Server) add(c *client) bool {
	if s.maxClients > 0 && len(s.clients) >= s.maxClients {
		return false
	}
	s.clients[c] = struct{}{}
	return true
}
func (s *Server) remove(c *client) { delete(s.clients, c) }

// Close stops accepting, unregisters the sink, and closes all clients.
// Called on the engine loop from the shutdown sequence.
func (s *Server) Close() {
	s.b.UnregisterRawRXSink(s)
	s.ln.Close()
	for c := range s.clients {
		c.close()
	}
}
```

- [ ] **Step 4: Implement the client**

Create `internal/frontend/kisstcp/client.go`:

```go
package kisstcp

import (
	"log"
	"net"
	"sync"

	"github.com/ben-kuhn/tncd/v2/kiss"
)

type client struct {
	s    *Server
	conn net.Conn

	writeCh chan []byte // buffered outbound KISS frames

	mu     sync.Mutex
	closed bool
}

func newClient(s *Server, conn net.Conn) *client {
	return &client{s: s, conn: conn, writeCh: make(chan []byte, 256)}
}

// enqueue is called on the engine loop (from Server.OnRawRX). Non-blocking;
// a full channel closes the slow client rather than stalling the loop.
func (c *client) enqueue(frame []byte) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	select {
	case c.writeCh <- frame:
		c.mu.Unlock()
	default:
		c.mu.Unlock()
		log.Printf("kisstcp: write channel full for %s, closing", c.conn.RemoteAddr())
		c.close()
	}
}

func (c *client) run() {
	// Register on the engine loop; reject if at capacity.
	accepted := make(chan bool, 1)
	c.s.eng.Do(func() { accepted <- c.s.add(c) })
	if !<-accepted {
		log.Printf("kisstcp: client limit reached, rejecting %s", c.conn.RemoteAddr())
		c.conn.Close()
		return
	}
	log.Printf("kisstcp: client connected from %s", c.conn.RemoteAddr())

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for pkt := range c.writeCh {
			if _, err := c.conn.Write(pkt); err != nil {
				return
			}
		}
	}()

	var dec kiss.Decoder
	rbuf := make([]byte, 4096)
	for {
		n, err := c.conn.Read(rbuf)
		if n > 0 {
			for _, frame := range dec.Feed(rbuf[:n]) {
				c.dispatch(frame)
			}
		}
		if err != nil {
			break
		}
	}

	c.close()
	<-writerDone
	done := make(chan struct{})
	c.s.eng.Do(func() { c.s.remove(c); close(done) })
	<-done
	log.Printf("kisstcp: client disconnected from %s", c.conn.RemoteAddr())
}

// dispatch classifies one reassembled KISS frame (cmd byte + payload) and
// marshals the action onto the engine loop.
func (c *client) dispatch(frame []byte) {
	if len(frame) < 1 {
		return
	}
	cmdByte := frame[0]
	port := int(cmdByte >> 4)
	cmdType := cmdByte & 0x0F
	payload := append([]byte{}, frame[1:]...)

	switch {
	case cmdType == 0x00: // data
		c.s.eng.Do(func() { c.s.b.SendToKISS(port, payload) })
	case cmdType >= 0x01 && cmdType <= 0x06: // timing params + SetHardware
		c.s.eng.Do(func() { c.s.b.SendKISSCommand(port, cmdType, payload) })
	case cmdType == 0x0F: // exit-KISS — never forward
		log.Printf("kisstcp: dropping exit-KISS from %s (protecting shared TNC)", c.conn.RemoteAddr())
	default:
		log.Printf("kisstcp: dropping unknown KISS command %#x from %s", cmdType, c.conn.RemoteAddr())
	}
}

func (c *client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.conn.Close()
		close(c.writeCh)
	}
}
```

- [ ] **Step 5: Run to verify integration tests pass**

Run: `CGO_ENABLED=0 go test ./internal/frontend/kisstcp/ -v`
Expected: PASS (`TestKISSTCPRoundTrip`, `TestKISSTCPExitKISSDropped`).

Note on `close()`: the read loop and `Server.Close` can both call it; the `closed` guard makes it idempotent, and closing `writeCh` after setting `closed` is safe because `enqueue` checks `closed` under the same mutex before sending.

- [ ] **Step 6: Commit**

```bash
git add internal/frontend/kisstcp/
git commit -m "feat(kisstcp): KISS-over-TCP passthrough frontend (RX fan-out + TX/command dispatch)"
```

---

### Task 5: Wire KISS-TCP into main + shutdown

**Files:**
- Modify: `cmd/tncd/main.go` (frontend startup ~225–241; shutdown sequence ~250–276)
- Modify: `internal/config/example.go` is already done (Task 3).

**Interfaces:**
- Consumes: `kisstcp.Serve(...)`, `(*kisstcp.Server) Close()`, `agwpe.NewMonitorSink`, `Bridge.RegisterMonitorSink`, `cfg.KISSTCP`.

- [ ] **Step 1: Register the AGWPE monitor sink**

In `cmd/tncd/main.go`, after `b.Start()` succeeds and before `agwpeserver.Serve(...)`, register the migrated monitor sink (setup phase, before `eng.Run`):

```go
	b.RegisterMonitorSink(agwpeserver.NewMonitorSink(b))
```

(Task 1 moved the monitor formatting into the agwpe package but nothing registered it yet — this restores AGWPE monitoring end-to-end. Without this step, monitoring silently stops.)

- [ ] **Step 2: Start the KISS-TCP listener when enabled**

Add the import `kisstcpserver "github.com/ben-kuhn/tncd/v2/internal/frontend/kisstcp"`. After the AGWPE `Serve` block, add:

```go
	var kissSrv *kisstcpserver.Server
	if cfg.KISSTCP.Enabled {
		kissSrv, err = kisstcpserver.Serve(eng, b, cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort, cfg.KISSTCP.MaxClients)
		if err != nil {
			slog.Error("kisstcp server failed to start", "err", err)
			os.Exit(1)
		}
		slog.Info("KISS-over-TCP passthrough started",
			"listen", fmt.Sprintf("%s:%d", cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort))
	}
```

- [ ] **Step 3: Add KISS-TCP to the shutdown sequence**

In the signal handler's `eng.Do(func() { ... })` block, close the KISS-TCP server between the AGWPE client-close step and `b.Shutdown()`:

```go
			// Step 2b: close the KISS-over-TCP server (listener + clients).
			if kissSrv != nil {
				kissSrv.Close()
			}
```

(Place it after `ln.Close()` and before `b.Shutdown()`.)

- [ ] **Step 4: Build + full suite + vet**

Run:
```
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go vet ./...
```
Expected: builds; all tests pass; vet clean.

- [ ] **Step 5: Manual smoke check (documented, not automated)**

With a config that has `[kisstcp] enabled = true` and any port, start `tncd -c file`, then in another shell `nc 127.0.0.1 8001` (or `kissutil`) connects without error and the log shows `kisstcp: client connected`. This is a sanity check for the reviewer/user; not a committed test.

- [ ] **Step 6: Commit**

```bash
git add cmd/tncd/main.go
git commit -m "feat(cmd): start KISS-over-TCP passthrough when [kisstcp] enabled; register AGWPE monitor sink"
```

---

## Final verification

- [ ] `CGO_ENABLED=0 go test ./...` — all pass.
- [ ] `CGO_ENABLED=0 go vet ./...` — clean.
- [ ] AGWPE monitor output byte-identical (Task 1 tests + existing agwpe tests green); **the monitor sink is registered in main (Task 5 Step 1)** — verify monitoring is not silently dead.
- [ ] KISS-TCP round-trip: client TX reaches the port; heard frames fan out to clients wrapped with the correct port nibble; no loopback.
- [ ] Command frames: timing/SetHardware forwarded; exit-KISS dropped.
- [ ] `[kisstcp]` absent ⇒ behavior identical to today.
- [ ] Merge `feature/kisstcp` → `main` with `--no-ff` after final review. No version bump (phase-4 release cuts the tag).
