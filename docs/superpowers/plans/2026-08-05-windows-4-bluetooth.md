# Windows Support — Plan 4: Bluetooth SPP via Winsock RFCOMM (AF_BTH)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Windows a real, in-process Bluetooth SPP KISS transport (parity with the Linux BlueZ path) so `type = bluetooth` / `bdaddr = <MAC>` connects to a paired TNC (e.g. Mobilinkd) directly over Winsock RFCOMM — no virtual COM port.

**Architecture:** A new `//go:build windows` transport `kiss/bluetooth_windows.go` implements the existing `kiss.Transport` interface using `golang.org/x/sys/windows`'s Winsock: `Socket(AF_BTH, SOCK_STREAM, BTHPROTO_RFCOMM)` → `Connect(&windows.SockaddrBth{...})` with the SPP service UUID (which makes Windows do the SDP channel lookup), then blocking `WSARecv`/`WSASend`. Config parity with Linux (`bdaddr` MAC). `BluetoothConfig` moves to a shared, build-tag-free file so all three platform variants (linux/windows/other) share one definition. Reconnect/backoff is already handled generically by the bridge (Plan 3.5).

**Tech Stack:** Go (pure, no cgo), `golang.org/x/sys/windows` (already vendored; provides `SockaddrBth`, `AF_BTH`, `BTHPROTO_RFCOMM`, `Socket`, `Connect`, `WSARecv`, `WSASend`, `Closesocket`, `WSAStartup`, `WSACleanup`, `WSABuf`, `GUID`).

## Global Constraints

- Pure Go, **no cgo**. `CGO_ENABLED=0` must build `GOOS=linux`, `GOOS=windows`
  (amd64+arm64), and (via the stub) `GOOS=darwin`/`GOOS=freebsd`.
- Module path `github.com/ben-kuhn/tncd/v2`; **no new module dependencies**
  (`golang.org/x/sys` already vendored).
- INI config format **unchanged** and **at parity with Linux Bluetooth**:
  `type = bluetooth`, `bdaddr = AA:BB:CC:DD:EE:FF`, optional `channel`
  (informational — the SPP UUID drives SDP), `reconnect*`. **No `device = bt:`
  scheme in this plan** — that is a deliberate divergence from the umbrella
  spec's literal wording, chosen for parity with the already-merged Linux BT
  transport (which uses `bdaddr`). Name→MAC discovery for the installer wizard
  is deferred to Plan 5.
- **Green on every platform after each task.** Task 1 keeps `bluetooth_stub.go`
  covering Windows (so Windows still builds via the stub). Task 2 adds the real
  Windows transport *and* retags the stub to `!linux && !windows` in the same
  commit, so there is never a duplicate or missing `NewBluetoothTransport`.
- **Runtime verification is on the Windows VM**, not Linux. Each Windows task's
  Linux gate is `GOOS=windows` build+vet; the real connect/KISS round-trip runs
  on the dedicated Windows test VM with a paired Mobilinkd (see § VM Validation).
  The byte-order of the parsed `BtAddr` and the SDP-driven connect are the two
  things only the VM can confirm.

## File Structure

- **Create `kiss/bluetooth.go`** (no build tag) — the shared `BluetoothConfig`
  struct, used by all platform variants.
- **Modify `kiss/bluetooth_linux.go`** — remove its local `BluetoothConfig`
  definition (now shared).
- **Modify `kiss/bluetooth_stub.go`** — remove its local `BluetoothConfig`
  definition; Task 2 retags it from `!linux` to `!linux && !windows`.
- **Create `kiss/bluetooth_windows.go`** (`//go:build windows`) — the Winsock
  RFCOMM transport + `NewBluetoothTransport` + `parseBTAddr`.
- **Create `kiss/bluetooth_windows_test.go`** (`//go:build windows`) — a unit
  test for `parseBTAddr` (the one piece with no socket dependency).

No bridge or config changes: `internal/bridge/transport.go` already builds
`kiss.NewBluetoothTransport(kiss.BluetoothConfig{BDAddr: pc.BDAddr, ...})` for
`type = bluetooth`; on Windows that now returns the real transport.

---

### Task 1: Share `BluetoothConfig` across platforms (refactor)

Pure refactor — no behavior change. Keeps `bluetooth_stub.go` covering Windows,
so all platforms (including Windows via the stub) stay green.

