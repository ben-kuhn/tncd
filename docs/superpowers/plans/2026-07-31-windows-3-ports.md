# Windows Support — Plan 3: Port Enumeration + `tncd ports` + Stable USB Device Resolution

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give tncd a stable way to identify serial devices that survives Windows COM-port renumbering: an `internal/ports` package that enumerates serial ports (with USB VID/PID/serial), a `tncd ports [--json]` subcommand for the installer wizard and humans, and a `usb:VID:PID[:SERIAL]` device reference that the serial transport resolves to the live device at open time.

**Architecture:** New `internal/ports` package wraps `go.bug.st/serial/enumerator` behind an injectable seam. `List()` returns rich `Port` records; `Resolve(ref)` maps a `usb:` reference to the current OS device path (bare paths pass through unchanged). A new `tncd ports` subcommand prints `List()` as a table or JSON. The `kiss` serial transport gains an optional `Resolve` hook (nil = open device as-is) so it stays free of any `internal/ports` dependency; the bridge wires `ports.Resolve` in, and resolution runs on every `Open()` (so a USB replug re-resolves on reconnect).

**Tech Stack:** Go (pure, no cgo), `go.bug.st/serial/enumerator` (already vendored via the existing `go.bug.st/serial` dep), existing `kiss` + `internal/bridge` packages.

## Global Constraints

- Pure Go, **no cgo**. `CGO_ENABLED=0` must build for `GOOS=linux` and
  `GOOS=windows` (amd64 + arm64).
- Module path `github.com/ben-kuhn/tncd/v2`.
- **No new module dependencies** — `go.bug.st/serial` (which contains the
  `enumerator` subpackage) is already required.
- INI config format **unchanged**. `device = usb:0403:6001` and
  `device = usb:0403:6001:A50285BI` are new *values* for the existing `device`
  key — no new keys. A bare `device = COM3` / `device = /dev/ttyUSB0` must keep
  working exactly as today.
- The exported `kiss` package must **not** import `internal/ports` — the
  transport receives resolution via an injected function on `SerialConfig`.
- USB VID/PID/serial matching is **case-insensitive**.
- This whole plan is Linux-testable via an injected enumerator seam; there are
  no Windows-only runtime gaps here. Bluetooth enumeration is out of scope
  (added in Plan 4 with the Bluetooth transport).

## File Structure

- **Create `internal/ports/ports.go`** — `Port` struct, `List()`, `Resolve()`,
  `usbRef`/`parseUSBRef`, and the `detailedPorts` enumerator seam. One
  responsibility: discover and resolve devices.
- **Create `internal/ports/ports_test.go`** — table-driven tests over a fake
  `detailedPorts`.
- **Create `cmd/tncd/ports.go`** (no build tag; cross-platform) — `runPorts`
  subcommand and a pure `formatPorts(ps, jsonOut) (string, error)`.
- **Create `cmd/tncd/ports_test.go`** — tests for `formatPorts` (table + JSON).
- **Modify `cmd/tncd/main.go`** — add `case "ports":` to the subcommand switch;
  add the `ports` line to the usage text.
- **Modify `kiss/serial.go`** — add `Resolve func(string)(string,error)` to
  `SerialConfig`; call it at the top of `Open()` and open the resolved device.
- **Modify `kiss/serial_test.go`** — add a test proving the resolved device is
  what gets opened (and that a resolve error aborts `Open`).
- **Modify `internal/bridge/transport.go`** — set `Resolve: ports.Resolve` when
  building the serial transport.

---

### Task 1: `internal/ports` — enumerate + resolve

**Files:**
- Create: `internal/ports/ports.go`, `internal/ports/ports_test.go`

**Interfaces:**
- Consumes: `go.bug.st/serial/enumerator` (`GetDetailedPortsList`, `PortDetails`).
- Produces:
  - `type Port struct { Ref, Label, Kind, Device, VID, PID, Serial string }` (with JSON tags)
  - `const KindSerial = "serial"`, `KindBluetooth = "bluetooth"`
  - `func List() ([]Port, error)`
  - `func Resolve(ref string) (string, error)`
  - `var detailedPorts func() ([]*enumerator.PortDetails, error)` (test seam)

- [ ] **Step 1: Write the failing tests**

