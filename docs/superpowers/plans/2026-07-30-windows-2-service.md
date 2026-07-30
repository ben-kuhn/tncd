# Windows Support — Plan 2: Windows Service (SCM) + Event Log + `service` CLI

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `tncd.exe` run as a proper Windows service under the Service Control Manager (SCM), log to the Windows Event Log when service-hosted, and manage itself via `tncd service install|uninstall|start|stop` — while keeping the Unix/console behavior identical and the tree green on every platform.

**Architecture:** A thin platform shim in `cmd/tncd` (`run_unix.go` / `run_windows.go`, `service_unix.go` / `service_windows.go`) provides four functions that `main.go` calls: `isWindowsService()`, `installLogging()`, `run()`, and `runServiceCommand()`. Non-Windows builds get trivial implementations (never a service; stderr logging; "Windows-only" for the service CLI). Windows builds add an `svc.Handler` that drives the existing `internal/app.Runtime`, an Event-Log-backed `slog.Handler`, and `golang.org/x/sys/windows/svc/mgr` service management.

**Tech Stack:** Go (pure, no cgo), `golang.org/x/sys/windows/svc`, `.../svc/mgr`, `.../svc/eventlog` (all already vendored via `golang.org/x/sys v0.43.0`), existing `internal/app.Runtime`.

## Global Constraints

- Pure Go, **no cgo**. `CGO_ENABLED=0` must build for every target, including
  `GOOS=windows GOARCH=amd64` and `GOOS=linux`.
- Module path `github.com/ben-kuhn/tncd/v2`.
- **No new module dependencies** — `golang.org/x/sys` is already required; use it.
- INI config format **unchanged**.
- **Unix/console parity:** on non-Windows and on interactive Windows, behavior
  is identical to today — SIGINT/SIGTERM (Unix) or Ctrl+C (Windows console)
  triggers the `internal/app.Runtime` 4-step graceful shutdown; the process
  prints `tncd running` / `Received signal, shutting down...` / `tncd stopped`.
- **Service name** is `tncd`; **display name** `tncd AGWPE-to-KISS bridge`;
  start type **Automatic (Delayed)**.
- **Start-failure rule (carried from Plan 1 final review):** the SCM handler must
  NOT reuse a `Runtime` after an `app.New` error (it returns `nil`); on start
  failure the process reports the error and exits rather than calling `Shutdown`.
  (In practice `app.New` is called in `main` before `run()`, and `main` already
  `os.Exit(1)`s on its error — so the service handler only ever receives a
  non-nil `Runtime`.)
- **Verification reality:** Task 1 has a Linux red-green test loop. Tasks 2–3 are
  Windows-only code; their gate is `CGO_ENABLED=0 GOOS=windows go build ./...`
  plus `go vet`, and a **documented manual Windows checklist** (§ Windows
  Verification) executed during the phase-4 hardware pass — NOT a Linux runtime
  test. Do not invent Linux runtime tests for SCM behavior.

## File Structure

- **Modify `cmd/tncd/main.go`** — early `service := isWindowsService()`;
  replace the inline stderr logging block with `installLogging(level, service)`;
  add `case "service":` to the subcommand switch; replace the inline
  SIGINT/SIGTERM block (current lines 231–243) with `run(r, service)`. Drop the
  now-unused `os/signal` and `syscall` imports (they move into the shims).
- **Create `cmd/tncd/run_unix.go`** (`//go:build !windows`) —
  `isWindowsService()` (false), `installLogging()` (stderr), `run()`
  (SIGINT/SIGTERM → `Shutdown`, then `Wait`). One responsibility: Unix process
  lifecycle + logging.
- **Create `cmd/tncd/service_unix.go`** (`//go:build !windows`) —
  `runServiceCommand()` returning the "Windows-only" message. One
  responsibility: the non-Windows stub of the service CLI.
- **Create `cmd/tncd/run_windows.go`** (`//go:build windows`) —
  `isWindowsService()` (via `svc.IsWindowsService()`), `installLogging()`
  (Event Log when service-hosted, else stderr), `run()` (SCM `svc.Run` when
  service-hosted, else interactive Ctrl+C), and the `svc.Handler`
  implementation. One responsibility: Windows process lifecycle + service
  handler + logging.