**Files:**
- Create: `kiss/bluetooth.go`
- Modify: `kiss/bluetooth_linux.go`, `kiss/bluetooth_stub.go`

**Interfaces:**
- Produces: `kiss.BluetoothConfig` (moved, not changed) in a build-tag-free file.

- [ ] **Step 1: Create the shared config file**

Create `kiss/bluetooth.go`:

```go
package kiss

import "time"

// BluetoothConfig holds the configuration for a Bluetooth SPP KISS transport.
// It is shared by all platform implementations (Linux BlueZ, Windows Winsock
// RFCOMM, and the unsupported-platform stub).
type BluetoothConfig struct {
	BDAddr            string
	Channel           string // informational; the SPP profile UUID drives connection
	Reconnect         bool
	ReconnectDelay    time.Duration
	ReconnectMaxDelay time.Duration
}
```

- [ ] **Step 2: Remove the duplicate from `bluetooth_linux.go`**

In `kiss/bluetooth_linux.go`, delete the `BluetoothConfig` struct definition
(the `type BluetoothConfig struct { ... }` block and its doc comment, currently
around lines 22-30). Leave the rest of the file (and its `//go:build linux` tag)
unchanged. If `time` is no longer referenced in that file, remove the `time`
import; if it is still used elsewhere in the file, keep it.

- [ ] **Step 3: Remove the duplicate from `bluetooth_stub.go`**

In `kiss/bluetooth_stub.go`, delete the `BluetoothConfig` struct definition and
its doc comment. **Keep the build tag `//go:build !linux` for now** (Task 2
retags it). Remove the `time` import if it is now unused (the stub's methods
don't use `time`), keep `fmt`.

- [ ] **Step 4: Build + test every platform**

Run:
```
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test ./kiss/
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build ./...
```
Expected: Linux build+tests PASS; windows/darwin/freebsd builds exit 0 (all use
the stub for Bluetooth, now referencing the shared `BluetoothConfig`).

- [ ] **Step 5: Commit**

```bash
git add kiss/bluetooth.go kiss/bluetooth_linux.go kiss/bluetooth_stub.go
git commit -m "refactor(kiss): share BluetoothConfig across platform variants"
```

---

### Task 2: Windows Bluetooth SPP transport (AF_BTH RFCOMM)

**Files:**
- Create: `kiss/bluetooth_windows.go`, `kiss/bluetooth_windows_test.go`
- Modify: `kiss/bluetooth_stub.go` (retag to exclude Windows)

**Interfaces:**
- Consumes: `kiss.BluetoothConfig` (Task 1); `golang.org/x/sys/windows`.
- Produces (Windows only): `NewBluetoothTransport(cfg BluetoothConfig) Transport`
  and `parseBTAddr(string) (uint64, error)`.

- [ ] **Step 1: Write the failing test for `parseBTAddr`**

Create `kiss/bluetooth_windows_test.go`:

```go
//go:build windows

package kiss

import "testing"

func TestParseBTAddr(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"00:11:22:33:44:55", 0x001122334455, true},
		{"AA:BB:CC:DD:EE:FF", 0xAABBCCDDEEFF, true},
		{"aa-bb-cc-dd-ee-ff", 0xAABBCCDDEEFF, true}, // dashes + lowercase
		{"001122334455", 0x001122334455, true},      // no separators
		{"00:11:22:33:44", 0, false},                // too short
		{"zz:11:22:33:44:55", 0, false},             // non-hex
	}
	for _, c := range cases {
		got, err := parseBTAddr(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseBTAddr(%q) = %#x, %v; want %#x, nil", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseBTAddr(%q) = %#x, nil; want error", c.in, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails (on the Windows target)**

Run: `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./kiss/`
Expected: FAIL — `undefined: parseBTAddr` (the windows transport file doesn't
exist yet). (The test is `//go:build windows`, so it can't run on this Linux
host; `go vet` for the windows target is the fail/pass signal.)

- [ ] **Step 3: Create `kiss/bluetooth_windows.go`**

