# Windows Support — Plan 1: Platform-Neutral `Runtime` Refactor

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract tncd's startup and graceful-shutdown logic out of `cmd/tncd/main.go` into a reusable `internal/app.Runtime`, so the console path and (in later plans) the Windows service handler share one source of truth for "run the bridge" and "shut it down cleanly."

**Architecture:** A new `internal/app` package exposes `Runtime` with `New` (wire engine + bridge + frontends, non-blocking), `Wait` (run the engine loop), and `Shutdown` (the existing 4-step teardown). `main.go` is reduced to flag/config handling plus `New`/`Wait`/`Shutdown`. No behavior changes; the existing e2e suite and Go tests stay green, and `GOOS=windows` still compiles.

**Tech Stack:** Go (pure, no cgo), existing `internal/{engine,bridge,config,frontend/*}` packages.

## Global Constraints

- Pure Go, **no cgo**. (`CGO_ENABLED=0` must still build for every target.)
- Module path is `github.com/ben-kuhn/tncd/v2`; all imports use that prefix.
- INI config format is **unchanged** — this plan touches no config keys.
- Behavioral parity: the AGWPE/KISS-over-TCP/API startup and the 4-step
  shutdown ordering must match the current `main.go` exactly.
- No platform build-tag split in this plan. Signal handling stays inline in
  `main.go` (it already cross-compiles to Windows). The `main_unix.go` /
  `main_windows.go` split is deferred to Plan 2 (service).

## File Structure

- **Create `internal/app/app.go`** — the `Runtime` type: `New`, `Wait`,
  `Shutdown`, `AGWPEAddr`. One responsibility: assemble and lifecycle-manage a
  running tncd instance.
- **Create `internal/app/app_test.go`** — external (`package app_test`) test
  that starts a `Runtime` on an ephemeral port, confirms the AGWPE listener
  accepts, then confirms `Shutdown` stops the loop and closes the listener.
- **Modify `cmd/tncd/main.go:225-314`** — replace the inline engine/bridge/
  frontend wiring and the inline signal-handler shutdown block with calls to
  `app.New` / `app.Wait` / `app.Shutdown`. Adjust imports.

---

### Task 1: `internal/app.Runtime`

**Files:**
- Create: `internal/app/app.go`
- Test: `internal/app/app_test.go`

**Interfaces:**
- Consumes (existing, verified signatures):
  - `engine.New() *engine.Engine`; `(*engine.Engine).Run()`, `.Stop()`, `.Do(func())`
  - `bridge.New(eng *engine.Engine, cfg *config.Config) *bridge.Bridge`
  - `(*bridge.Bridge).SetVerbosity(verbose, traffic int)`, `.Start() error`,
    `.RegisterMonitorSink(bridge.MonitorSink)`, `.Clients() []bridge.Client`,
    `.Shutdown()`; `bridge.Client` has `CloseTransport()`
  - `agwpeserver.NewMonitorSink(b *bridge.Bridge) bridge.MonitorSink`
  - `agwpeserver.Serve(eng, b, host string, port int) (net.Listener, error)`
  - `kisstcpserver.Serve(eng, b, host string, port, maxClients int) (*kisstcpserver.Server, error)`; `(*Server).Close()`
  - `apiserver.Serve(eng, b, host string, port, maxClients int, serveUI bool) (*apiserver.Server, error)`; `(*Server).Close()`
- Produces (later plans + main rely on these exact names/types):
  - `app.New(cfg *config.Config, verbose, traffic int) (*app.Runtime, error)`
  - `(*app.Runtime).Wait()`
  - `(*app.Runtime).Shutdown()`
  - `(*app.Runtime).AGWPEAddr() net.Addr`

- [ ] **Step 1: Write the failing test**

Create `internal/app/app_test.go`:

```go
package app_test

import (
	"net"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// TestRuntimeServesThenShutsDown verifies that New brings up the AGWPE
// listener (accepting connections) and that Shutdown both stops the engine
// loop (Wait returns) and closes the listener.
func TestRuntimeServesThenShutsDown(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{
			ListenHost: "127.0.0.1",
			ListenPort: 0, // ephemeral
			Callsign:   "TEST",
			MaxClients: 8,
		},
		AX25: config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 180},
		Ports: []config.Port{{
			Name:        "Port 0",
			Type:        "tcp",
			Host:        "127.0.0.1",
			TCPPort:     1, // nothing listening; bridge connects async, won't block New
			OTABaudrate: 1200,
			AX25Version: 22,
			Reconnect:   false,
		}},
	}

	r, err := app.New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	addr := r.AGWPEAddr().String()

	done := make(chan struct{})
	go func() { r.Wait(); close(done) }()

	// Listener is accepting while running.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial while running: %v", err)
	}
	c.Close()

	r.Shutdown()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Shutdown")
	}

	// Listener is closed after shutdown.
	if c2, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c2.Close()
		t.Fatal("listener still accepting after Shutdown")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/app/ -run TestRuntimeServesThenShutsDown -v`