- **Create `cmd/tncd/eventlog_windows.go`** (`//go:build windows`) — an
  Event-Log-backed `slog.Handler`. One responsibility: slog → Windows Event Log.
- **Create `cmd/tncd/service_windows.go`** (`//go:build windows`) —
  `runServiceCommand()` implementing `install|uninstall|start|stop` via
  `svc/mgr` + `svc/eventlog`. One responsibility: SCM service management CLI.

---

### Task 1: Platform-split the process lifecycle (`run` / `installLogging` / `isWindowsService` / `runServiceCommand`)

This is the Linux-testable foundation: extract the console run loop and logging
into platform shims and wire `main.go` to them, with **no service behavior yet**
on either platform (Windows `run` is interactive-only in this task; the SCM
branch arrives in Task 2). After this task the tree builds on Windows and Linux,
and Unix/console behavior is unchanged.

**Files:**
- Modify: `cmd/tncd/main.go`
- Create: `cmd/tncd/run_unix.go`, `cmd/tncd/service_unix.go`,
  `cmd/tncd/run_windows.go`, `cmd/tncd/service_windows.go`
- Test: `cmd/tncd/run_unix_test.go`

**Interfaces:**
- Consumes: `app.New`, `(*app.Runtime).Wait`, `(*app.Runtime).Shutdown`,
  `(*app.Runtime).AGWPEAddr` (Plan 1).
- Produces (both platforms define these; Task 2/3 extend the Windows bodies):
  - `isWindowsService() bool`
  - `installLogging(level slog.Level, service bool)` — sets the default slog logger
  - `run(r *app.Runtime, service bool)` — blocks until shutdown completes
  - `runServiceCommand(args []string) int` — process exit code

- [ ] **Step 1: Write the failing test for the Unix run loop**

Create `cmd/tncd/run_unix_test.go`:

```go
//go:build !windows

package main

import (
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// TestRunUnixShutsDownOnSIGTERM verifies that run() installs a signal handler
// that drives Runtime.Shutdown, so run() returns after a SIGTERM.
func TestRunUnixShutsDownOnSIGTERM(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{ListenHost: "127.0.0.1", ListenPort: 0, Callsign: "TEST", MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 180},
		Ports: []config.Port{{
			Name: "Port 0", Type: "tcp", Host: "127.0.0.1", TCPPort: 1,
			OTABaudrate: 1200, AX25Version: 22, Reconnect: false,
		}},
	}
	r, err := app.New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := r.AGWPEAddr().String()

	done := make(chan struct{})
	go func() { run(r, false); close(done) }()

	// Wait until the listener is accepting, then signal ourselves.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener never came up: %v", derr)
		}
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `CGO_ENABLED=0 go test ./cmd/tncd/ -run TestRunUnixShutsDownOnSIGTERM -v`
Expected: FAIL — compile error, `undefined: run` (no `run_unix.go` yet).

- [ ] **Step 3: Create `cmd/tncd/run_unix.go`**

```go
//go:build !windows

package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ben-kuhn/tncd/v2/internal/app"
)

// isWindowsService is always false off Windows.
func isWindowsService() bool { return false }

// installLogging sets the default slog logger. Off Windows there is no service
// event log, so logging always goes to stderr. The service argument is ignored.
func installLogging(level slog.Level, _ bool) {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}

// run blocks until the Runtime shuts down. On Unix, SIGINT/SIGTERM triggers the
// graceful 4-step shutdown. The service argument is always false here.
func run(r *app.Runtime, _ bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("Received signal, shutting down...", "signal", sig)
		r.Shutdown()
	}()
	r.Wait()
}
```

- [ ] **Step 4: Create `cmd/tncd/service_unix.go`**

```go
//go:build !windows

package main

import (
	"fmt"
	"os"
)

// runServiceCommand handles `tncd service ...`. Service management targets the
// Windows Service Control Manager, so it is unsupported off Windows.
func runServiceCommand(_ []string) int {
	fmt.Fprintln(os.Stderr, "service management is only supported on Windows")
	return 1
}
```

- [ ] **Step 5: Create `cmd/tncd/run_windows.go` (interactive-only for now)**

```go
//go:build windows