```go
//go:build windows

package kiss

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// sppServiceClassID is the Bluetooth Serial Port Profile UUID
// {00001101-0000-1000-8000-00805F9B34FB}. Passing it as the connect
// ServiceClassId makes Windows resolve the RFCOMM channel via SDP — the
// equivalent of the Linux path's SDP auto-detect.
var sppServiceClassID = windows.GUID{
	Data1: 0x00001101,
	Data2: 0x0000,
	Data3: 0x1000,
	Data4: [8]byte{0x80, 0x00, 0x00, 0x80, 0x5F, 0x9B, 0x34, 0xFB},
}

// bluetoothTransport is a Windows Bluetooth SPP transport over Winsock RFCOMM.
type bluetoothTransport struct {
	cfg     BluetoothConfig
	mu      sync.Mutex
	fd      windows.Handle
	open    bool
	started bool // WSAStartup succeeded and needs WSACleanup
}

// NewBluetoothTransport returns a Windows Bluetooth SPP transport. It connects
// to cfg.BDAddr; the SPP service UUID drives SDP channel discovery, so
// cfg.Channel is informational only. The device must already be paired in
// Windows.
func NewBluetoothTransport(cfg BluetoothConfig) Transport {
	return &bluetoothTransport{cfg: cfg, fd: windows.InvalidHandle}
}

func (bt *bluetoothTransport) Open() error {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	addr, err := parseBTAddr(bt.cfg.BDAddr)
	if err != nil {
		return fmt.Errorf("bluetooth: %w", err)
	}

	var wsad windows.WSAData
	if err := windows.WSAStartup(0x202, &wsad); err != nil { // MAKEWORD(2,2)
		return fmt.Errorf("bluetooth: WSAStartup: %w", err)
	}
	bt.started = true

	fd, err := windows.Socket(windows.AF_BTH, windows.SOCK_STREAM, windows.BTHPROTO_RFCOMM)
	if err != nil {
		windows.WSACleanup()
		bt.started = false
		return fmt.Errorf("bluetooth: socket: %w", err)
	}

	sa := &windows.SockaddrBth{
		BtAddr:         addr,
		ServiceClassId: sppServiceClassID,
		Port:           0, // 0 + ServiceClassId => SDP channel lookup
	}
	if err := windows.Connect(fd, sa); err != nil {
		windows.Closesocket(fd)
		windows.WSACleanup()
		bt.started = false
		return fmt.Errorf("bluetooth: connect %s: %w", bt.cfg.BDAddr, err)
	}

	bt.fd = fd
	bt.open = true
	return nil
}

func (bt *bluetoothTransport) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
	var recvd, flags uint32
	if err := windows.WSARecv(bt.fd, &buf, 1, &recvd, &flags, nil, nil); err != nil {
		return 0, err
	}
	if recvd == 0 {
		return 0, io.EOF
	}
	return int(recvd), nil
}

func (bt *bluetoothTransport) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := windows.WSABuf{Len: uint32(len(p)), Buf: &p[0]}
	var sent uint32
	if err := windows.WSASend(bt.fd, &buf, 1, &sent, 0, nil, nil); err != nil {
		return 0, err
	}
	return int(sent), nil
}

// Close closes the socket (unblocking any in-flight WSARecv in the reader
// goroutine) and releases the Winsock refcount.
func (bt *bluetoothTransport) Close() error {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if bt.open {
		windows.Closesocket(bt.fd)
		bt.fd = windows.InvalidHandle
		bt.open = false
	}
	if bt.started {
		windows.WSACleanup()
		bt.started = false
	}
	return nil
}

func (bt *bluetoothTransport) EnterKISS() error { return nil }
func (bt *bluetoothTransport) ExitKISS()        {}

// parseBTAddr parses "AA:BB:CC:DD:EE:FF" (colons or dashes, any case, or no
// separators) into a BTH_ADDR: the 48-bit address in the low 6 bytes of a
// uint64, with AA as the most-significant octet.
func parseBTAddr(s string) (uint64, error) {
	h := strings.ReplaceAll(s, ":", "")
	h = strings.ReplaceAll(h, "-", "")
	if len(h) != 12 {
		return 0, fmt.Errorf("invalid Bluetooth address %q (want AA:BB:CC:DD:EE:FF)", s)
	}
	v, err := strconv.ParseUint(h, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Bluetooth address %q: %w", s, err)
	}
	return v, nil
}
```

- [ ] **Step 4: Retag the stub to exclude Windows**

In `kiss/bluetooth_stub.go`, change the first line from:

```go
//go:build !linux
```