Expected: FAIL — compile error, `undefined: app.New` / `app.Runtime` (package has no `app.go` yet).

- [ ] **Step 3: Write the minimal implementation**

Create `internal/app/app.go`:

```go
// Package app wires the tncd engine, bridge, and frontends into a single
// runnable unit shared by every launch mode (console today; the Windows
// service in a later plan). It owns startup and the graceful shutdown
// sequence so there is exactly one source of truth for both.
package app

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	agwpeserver "github.com/ben-kuhn/tncd/v2/internal/frontend/agwpe"
	apiserver "github.com/ben-kuhn/tncd/v2/internal/frontend/api"
	kisstcpserver "github.com/ben-kuhn/tncd/v2/internal/frontend/kisstcp"
)

// Runtime is a wired-up tncd instance: engine, bridge, and all enabled
// frontends. Build one with New, then call Wait to run it; Shutdown stops it.
type Runtime struct {
	eng     *engine.Engine
	bridge  *bridge.Bridge
	agwpeLn net.Listener
	kissSrv *kisstcpserver.Server
	apiSrv  *apiserver.Server
}

// New builds the engine and bridge, starts the AGWPE server, and starts the
// KISS-over-TCP and read-only API servers when enabled in cfg. It does not
// block; call Wait to run the engine loop. verbose and traffic set AX.25 frame
// and hex-dump verbosity (0 = off). On any startup error, already-opened
// listeners are closed before returning.
func New(cfg *config.Config, verbose, traffic int) (*Runtime, error) {
	eng := engine.New()
	b := bridge.New(eng, cfg)
	b.SetVerbosity(verbose, traffic)

	if err := b.Start(); err != nil {
		return nil, fmt.Errorf("bridge start: %w", err)
	}
	b.RegisterMonitorSink(agwpeserver.NewMonitorSink(b))

	ln, err := agwpeserver.Serve(eng, b, cfg.Server.ListenHost, cfg.Server.ListenPort)
	if err != nil {
		return nil, fmt.Errorf("agwpe server: %w", err)
	}

	r := &Runtime{eng: eng, bridge: b, agwpeLn: ln}

	if cfg.KISSTCP.Enabled {
		r.kissSrv, err = kisstcpserver.Serve(eng, b, cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort, cfg.KISSTCP.MaxClients)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("kisstcp server: %w", err)
		}
		slog.Info("KISS-over-TCP passthrough started",
			"listen", fmt.Sprintf("%s:%d", cfg.KISSTCP.ListenHost, cfg.KISSTCP.ListenPort))
	}

	if cfg.API.Enabled {
		r.apiSrv, err = apiserver.Serve(eng, b, cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.MaxClients, cfg.API.ServeUI)
		if err != nil {
			if r.kissSrv != nil {
				r.kissSrv.Close()
			}
			ln.Close()
			return nil, fmt.Errorf("api server: %w", err)
		}
		slog.Info("read-only API started",
			"listen", fmt.Sprintf("%s:%d", cfg.API.ListenHost, cfg.API.ListenPort))
	}

	return r, nil
}

// AGWPEAddr returns the address the AGWPE server is listening on. Useful when
// the configured port is 0 (ephemeral) and for status/manage displays.
func (r *Runtime) AGWPEAddr() net.Addr { return r.agwpeLn.Addr() }

// Wait runs the engine loop on the calling goroutine, blocking until Shutdown
// completes its teardown (which stops the loop).
func (r *Runtime) Wait() { r.eng.Run() }

// Shutdown posts the graceful teardown sequence to the engine loop. Ordering
// mirrors the original main.go path:
//  1. close AGWPE client transports (so the listener's Accept unblocks),
//  2. close the listeners (AGWPE, KISS-over-TCP, API),
//  3. bridge.Shutdown() (KISS exit strings + port close),
//  4. engine.Stop().
// Safe to call from any goroutine.
func (r *Runtime) Shutdown() {
	r.eng.Do(func() {
		for _, c := range r.bridge.Clients() {
			c.CloseTransport()
		}
		r.agwpeLn.Close()
		if r.kissSrv != nil {
			r.kissSrv.Close()
		}
		if r.apiSrv != nil {
			r.apiSrv.Close()
		}
		r.bridge.Shutdown()
		r.eng.Stop()
	})
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/app/ -run TestRuntimeServesThenShutsDown -v`
Expected: PASS.

- [ ] **Step 5: Verify Windows still cross-compiles**