Create `internal/ports/ports_test.go`:

```go
package ports

import (
	"strings"
	"testing"

	"go.bug.st/serial/enumerator"
)

func withFakePorts(t *testing.T, ds []*enumerator.PortDetails) {
	t.Helper()
	orig := detailedPorts
	detailedPorts = func() ([]*enumerator.PortDetails, error) { return ds, nil }
	t.Cleanup(func() { detailedPorts = orig })
}

func TestListUSBAndPlainPorts(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM3", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "A50285BI", Product: "FT232R USB UART"},
		{Name: "COM1", IsUSB: false},
	})
	ps, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("want 2 ports, got %d", len(ps))
	}
	// Sorted by Device: COM1 before COM3.
	if ps[0].Device != "COM1" || ps[0].Ref != "COM1" || ps[0].Kind != KindSerial {
		t.Errorf("plain port wrong: %+v", ps[0])
	}
	if ps[1].Ref != "usb:0403:6001:A50285BI" {
		t.Errorf("usb ref = %q, want usb:0403:6001:A50285BI", ps[1].Ref)
	}
	if !strings.Contains(ps[1].Label, "FT232R") || !strings.Contains(ps[1].Label, "COM3") {
		t.Errorf("usb label = %q, want it to mention product + COM3", ps[1].Label)
	}
}

func TestListUSBNoSerialNumber(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM5", IsUSB: true, VID: "10C4", PID: "EA60", Product: "CP2102"},
	})
	ps, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if ps[0].Ref != "usb:10c4:ea60" {
		t.Errorf("ref = %q, want usb:10c4:ea60 (lowercased, no serial)", ps[0].Ref)
	}
}

func TestResolvePassthrough(t *testing.T) {
	// No enumerator call needed for bare device paths.
	for _, dev := range []string{"COM3", "/dev/ttyUSB0", "/dev/serial/by-id/usb-FTDI"} {
		got, err := Resolve(dev)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", dev, err)
		}
		if got != dev {
			t.Errorf("Resolve(%q) = %q, want passthrough", dev, got)
		}
	}
}

func TestResolveUSBSingleMatch(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "A50285BI"},
		{Name: "COM2", IsUSB: false},
	})
	got, err := Resolve("usb:0403:6001")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "COM7" {
		t.Errorf("got %q, want COM7", got)
	}
}

func TestResolveUSBNoMatch(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "1234", PID: "5678"},
	})
	if _, err := Resolve("usb:0403:6001"); err == nil {
		t.Fatal("want error for no match, got nil")
	}
}

func TestResolveUSBAmbiguousWantsSerial(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "AAAA"},
		{Name: "COM8", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "BBBB"},
	})
	_, err := Resolve("usb:0403:6001")
	if err == nil {
		t.Fatal("want ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "serial number") {
		t.Errorf("error should suggest adding a serial number, got: %v", err)
	}
}

func TestResolveUSBWithSerialDisambiguates(t *testing.T) {
	withFakePorts(t, []*enumerator.PortDetails{
		{Name: "COM7", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "AAAA"},
		{Name: "COM8", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "BBBB"},
	})
	got, err := Resolve("usb:0403:6001:bbbb") // case-insensitive
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "COM8" {
		t.Errorf("got %q, want COM8", got)
	}
}

func TestResolveBadRef(t *testing.T) {
	if _, err := Resolve("usb:0403"); err == nil {
		t.Fatal("want error for malformed ref, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `CGO_ENABLED=0 go test ./internal/ports/ -v`
Expected: FAIL — compile error, `undefined: List` / `Resolve` / `detailedPorts` etc.

- [ ] **Step 3: Write `internal/ports/ports.go`**

```go
// Package ports discovers serial (and, later, Bluetooth) devices and resolves
// stable device references to the concrete OS device path. A "usb:VID:PID" or
// "usb:VID:PID:SERIAL" reference survives Windows COM-port renumbering, since it
// is matched against the currently-attached USB serial ports at open time.
package ports

import (
	"fmt"
	"sort"
	"strings"

	"go.bug.st/serial/enumerator"
)

// Kind values for Port.
const (
	KindSerial    = "serial"
	KindBluetooth = "bluetooth"
)