package main

import (
	"log/slog"
	"os"
	"os/signal"

	"github.com/ben-kuhn/tncd/v2/internal/app"
	"golang.org/x/sys/windows/svc"
)

// isWindowsService reports whether the process was started by the Service
// Control Manager (vs. an interactive console).
func isWindowsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return is
}

// installLogging sets the default slog logger. The service event-log path is
// added in Task 2; for now Windows always logs to stderr.
func installLogging(level slog.Level, _ bool) {
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}

// run blocks until the Runtime shuts down. The SCM branch is added in Task 2;
// for now Windows always runs interactively (Ctrl+C).
func run(r *app.Runtime, _ bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		slog.Info("Received signal, shutting down...")
		r.Shutdown()
	}()
	r.Wait()
}
```

- [ ] **Step 6: Create `cmd/tncd/service_windows.go` (stub returns not-yet in Task 1, real in Task 3)**

Do NOT stub. Instead, provide the minimal real dispatch now so the file has one
clear job and Task 3 fills the operations. Create:

```go
//go:build windows

package main

import (
	"fmt"
	"os"
)

// runServiceCommand handles `tncd service install|uninstall|start|stop`.
// The individual operations are implemented in Task 3; this task wires the
// dispatch and usage so the command exists and reports clearly.
func runServiceCommand(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: tncd service install|uninstall|start|stop")
		return 2
	}
	switch args[0] {
	case "install", "uninstall", "start", "stop":
		fmt.Fprintf(os.Stderr, "service %s: not yet implemented\n", args[0])
		return 1
	default:
		fmt.Fprintf(os.Stderr, "unknown service command %q\n", args[0])
		return 2
	}
}
```

Note: this returns exit 1 with a clear message for the four verbs; Task 3
replaces the bodies with real `svc/mgr` calls. The message is a truthful state,
not a placeholder deliverable — the command is wired and dispatches correctly.

- [ ] **Step 7: Rewire `cmd/tncd/main.go`**

Make these edits to `cmd/tncd/main.go`:

(a) In the subcommand switch (currently `case "version"/"genconfig"/"check"`),
add before the closing brace of the switch:

```go
		case "service":
			os.Exit(runServiceCommand(os.Args[2:]))
```

(b) Replace the current logging setup block:

```go
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
```

with:

```go
	service := isWindowsService()
	installLogging(level, service)
```

(Keep the surrounding `var level slog.Level` / `switch *logLevel` block and the
`log.SetFlags(0)` / `log.SetOutput(os.Stderr)` lines that follow.)

(c) Replace the inline signal-handler block (the `sigCh := make(...)` through
`r.Wait()` and the trailing `slog.Info("tncd stopped")`, current lines 231–243)
with:

```go
	// Block until shutdown (platform-specific: Unix signals, Windows SCM or
	// console). run performs the graceful teardown before returning.
	run(r, service)
	slog.Info("tncd stopped")
```

(d) Fix imports: remove `"os/signal"` and `"syscall"` from `main.go` (now used
only in the shims). Keep `"flag"`, `"fmt"`, `"log"`, `"log/slog"`, `"os"`,
`"strings"`, and the three internal imports.

- [ ] **Step 8: Run the Unix test to verify it passes**

Run: `CGO_ENABLED=0 go test ./cmd/tncd/ -run TestRunUnixShutsDownOnSIGTERM -v`
Expected: PASS.

- [ ] **Step 9: Full Linux suite + vet**

Run: `CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go vet ./...`
Expected: all packages PASS, vet silent.

- [ ] **Step 10: Windows cross-compile + vet**

Run: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./... && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./cmd/tncd/`
Expected: exit 0, no output.

- [ ] **Step 11: Manual Unix smoke (parity check)**

```bash
printf '[server]\nlisten_host = 127.0.0.1\nlisten_port = 0\ncallsign = TEST\n\n[client.0]\ntype = tcp\nhost = 127.0.0.1\nport = 1\nreconnect = false\n' > /tmp/tncd-smoke.ini
CGO_ENABLED=0 go build -o /tmp/tncd ./cmd/tncd
/tmp/tncd -c /tmp/tncd-smoke.ini & TNCD_PID=$!
sleep 1; kill -INT $TNCD_PID; wait $TNCD_PID
```