Run: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`
Expected: exit 0, no output.

- [ ] **Step 6: Commit**

```bash
git add internal/app/app.go internal/app/app_test.go
git commit -m "feat(app): extract Runtime (start/wait/shutdown) from main"
```

---

### Task 2: Rewire `cmd/tncd/main.go` onto `Runtime`

**Files:**
- Modify: `cmd/tncd/main.go` (replace lines 225–314; adjust the import block)

**Interfaces:**
- Consumes: `app.New`, `(*Runtime).Wait`, `(*Runtime).Shutdown`,
  `(*Runtime).AGWPEAddr` from Task 1.
- Produces: no new exported symbols; `main` behavior is unchanged.

- [ ] **Step 1: Replace the wiring + shutdown block**

In `cmd/tncd/main.go`, replace everything from the `// --- Build runtime ---`
comment through the final `slog.Info("tncd stopped")` (current lines 225–314)
with:

```go
	// --- Build and run ---
	r, err := app.New(cfg, vCount.n, tCount.n)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}

	slog.Info("tncd running", "version", version.Version, "listen", r.AGWPEAddr().String())
	slog.Info("Press Ctrl+C to stop")

	// SIGINT / SIGTERM → graceful shutdown. (Platform split into main_unix.go /
	// main_windows.go arrives with the service plan; this already cross-compiles
	// to Windows unchanged.)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("Received signal, shutting down...", "signal", sig)
		r.Shutdown()
	}()

	r.Wait()
	slog.Info("tncd stopped")
}
```

Note: this replaces the closing `}` of `main` as well — do not leave a
duplicate brace.

- [ ] **Step 2: Fix the import block**

In `cmd/tncd/main.go`, update the imports: **remove** the now-unused
`"github.com/ben-kuhn/tncd/v2/internal/bridge"`,
`"github.com/ben-kuhn/tncd/v2/internal/engine"`,
`agwpeserver "github.com/ben-kuhn/tncd/v2/internal/frontend/agwpe"`,
`apiserver "github.com/ben-kuhn/tncd/v2/internal/frontend/api"`,
`kisstcpserver "github.com/ben-kuhn/tncd/v2/internal/frontend/kisstcp"`; and
**add** `"github.com/ben-kuhn/tncd/v2/internal/app"`. Keep `flag`, `fmt`,
`log`, `log/slog`, `os`, `os/signal`, `strings`, `syscall`,
`internal/config`, `internal/version`.

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: exit 0, no output. (If `go vet` reports an unused import, remove it;
if it reports `imported and not used`, re-check Step 2.)

- [ ] **Step 4: Full test suite**

Run: `go test ./...`
Expected: PASS across all packages (same set that passed before this plan).

- [ ] **Step 5: Manual foreground smoke test**

```bash
printf '[server]\nlisten_host = 127.0.0.1\nlisten_port = 0\ncallsign = TEST\n\n[client.0]\ntype = tcp\nhost = 127.0.0.1\nport = 1\nreconnect = false\n' > /tmp/tncd-smoke.ini
go build -o /tmp/tncd ./cmd/tncd
/tmp/tncd -c /tmp/tncd-smoke.ini &
TNCD_PID=$!
sleep 1
kill -INT $TNCD_PID
wait $TNCD_PID
```

Expected: startup logs `tncd running`, then on `kill -INT`:
`Received signal, shutting down...` followed by `tncd stopped`, and the process
exits cleanly (no panic, no hang).

- [ ] **Step 6: Verify Windows still cross-compiles**

Run: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
git add cmd/tncd/main.go
git commit -m "refactor(cmd): run tncd via internal/app.Runtime"
```

---

## Self-Review

**Spec coverage (this plan = spec Component 1 only):** The spec's
`internal/app.Runtime` with `New`/`Wait`/`Shutdown` (spec "Component 1 — Runtime
refactor") is implemented in Task 1; the console rewiring is Task 2. `AGWPEAddr`
is added beyond the spec's three-method sketch because the spec's own manage
window and status displays need the live listen address, and the test needs it
for the ephemeral port — a minimal, justified addition. All later components
(service, ports, Bluetooth, GUI, packaging) are explicitly out of scope for this
plan and get their own plans, per the spec's implementation order.

**Placeholder scan:** No TBD/TODO/"handle errors appropriately". Every code step
contains complete, compilable code. Every command has an expected result.

**Type consistency:** `app.New(cfg *config.Config, verbose, traffic int)
(*Runtime, error)`, `Wait()`, `Shutdown()`, `AGWPEAddr() net.Addr` are used
identically in the test (Task 1), the implementation (Task 1), and main (Task 2).
Consumed signatures (`bridge.New`, `agwpeserver.Serve`, `kisstcpserver.Serve`,
`apiserver.Serve`, `bridge.Client.CloseTransport`) were read from the current
source and match.

**Green-everywhere check:** No build-tag files are introduced, and the retained
`syscall.SIGINT/SIGTERM` signal code already compiles under `GOOS=windows`
(verified in the current tree), so Steps 1.5 / 2.6 guard against regressions.