// Port describes a device that can back a KISS TNC connection.
type Port struct {
	Ref    string `json:"ref"`              // stable value to write into config `device = ...`
	Label  string `json:"label"`            // human-readable label for the wizard/list
	Kind   string `json:"kind"`             // KindSerial | KindBluetooth
	Device string `json:"device"`           // current OS device path (COMx / /dev/ttyUSB0)
	VID    string `json:"vid,omitempty"`    // USB vendor id (USB serial only)
	PID    string `json:"pid,omitempty"`    // USB product id (USB serial only)
	Serial string `json:"serial,omitempty"` // USB serial number (when available)
}

// detailedPorts is the serial enumerator, indirected so tests can fake it.
var detailedPorts = func() ([]*enumerator.PortDetails, error) {
	return enumerator.GetDetailedPortsList()
}

// List returns all discoverable serial ports. USB ports get a "usb:VID:PID[:SERIAL]"
// Ref; other ports use their device path as the Ref. Results are sorted by
// device path for stable output.
func List() ([]Port, error) {
	ds, err := detailedPorts()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	out := make([]Port, 0, len(ds))
	for _, d := range ds {
		p := Port{Kind: KindSerial, Device: d.Name}
		if d.IsUSB && d.VID != "" && d.PID != "" {
			p.VID = strings.ToLower(d.VID)
			p.PID = strings.ToLower(d.PID)
			p.Serial = d.SerialNumber
			p.Ref = usbRef(p.VID, p.PID, p.Serial)
			name := d.Product
			if name == "" {
				name = "USB serial"
			}
			p.Label = fmt.Sprintf("USB: %s (%s)", name, d.Name)
		} else {
			p.Ref = d.Name
			p.Label = fmt.Sprintf("Serial: %s", d.Name)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, nil
}

// usbRef builds a "usb:vid:pid[:serial]" reference.
func usbRef(vid, pid, serial string) string {
	if serial != "" {
		return fmt.Sprintf("usb:%s:%s:%s", vid, pid, serial)
	}
	return fmt.Sprintf("usb:%s:%s", vid, pid)
}

// Resolve turns a config device reference into a concrete OS device path.
// A "usb:VID:PID[:SERIAL]" reference is matched (case-insensitively) against
// currently-attached USB serial ports. Any other value — a bare COMx, a
// /dev/tty* path, etc. — is returned unchanged.
func Resolve(ref string) (string, error) {
	if !strings.HasPrefix(ref, "usb:") {
		return ref, nil
	}
	vid, pid, serial, err := parseUSBRef(ref)
	if err != nil {
		return "", err
	}
	ds, err := detailedPorts()
	if err != nil {
		return "", fmt.Errorf("enumerate serial ports: %w", err)
	}
	var matches []string
	for _, d := range ds {
		if !d.IsUSB {
			continue
		}
		if !strings.EqualFold(d.VID, vid) || !strings.EqualFold(d.PID, pid) {
			continue
		}
		if serial != "" && !strings.EqualFold(d.SerialNumber, serial) {
			continue
		}
		matches = append(matches, d.Name)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no USB serial port matching %s (is the device plugged in?)", ref)
	case 1:
		return matches[0], nil
	default:
		if serial == "" {
			return "", fmt.Errorf("multiple USB serial ports match %s (%s); add a serial number: usb:%s:%s:SERIAL",
				ref, strings.Join(matches, ", "), vid, pid)
		}
		return "", fmt.Errorf("multiple USB serial ports match %s (%s)", ref, strings.Join(matches, ", "))
	}
}

// parseUSBRef parses "usb:vid:pid" or "usb:vid:pid:serial". The serial number
// may itself contain colons, so only the first two colons are structural.
func parseUSBRef(ref string) (vid, pid, serial string, err error) {
	body := strings.TrimPrefix(ref, "usb:")
	parts := strings.SplitN(body, ":", 3)
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			break
		}
		return parts[0], parts[1], "", nil
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			break
		}
		return parts[0], parts[1], parts[2], nil
	}
	return "", "", "", fmt.Errorf("invalid usb device reference %q (want usb:VID:PID or usb:VID:PID:SERIAL)", ref)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `CGO_ENABLED=0 go test ./internal/ports/ -v`
Expected: PASS (all 8 tests).