Expected: `tncd running` … on Ctrl+C `Received signal, shutting down...` then
`tncd stopped`, clean exit.

- [ ] **Step 12: Commit**

```bash
git add cmd/tncd/main.go cmd/tncd/run_unix.go cmd/tncd/run_unix_test.go cmd/tncd/service_unix.go cmd/tncd/run_windows.go cmd/tncd/service_windows.go
git commit -m "refactor(cmd): platform shim for run/logging/service dispatch"
```

---

### Task 2: Windows service handler + Event-Log logging

Add the SCM `svc.Handler` and route logging to the Windows Event Log when
service-hosted. Windows-only; verified by cross-compile + the manual Windows
checklist, not a Linux runtime test.

**Files:**
- Create: `cmd/tncd/eventlog_windows.go`
- Modify: `cmd/tncd/run_windows.go` (add the `svc.Handler` + SCM branch in
  `run`; switch `installLogging` to Event Log when service-hosted)

**Interfaces:**
- Consumes: `(*app.Runtime).Wait`, `(*app.Runtime).Shutdown`; `isWindowsService()`.
- Produces: `serviceName` const (`"tncd"`); `type tncdService struct{ r *app.Runtime }`
  implementing `svc.Handler`; `eventLogHandler` implementing `slog.Handler`.

- [ ] **Step 1: Create `cmd/tncd/eventlog_windows.go`**

```go
//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sys/windows/svc/eventlog"
)

// eventLogHandler is a minimal slog.Handler that writes records to the Windows
// Event Log. Levels map to the three Event Log severities: Debug/Info -> Info,
// Warn -> Warning, Error -> Error. Attributes are appended as key=value pairs.
type eventLogHandler struct {
	log   *eventlog.Log
	level slog.Level
	attrs []slog.Attr
	group string
}

// newEventLogHandler opens the Event Log source registered by
// `tncd service install` (eventlog.InstallAsEventCreate). If opening fails, it
// returns an error so the caller can fall back to stderr.
func newEventLogHandler(source string, level slog.Level) (*eventLogHandler, error) {
	l, err := eventlog.Open(source)
	if err != nil {
		return nil, err
	}
	return &eventLogHandler{log: l, level: level}, nil
}

func (h *eventLogHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level
}

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	writeAttr := func(a slog.Attr) {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		fmt.Fprintf(&b, " %s=%v", key, a.Value.Any())
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool { writeAttr(a); return true })
	msg := b.String()

	const eid = 1
	switch {
	case r.Level >= slog.LevelError:
		return h.log.Error(eid, msg)
	case r.Level >= slog.LevelWarn:
		return h.log.Warning(eid, msg)
	default:
		return h.log.Info(eid, msg)
	}
}

func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *eventLogHandler) WithGroup(name string) slog.Handler {
	nh := *h
	if h.group == "" {
		nh.group = name
	} else {
		nh.group = h.group + "." + name
	}
	return &nh
}
```

- [ ] **Step 2: Update `installLogging` in `cmd/tncd/run_windows.go`**

Replace the Task-1 `installLogging` body with the Event-Log-aware version, and
add the `serviceName` const at the top of the file (below the imports):

```go
const serviceName = "tncd"

// installLogging sets the default slog logger. When the process is service-
// hosted there is no console, so logs go to the Windows Event Log; if the Event
// Log source cannot be opened (e.g. not yet registered) it falls back to stderr.
// Interactive Windows runs always log to stderr.
func installLogging(level slog.Level, service bool) {
	if service {
		if h, err := newEventLogHandler(serviceName, level); err == nil {
			slog.SetDefault(slog.New(h))
			return
		}
		// fall through to stderr on failure
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}
```

- [ ] **Step 3: Add the `svc.Handler` and SCM branch to `cmd/tncd/run_windows.go`**

Replace the Task-1 `run` function with:

```go
// run blocks until the Runtime shuts down. When service-hosted it hands control
// to the SCM via svc.Run; otherwise it runs interactively (Ctrl+C).
func run(r *app.Runtime, service bool) {
	if service {
		if err := svc.Run(serviceName, &tncdService{r: r}); err != nil {
			slog.Error("service run failed", "err", err)
			r.Shutdown()
		}
		return
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		slog.Info("Received signal, shutting down...")
		r.Shutdown()
	}()
	r.Wait()
}

// tncdService adapts internal/app.Runtime to the SCM's svc.Handler contract.
type tncdService struct{ r *app.Runtime }

// Execute runs the service: report StartPending, run the engine loop in a
// goroutine, report Running, then service Stop/Shutdown by driving the
// Runtime's graceful teardown. Interrogate echoes current status.
func (s *tncdService) Execute(_ []string, req <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	done := make(chan struct{})
	go func() { s.r.Wait(); close(done) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				s.r.Shutdown()
				<-done
				changes <- svc.Status{State: svc.Stopped}
				return false, 0
			default:
				slog.Warn("unexpected service control request", "cmd", c.Cmd)
			}
		case <-done:
			// Engine stopped on its own (e.g. fatal internal error).
			changes <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
}
```

Ensure `cmd/tncd/run_windows.go` imports include `"golang.org/x/sys/windows/svc"`
(already added in Task 1). No new imports are needed beyond what Task 1 set plus
this file already using `os`, `os/signal`, `log/slog`, `app`.

- [ ] **Step 4: Windows cross-compile + vet**

Run: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./... && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./cmd/tncd/`
Expected: exit 0, no output. (This is the gate for this task — the SCM/Event-Log
runtime behavior is verified on Windows hardware per § Windows Verification.)

- [ ] **Step 5: Confirm Linux is unaffected**

Run: `CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test ./cmd/tncd/ ./internal/app/`
Expected: PASS (no Windows files compiled into the Linux build).

- [ ] **Step 6: Commit**

```bash
git add cmd/tncd/eventlog_windows.go cmd/tncd/run_windows.go
git commit -m "feat(cmd): Windows service handler (svc.Run) + Event Log logging"
```

---

### Task 3: `tncd service install|uninstall|start|stop`

Implement the service-management CLI via `svc/mgr` and register/remove the
Event-Log source. Windows-only; cross-compile-gated + manual Windows checklist.

**Files:**
- Modify: `cmd/tncd/service_windows.go` (replace the Task-1 dispatch bodies with
  real operations)

**Interfaces:**
- Consumes: `serviceName` (Task 2).
- Produces: `runServiceCommand` fully implemented; helper `absConfigPath`.

- [ ] **Step 1: Replace `cmd/tncd/service_windows.go` with the full implementation**

```go
//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceDisplayName = "tncd AGWPE-to-KISS bridge"

// runServiceCommand handles `tncd service install|uninstall|start|stop`.
func runServiceCommand(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: tncd service install -c FILE | uninstall | start | stop")
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = svcInstall(args[1:])
	case "uninstall":
		err = svcUninstall()
	case "start":
		err = svcControl("start")
	case "stop":
		err = svcControl("stop")
	default:
		fmt.Fprintf(os.Stderr, "unknown service command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "service %s: %v\n", args[0], err)
		return 1
	}
	fmt.Printf("service %s: ok\n", args[0])
	return 0
}

// absConfigPath resolves the -c path to an absolute path. A service's working
// directory is C:\Windows\System32, so a relative config path would break; the
// installed service must always carry an absolute path.
func absConfigPath(args []string) (string, error) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	cfg := fs.String("c", "", "configuration file")
	fs.StringVar(cfg, "config", "", "configuration file (long form)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *cfg == "" {
		return "", fmt.Errorf("install requires -c FILE (absolute path recommended)")
	}
	abs, err := filepath.Abs(*cfg)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("config file %s: %w", abs, err)
	}
	return abs, nil
}

func svcInstall(args []string) error {
	cfgPath, err := absConfigPath(args)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", serviceName)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:      serviceDisplayName,
		Description:      "AGWPE-to-KISS bridge for AX.25 packet radio.",
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}, "-c", cfgPath)
	if err != nil {
		return err
	}
	defer s.Close()

	// Register the Event Log source so the service can write to it. Ignore
	// "already exists" from a prior install.
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		// Non-fatal: log to stderr; the service still runs (falls back to stderr).
		fmt.Fprintf(os.Stderr, "warning: could not register event log source: %v\n", err)
	}
	return nil
}

func svcUninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()

	// Best-effort stop before delete.
	_, _ = s.Control(svc.Stop)

	if err := s.Delete(); err != nil {
		return err
	}
	if err := eventlog.Remove(serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove event log source: %v\n", err)
	}
	return nil
}

func svcControl(action string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()

	switch action {
	case "start":
		return s.Start()
	case "stop":
		_, err := s.Control(svc.Stop)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}
```

- [ ] **Step 2: Windows cross-compile + vet**

Run: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./... && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./cmd/tncd/`
Expected: exit 0, no output.

- [ ] **Step 3: Confirm the Unix stub still governs on Linux**

Run: `CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./cmd/tncd/`
Expected: exit 0. (The `service_unix.go` "Windows-only" message is what a Linux
`tncd service ...` prints; no `svc/mgr` symbols enter the Linux build.)

- [ ] **Step 4: Commit**

```bash
git add cmd/tncd/service_windows.go
git commit -m "feat(cmd): tncd service install/uninstall/start/stop via svc/mgr"
```

---

## Windows Verification (manual, phase-4 hardware pass)

Not runnable on Linux. On a Windows 10/11 box, from an **elevated** PowerShell,
with a valid `C:\ProgramData\tncd\tncd.ini`:

1. `tncd.exe service install -c C:\ProgramData\tncd\tncd.ini` → `ok`;
   `Get-Service tncd` shows it registered, `StartType` Automatic (Delayed).
2. `tncd.exe service start` → service enters Running; `Get-Service tncd` = Running.
3. Event Viewer → Windows Logs → Application shows a `tncd` source with the
   `tncd running` / startup entries (confirms Event-Log routing).
4. Connect an AGWPE client (e.g. PAT) to `127.0.0.1:8000` → normal operation.
5. `tncd.exe service stop` (or `Stop-Service tncd`) → StopPending → Stopped,
   with a shutdown entry in the Event Log; no orphaned process.
6. `tncd.exe service uninstall` → service removed from `Get-Service`; Event-Log
   source removed.
7. Interactive sanity: double-clicking / running `tncd.exe -c ...` in a console
   still logs to the console and stops on Ctrl+C (interactive path unaffected).

---

## Self-Review

**Spec coverage (spec Component 2):** SCM handler — Task 2 (`tncdService`
`svc.Handler` + `svc.Run`). Event-Log slog handler — Task 2
(`eventlog_windows.go` + `installLogging`). `service install|uninstall|start|stop`
via `svc/mgr` with absolute `-c` path, Automatic-Delayed start type, description,
and event-log source registration — Task 3. `main_unix.go`/`main_windows.go`
platform split retaining Unix SIGINT/SIGTERM — Task 1 (named `run_unix.go` /
`run_windows.go`; the spec's "main_unix.go" naming is descriptive, not literal —
the shim files carry the split). The absolute-path working-directory gotcha and
the start-failure rule from the constraints are both handled (`absConfigPath`;
`app.New` error → `os.Exit(1)` in `main` before `run`).

**Placeholder scan:** No TBD/TODO. The Task-1 `service_windows.go` "not yet
implemented" message is a wired, dispatching command with a truthful state that
Task 3 replaces in full — not an unfinished deliverable left dangling at plan
end. Every code step is complete and compilable.

**Type consistency:** `run(r *app.Runtime, service bool)`,
`installLogging(level slog.Level, service bool)`, `isWindowsService() bool`,
`runServiceCommand(args []string) int` are defined identically in both platform
shims and called identically in `main.go`. `serviceName` (`"tncd"`) is defined
once (Task 2, `run_windows.go`) and reused in `service_windows.go` (Task 3) —
both are `//go:build windows`, same package, so the const is shared; Task 3 does
not redefine it. `tncdService.r` is `*app.Runtime` throughout.

**Green-everywhere:** Every task ends with both a Linux gate and a
`GOOS=windows` gate; Task 1 additionally carries the Unix runtime test and smoke
test for parity. The Windows runtime behavior is explicitly deferred to the
manual checklist, not faked as a Linux test.