to:

```go
//go:build !linux && !windows
```

(Now: Linux → BlueZ; Windows → the new real transport; everything else → stub.
Exactly one `NewBluetoothTransport` per platform.)

- [ ] **Step 5: Windows vet/build (test now passes) + other platforms**

Run:
```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./kiss/
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build ./...
```
Expected: windows vet clean (TestParseBTAddr compiles; `parseBTAddr` defined);
all builds exit 0; Linux full suite PASS; darwin/freebsd still build via the
stub. (The windows-tagged `TestParseBTAddr` cannot execute on this Linux host —
it runs on the VM in § VM Validation; `go vet` confirms it compiles for the
target.)

- [ ] **Step 6: Commit**

```bash
git add kiss/bluetooth_windows.go kiss/bluetooth_windows_test.go kiss/bluetooth_stub.go
git commit -m "feat(kiss): Windows Bluetooth SPP transport via Winsock RFCOMM (AF_BTH)"
```

---

## VM Validation (dedicated Windows test VM; not runnable on Linux)

Run once the VM test rig is up (snapshot first). With `tncd.exe` copied in and a
Mobilinkd TNC **paired in Windows Settings**:

1. `go test` the windows unit test on the guest: `go test ./kiss/ -run TestParseBTAddr`
   (or run the prebuilt test binary) → `parseBTAddr` byte-order sanity.
2. Find the TNC's MAC (Windows Settings → Bluetooth → device details, or
   `Get-PnpDevice`/`btpair`), write a config:
   ```ini
   [client.0]
   type = bluetooth
   bdaddr = <MAC>
   ```
3. Run `tncd.exe -c tncd.ini -v`. Expect an AGWPE listener up and the bridge
   logging a Bluetooth connect (no "socket"/"connect" error). **This is where a
   wrong `BtAddr` byte order or a bad SDP/service-GUID connect would surface** —
   a connect failure vs. a healthy link.
4. Connect PAT (AGWPE `127.0.0.1:8000`) and complete an AX.25 handshake /
   Winlink round-trip through the paired TNC — the real KISS-over-RFCOMM proof.
5. Kill the link (power off the TNC) and confirm the bridge auto-reconnects
   (Plan 3.5) with backoff, then recovers when the TNC returns.

If step 3 shows a connect that never completes or a garbled KISS stream,
suspect (a) `BtAddr` endianness in `parseBTAddr`, or (b) the SPP `ServiceClassId`
/ `Port=0` SDP path — both isolated to `bluetooth_windows.go`.

---

## Self-Review

**Spec coverage (spec Component 4):** Windows Bluetooth via Winsock `AF_BTH`/
`BTHPROTO_RFCOMM` with SDP channel discovery via the SPP service UUID — Task 2.
The umbrella spec's `device = bt:<name|MAC>` and `WSALookupService` name→MAC
enumeration are **deliberately deferred**: this plan connects by `bdaddr` MAC for
parity with the merged Linux BT transport (documented under Global Constraints),
and the name-discovery/enumeration piece moves to Plan 5 (the installer wizard is
its only consumer). Reconnect is already generic (Plan 3.5), so no bridge work.

**Placeholder scan:** No TBD/TODO. Every code step is complete. The one thing not
executed on Linux (the windows-tagged unit test + the socket path) is explicitly
routed to `go vet` for the target plus the § VM Validation checklist — not left
as an unverified claim.

**Type consistency:** `NewBluetoothTransport(cfg BluetoothConfig) Transport`
matches the signature the bridge already calls and the Linux/stub variants'
signatures. `BluetoothConfig` is defined once (Task 1, shared file) and used by
all three variants. `bluetoothTransport` implements the full `kiss.Transport`
interface (`Open`/`Read`/`Write`/`Close`/`EnterKISS`/`ExitKISS`). `parseBTAddr`
is used by `Open` and the test. `windows.SockaddrBth`/`WSABuf`/`GUID` field names
match `golang.org/x/sys/windows@v0.43.0`.

**Green-everywhere / build-tag safety:** Task 1 leaves the stub covering Windows
(Windows builds via stub); Task 2 adds the real transport and retags the stub in
the same commit, so there is never a duplicate/missing `NewBluetoothTransport` on
any target. Every task ends with linux + windows(amd64/arm64) + darwin + freebsd
build checks.