- [ ] **Step 5: Windows cross-compile**

Run: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./internal/ports/`
Expected: exit 0.

- [ ] **Step 6: Commit**

```bash
git add internal/ports/ports.go internal/ports/ports_test.go
git commit -m "feat(ports): serial enumeration + usb:VID:PID device resolution"
```

---

### Task 2: `tncd ports [--json]` subcommand

**Files:**
- Create: `cmd/tncd/ports.go`, `cmd/tncd/ports_test.go`
- Modify: `cmd/tncd/main.go`

**Interfaces:**
- Consumes: `ports.List`, `ports.Port` (Task 1).
- Produces: `func runPorts(args []string) int`; `func formatPorts(ps []ports.Port, jsonOut bool) (string, error)`.

- [ ] **Step 1: Write the failing test**

Create `cmd/tncd/ports_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/ports"
)

func samplePorts() []ports.Port {
	return []ports.Port{
		{Ref: "COM1", Label: "Serial: COM1", Kind: "serial", Device: "COM1"},
		{Ref: "usb:0403:6001:A50285BI", Label: "USB: FT232R USB UART (COM3)", Kind: "serial",
			Device: "COM3", VID: "0403", PID: "6001", Serial: "A50285BI"},
	}
}

func TestFormatPortsTable(t *testing.T) {
	out, err := formatPorts(samplePorts(), false)
	if err != nil {
		t.Fatalf("formatPorts: %v", err)
	}
	if !strings.Contains(out, "usb:0403:6001:A50285BI") {
		t.Errorf("table missing usb ref:\n%s", out)
	}
	if !strings.Contains(out, "FT232R") {
		t.Errorf("table missing label:\n%s", out)
	}
	if !strings.Contains(out, "COM1") {
		t.Errorf("table missing plain port:\n%s", out)
	}
}

func TestFormatPortsJSON(t *testing.T) {
	out, err := formatPorts(samplePorts(), true)
	if err != nil {
		t.Fatalf("formatPorts: %v", err)
	}
	if !strings.Contains(out, `"ref": "usb:0403:6001:A50285BI"`) {
		t.Errorf("json missing ref field:\n%s", out)
	}
	if !strings.Contains(out, `"kind": "serial"`) {
		t.Errorf("json missing kind field:\n%s", out)
	}
}

func TestFormatPortsEmptyTable(t *testing.T) {
	out, err := formatPorts(nil, false)
	if err != nil {
		t.Fatalf("formatPorts: %v", err)
	}
	if !strings.Contains(out, "no serial ports found") {
		t.Errorf("want empty-table message, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./cmd/tncd/ -run TestFormatPorts -v`
Expected: FAIL — `undefined: formatPorts`.

- [ ] **Step 3: Create `cmd/tncd/ports.go`**

```go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/ben-kuhn/tncd/v2/internal/ports"
)

// runPorts implements `tncd ports [--json]`: list serial (and, later, Bluetooth)
// devices with a stable reference to put in the config `device =` key.
func runPorts(args []string) int {
	fs := flag.NewFlagSet("ports", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ps, err := ports.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out, err := formatPorts(ps, *jsonOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(out)
	return 0
}

// formatPorts renders the port list as an aligned table or as JSON. It is a
// pure function (no I/O) so it can be unit-tested.
func formatPorts(ps []ports.Port, jsonOut bool) (string, error) {
	if jsonOut {
		b, err := json.MarshalIndent(ps, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	}
	if len(ps) == 0 {
		return "no serial ports found\n", nil
	}
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tREF\tLABEL")
	for _, p := range ps {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Device, p.Ref, p.Label)
	}
	tw.Flush()
	return sb.String(), nil
}
```

Note: add `"strings"` to the import block above (used by `strings.Builder`).
The final import list for `cmd/tncd/ports.go` is: `encoding/json`, `flag`,
`fmt`, `os`, `strings`, `text/tabwriter`, and `internal/ports`.

- [ ] **Step 4: Wire the subcommand in `cmd/tncd/main.go`**

(a) In the subcommand switch, add:

```go
		case "ports":
			os.Exit(runPorts(os.Args[2:]))
```

(b) In `fs.Usage`, add a line to the Subcommands block (after the `check` line):

```go
		fmt.Fprintf(os.Stderr, "  ports [--json]  List serial devices and their stable references\n")
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `CGO_ENABLED=0 go test ./cmd/tncd/ -run TestFormatPorts -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Full suite + Windows build**

Run: `CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`
Expected: all PASS; Windows build exit 0.

- [ ] **Step 7: Manual smoke (lists this machine's ports)**

```bash
CGO_ENABLED=0 go build -o /tmp/tncd ./cmd/tncd
/tmp/tncd ports
/tmp/tncd ports --json
```

Expected: a table (or `no serial ports found`) and valid JSON. No crash.

- [ ] **Step 8: Commit**

```bash
git add cmd/tncd/ports.go cmd/tncd/ports_test.go cmd/tncd/main.go
git commit -m "feat(cmd): tncd ports subcommand (table + JSON)"
```

---

### Task 3: Resolve `usb:` references in the serial transport

**Files:**
- Modify: `kiss/serial.go`, `kiss/serial_test.go`, `internal/bridge/transport.go`

**Interfaces:**
- Consumes: `ports.Resolve` (Task 1).
- Produces: `SerialConfig.Resolve func(string) (string, error)` (optional field;
  nil = open `Device` unchanged).

- [ ] **Step 1: Write the failing test**

Add to `kiss/serial_test.go`:

```go
func TestSerialOpenResolvesDevice(t *testing.T) {
	var opened string
	tr := &serialTransport{
		cfg: SerialConfig{
			Device: "usb:0403:6001",
			Baud:   9600,
			Resolve: func(ref string) (string, error) {
				if ref != "usb:0403:6001" {
					t.Errorf("Resolve got %q", ref)
				}
				return "/dev/ttyUSB9", nil
			},
		},
		probeWait: time.Millisecond,
		openPort: func(device string, mode *goserial.Mode) (modemPort, error) {
			opened = device
			return &fakeModemPort{}, nil
		},
	}
	if err := tr.Open(); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != "/dev/ttyUSB9" {
		t.Errorf("opened %q, want the resolved /dev/ttyUSB9", opened)
	}
}

func TestSerialOpenResolveError(t *testing.T) {
	tr := &serialTransport{
		cfg: SerialConfig{
			Device:  "usb:0403:6001",
			Baud:    9600,
			Resolve: func(string) (string, error) { return "", errors.New("not plugged in") },
		},
		probeWait: time.Millisecond,
		openPort: func(device string, mode *goserial.Mode) (modemPort, error) {
			t.Fatal("openPort should not be called when Resolve fails")
			return nil, nil
		},
	}
	if err := tr.Open(); err == nil {
		t.Fatal("want error when Resolve fails, got nil")
	}
}
```

Note: `kiss/serial_test.go` already defines `type fakeModemPort struct{ ... }`
with **pointer** receivers (fields `dtrErr`/`rtsErr`), and already imports
`errors`, `time`, and `goserial "go.bug.st/serial"`. Reuse it via
`&fakeModemPort{}` (as shown above) — do NOT add a second definition, and do NOT
add already-present imports.

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./kiss/ -run TestSerialOpenResolve -v`
Expected: FAIL — `unknown field 'Resolve' in struct literal of type SerialConfig`.

- [ ] **Step 3: Add the `Resolve` field to `SerialConfig`**

In `kiss/serial.go`, add to the `SerialConfig` struct (after `ExitDelay`):

```go
	// Resolve, if set, maps Device (which may be a stable reference like
	// "usb:VID:PID") to the concrete OS device path to open. It is called on
	// every Open, so a device that moved to a different USB port is re-resolved
	// on reconnect. nil means open Device unchanged.
	Resolve func(ref string) (device string, err error)
```

- [ ] **Step 4: Call `Resolve` at the top of `Open()`**

In `kiss/serial.go`, inside `Open()`, replace the `port, err := openFn(s.cfg.Device, mode)`
call and the two immediately-following error messages so device resolution
happens first. Concretely, immediately before the `openFn := s.openPort` block,
insert:

```go
	// Resolve a stable device reference (e.g. "usb:VID:PID") to the live OS
	// device path. Runs on every Open so a USB replug re-resolves on reconnect.
	device := s.cfg.Device
	if s.cfg.Resolve != nil {
		resolved, rerr := s.cfg.Resolve(device)
		if rerr != nil {
			return fmt.Errorf("serial: resolve %q: %w", device, rerr)
		}
		if resolved != device {
			log.Printf("serial: resolved %s -> %s", device, resolved)
		}
		device = resolved
	}
```

Then change the open call and its error messages to use `device` instead of
`s.cfg.Device`:

```go
	port, err := openFn(device, mode)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("serial: cannot open %s: port is busy (in use or exclusively locked) — free it and retry", device)
		}
		return fmt.Errorf("serial: open %s: %w", device, err)
	}
```

Leave the later `SetDTR`/`SetRTS` log lines that reference `s.cfg.Device` as-is
(they are cosmetic); do not introduce unused-variable issues.

- [ ] **Step 5: Run the serial tests**

Run: `CGO_ENABLED=0 go test ./kiss/ -v`
Expected: PASS, including the two new tests and all pre-existing serial tests
(the nil-`Resolve` default preserves current behavior).

- [ ] **Step 6: Wire `ports.Resolve` into the bridge**

In `internal/bridge/transport.go`, add the import
`"github.com/ben-kuhn/tncd/v2/internal/ports"` and set the field in the serial
case:

```go
		case "serial":
			return kiss.NewSerialTransport(kiss.SerialConfig{
				Device:         pc.Device,
				Baud:           pc.SerialBaudrate,
				Parity:         pc.Parity,
				StopBits:       pc.StopBits,
				RTSCTS:         pc.RTSCTS,
				InitString:     pc.InitString,
				InitDelay:      time.Duration(pc.InitDelay * float64(time.Second)),
				SendKISSExit:   pc.SendKISSExit,
				HostExitString: pc.HostExitString,
				ExitDelay:      time.Duration(pc.ExitDelay * float64(time.Second)),
				Resolve:        ports.Resolve,
			}), nil
```

- [ ] **Step 7: Full suite, vet, and both-platform builds**

Run:
```
CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./... && CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...
```
Expected: all PASS; both Windows builds exit 0.

- [ ] **Step 8: Commit**

```bash
git add kiss/serial.go kiss/serial_test.go internal/bridge/transport.go
git commit -m "feat(kiss): resolve usb: device refs at serial Open; wire ports.Resolve"
```

---

## Self-Review

**Spec coverage (spec Component 3):** `internal/ports` with `List()` and
`Resolve()` — Task 1. `tncd ports [--json]` reading `List()` — Task 2. USB
`VID:PID(:serial)` stable identity resolved to the live `COMx`/dev at open time,
with bare paths passing through — Tasks 1 + 3. The spec's "runtime uses
`Resolve()` in the serial transport at startup" and "logs `resolved
usb:... -> COMx`" — Task 3 (the `log.Printf("serial: resolved ...")` line; runs
on every Open, satisfying the reconnect-after-replug intent). The exported-`kiss`
layering constraint is honored via the injected `SerialConfig.Resolve` hook
rather than a `kiss → internal/ports` import. Bluetooth enumeration is explicitly
deferred to Plan 4 (spec implementation order step 4), so `List()` returns serial
ports only here; the `KindBluetooth` constant is defined now so Plan 4 only adds
a backend.

**Placeholder scan:** No TBD/TODO. Every code step is complete and compilable.
The `ports.go` import note (adding `"strings"`) is explicit. Task 3 Step 1 flags
the pre-existing-fake check with a concrete fallback rather than assuming.

**Type consistency:** `Port` fields (`Ref/Label/Kind/Device/VID/PID/Serial`) are
used identically in `ports.go`, `ports_test.go`, and `formatPorts`. `List()
([]Port, error)`, `Resolve(string) (string, error)`, and
`SerialConfig.Resolve func(string)(string,error)` match across producer
(Task 1), CLI (Task 2), transport (Task 3), and bridge wiring (Task 3). The
`detailedPorts` seam has one signature (`func() ([]*enumerator.PortDetails,
error)`) used by both `List` and `Resolve` and overridden by the test helper.

**Green-everywhere:** Every task ends with a Linux test run and a `GOOS=windows`
build; Task 3 adds `arm64`. Nothing here is Windows-only, so there is no deferred
runtime gap in this plan.
