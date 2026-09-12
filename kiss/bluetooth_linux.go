//go:build linux

package kiss

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// SPP profile UUID for Serial Port Profile.
const sppUUID = "00001101-0000-1000-8000-00805f9b34fb"

// btCallTimeout bounds a single blocking BlueZ D-Bus method call.
//
// godbus's Call blocks until the peer replies, with no deadline of its own. A
// BlueZ that accepts the call but never answers therefore hangs Open()
// forever, and because Open() runs on the port's reconnect path the port stops
// retrying and goes permanently silent with nothing logged — observed live:
// after "already connected, disconnecting first" the port emitted nothing for
// over ten minutes while its siblings kept reconnecting normally, and only a
// restart cleared it.
//
// Every BlueZ call on the connect path is bounded by this instead. It is long
// enough that a busy-but-healthy BlueZ still completes, and short enough that
// a wedged one costs one backoff interval rather than the port.
const btCallTimeout = 15 * time.Second

// dbusCaller is the subset of dbus.BusObject that callBlueZ needs. It exists
// so the timeout behaviour can be tested without a real system bus.
type dbusCaller interface {
	CallWithContext(ctx context.Context, method string, flags dbus.Flags,
		args ...interface{}) *dbus.Call
}

// callBlueZ invokes a BlueZ method, failing with the context error if it does
// not reply within timeout. A prompt reply (success or failure) is returned
// unchanged so callers keep their existing error handling.
func callBlueZ(obj dbusCaller, timeout time.Duration, method string, args ...interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return obj.CallWithContext(ctx, method, 0, args...).Err
}

// profilePath is the D-Bus object path at which we export our Profile1 object.
const profilePath = dbus.ObjectPath("/org/tncd/spp")

// bluetoothTransport implements Transport for a Bluetooth SPP KISS TNC via
// BlueZ D-Bus on Linux.
type bluetoothTransport struct {
	cfg  BluetoothConfig
	file *os.File // raw OS file wrapping the socket fd

	// ioDebug logs every Read/Write with byte counts, duration, and errors.
	// Enabled by setting TNCD_BT_WRITE_DEBUG in the environment. Used to prove
	// whether a write actually delivers or parks (no error, no bytes on air).
	ioDebug bool

	// txStall tracks how long the socket send queue has been backed up.
	// Touched only by the writer goroutine via Write.
	txStall txStallDetector
}

// NewBluetoothTransport returns a Transport that connects to a Bluetooth SPP
// KISS TNC via BlueZ D-Bus.
func NewBluetoothTransport(cfg BluetoothConfig) Transport {
	return &bluetoothTransport{cfg: cfg, ioDebug: os.Getenv("TNCD_BT_WRITE_DEBUG") != ""}
}

// hexHead returns a short hex preview of up to n bytes for logging.
func hexHead(b []byte, n int) string {
	if len(b) < n {
		n = len(b)
	}
	return fmt.Sprintf("% x", b[:n])
}

// Open connects to the Bluetooth SPP device.
//
// It registers the SPP Profile1 object on org.bluez (once per process), then
// calls Device1.ConnectProfile asynchronously. BlueZ delivers the connected
// file descriptor via the Profile1.NewConnection D-Bus method call; Open waits
// up to 30 seconds for that to arrive.
//
// Concurrency: ConnectProfile is dispatched via a goroutine so that the
// current goroutine can block on a channel waiting for the fd. godbus spawns
// a new goroutine per incoming call to invoke NewConnection, which writes the
// fd into the pending map and signals the channel. The two goroutines
// communicate via a buffered channel of size 1; there is no shared lock
// required between Open's wait and NewConnection's delivery.
func (bt *bluetoothTransport) Open() error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("bluetooth: connect to D-Bus system bus: %w", err)
	}
	// We keep the connection alive for the duration of this Open() call.
	// The profile registration is process-scoped and uses its own long-lived
	// connection managed by registerProfileOnce.
	defer conn.Close()

	devicePath, err := bdaddrToPath(bt.cfg.BDAddr)
	if err != nil {
		return err
	}

	// Ensure the profile is registered exactly once per process.
	if err := registerProfileOnce(); err != nil {
		return err
	}

	// Register a pending slot for this device before calling ConnectProfile
	// so that an extremely fast delivery from BlueZ is not lost.
	fdCh := make(chan int, 1)
	errCh := make(chan error, 1)
	registerPending(string(devicePath), fdCh)

	// Check if the device is already Connected; if so, disconnect first.
	// This mirrors tncd.py:817–830.
	propsObj := conn.Object("org.bluez", devicePath)
	connected, err := isDeviceConnected(propsObj)
	if err == nil && connected {
		log.Printf("bluetooth: %s already connected, disconnecting first", bt.cfg.BDAddr)
		deviceObj := conn.Object("org.bluez", devicePath)
		if callErr := callBlueZ(deviceObj, btCallTimeout, "org.bluez.Device1.Disconnect"); callErr != nil {
			log.Printf("bluetooth: pre-disconnect error (continuing): %v", callErr)
		}
		// Settle before reconnecting. An immediate ConnectProfile after Disconnect
		// can reuse a half-open RFCOMM link whose writes are accepted at the socket
		// but silently dropped — the "wedged SPP" seen on reconnect/handoff, where
		// frames never reach the TNC (0 bytes on air) despite a healthy-looking
		// socket. Letting BlueZ and the peer fully tear the link down first avoids
		// reconnecting onto the stale channel.
		time.Sleep(bluetoothReconnectSettle)
	}

	// Call ConnectProfile asynchronously so the GLib main loop (or D-Bus
	// dispatcher) can process the incoming NewConnection call while we wait.
	// We use a goroutine + conn.Object.Go to avoid blocking the current
	// goroutine; benign errors are silently swallowed (NoReply, InProgress,
	// br-connection-busy) mirroring tncd.py:800–810.
	deviceObj := conn.Object("org.bluez", devicePath)
	go func() {
		log.Printf("bluetooth: calling ConnectProfile on %s", devicePath)
		call := deviceObj.Call("org.bluez.Device1.ConnectProfile", 0, sppUUID)
		if call.Err != nil {
			if !isBenignConnectError(call.Err) {
				errCh <- fmt.Errorf("bluetooth: ConnectProfile: %w", call.Err)
			}
			// Benign errors are expected: the fd arrives via NewConnection.
		}
	}()

	// Wait for NewConnection to deliver the fd (or a fatal error), up to 30s.
	select {
	case fd := <-fdCh:
		// NewConnection has already dup'd or taken ownership; wrap as *os.File.
		bt.file = os.NewFile(uintptr(fd), fmt.Sprintf("bt-spp-%s", bt.cfg.BDAddr))
		// Fresh socket, fresh queue: clear any stall clock left from a previous
		// link so a reopened transport cannot trip on the old one's backlog.
		bt.txStall = txStallDetector{}
		log.Printf("bluetooth: SPP socket ready (fd=%d) for %s", fd, bt.cfg.BDAddr)
		// Drop any Bluetooth audio profiles now that SPP is up. On some radios
		// (notably the UV-PRO) an active audio profile — auto-connected by the
		// desktop/BlueZ, often on a radio power-cycle — corrupts the SPP/KISS
		// data channel: writes complete at the socket but frames never reach the
		// TNC. Runs on every connect and reconnect (Open is called for both).
		disconnectAudioProfiles(deviceObj, bt.cfg.BDAddr)
		return nil
	case err := <-errCh:
		removePending(string(devicePath))
		return err
	case <-time.After(30 * time.Second):
		removePending(string(devicePath))
		return fmt.Errorf("bluetooth: connection to %s timed out (30s)", bt.cfg.BDAddr)
	}
}

// bluetoothReconnectSettle is how long Open waits after disconnecting a stale
// connection before calling ConnectProfile, so BlueZ and the peer fully tear
// down the old RFCOMM link rather than handing back a wedged half-open channel.
const bluetoothReconnectSettle = 2 * time.Second

// audioProfileUUIDs are the Bluetooth Classic audio profiles we drop after
// establishing SPP. When one of these is connected alongside SPP, the RFCOMM
// data channel becomes unreliable on some radios (writes succeed at the socket
// but frames never reach the TNC's transmitter).
var audioProfileUUIDs = []string{
	"0000111e-0000-1000-8000-00805f9b34fb", // Handsfree (HFP HF)
	"0000111f-0000-1000-8000-00805f9b34fb", // Handsfree Audio Gateway (HFP AG)
	"0000110b-0000-1000-8000-00805f9b34fb", // A2DP Sink
	"0000110a-0000-1000-8000-00805f9b34fb", // A2DP Source
	"0000110d-0000-1000-8000-00805f9b34fb", // Advanced Audio Distribution
	"0000110e-0000-1000-8000-00805f9b34fb", // A/V Remote Control
}

// disconnectAudioProfiles asks BlueZ to disconnect each audio profile on the
// device, leaving SPP intact. Errors (profile not connected / not supported)
// are expected and ignored; only successful drops are logged.
func disconnectAudioProfiles(deviceObj dbus.BusObject, bdaddr string) {
	for _, uuid := range audioProfileUUIDs {
		if err := callBlueZ(deviceObj, btCallTimeout,
			"org.bluez.Device1.DisconnectProfile", uuid); err == nil {
			log.Printf("bluetooth: %s dropped audio profile %s", bdaddr, uuid)
		}
	}
}

func (bt *bluetoothTransport) Read(b []byte) (int, error) {
	if bt.file == nil {
		return 0, fmt.Errorf("bluetooth: not open")
	}
	if bt.ioDebug {
		n, err := bt.file.Read(b)
		if n > 0 || err != nil {
			log.Printf("bluetooth: READ  %d bytes err=%v [%s]", n, err, hexHead(b[:max(n, 0)], 24))
		}
		return n, err
	}
	return bt.file.Read(b)
}

// What this detector can and cannot see
//
// It watches ONE layer: the host's own socket send queue. That covers the case
// where the peer stops granting RFCOMM credits — the kernel then cannot hand
// the bytes over, they pile up in our send buffer, and the depth below grows
// until this trips.
//
// It does NOT cover a radio that accepts the bytes and then sits on them, and
// the field evidence says to expect exactly that. Over two minutes of a real
// UV-PRO stall (2026-09-11, frames written at 21:58:42 that an independent air
// monitor did not hear until 22:00:49 — 126s later) every write returned the
// full byte count in 4-21 microseconds and the depth below read ZERO. Sampled
// across 61 writes in that session the queue was empty for 42 of them and
// otherwise held only the transient single-frame values. The bytes left the
// host promptly; the radio buffered them. Nothing observable from this side of
// the socket changes during such a stall, so do not read a quiet detector as
// evidence that TX is healthy.
//
// The layer that CAN see the radio-side case is the bridge's read-side wedge
// watchdog (rx_wedge_timeout): it notices that nothing is coming back while TX
// is outstanding, which is the only host-visible symptom a buffering radio
// produces. Classic SPP has no delivery acknowledgement, so per-frame proof is
// impossible here by construction; a BLE GATT write-with-response is the
// transport-level fix, since that ack originates at the radio.

// txStallTimeout is how long the send queue may stay continuously backed up
// before the TX path is declared stalled. KISS frames are small and the
// Bluetooth link is far faster than 1200-baud RF, so in normal operation the
// kernel hands each frame to the radio in milliseconds and the queue reads
// empty between writes. A queue that stays backed up for this long is not
// congestion, it is a link that has stopped draining. The value sits above the
// L2 T1 poll interval so an ordinary retransmit cycle cannot trip it, and below
// the peer's own give-up time so the port is cycled while the session is still
// worth saving.
const txStallTimeout = 30 * time.Second

// txStallDetector tracks how long the socket send queue has been continuously
// backed up. Only the writer goroutine touches it, so it needs no lock.
type txStallDetector struct {
	since time.Time
}

// observe records a send-queue depth sample and returns how long the queue has
// been continuously backed up. A drained queue clears the tracking, so the
// duration only grows while bytes are genuinely stuck.
func (d *txStallDetector) observe(depth int, now time.Time) time.Duration {
	if depth <= 0 {
		d.since = time.Time{}
		return 0
	}
	if d.since.IsZero() {
		d.since = now
		return 0
	}
	return now.Sub(d.since)
}

// txQueueDepth reports how many bytes the kernel has accepted from us but not
// yet delivered to the peer.
//
// Bluetooth sockets answer TIOCOUTQ with the *free* space in the send buffer
// (net/bluetooth/af_bluetooth.c returns sk_sndbuf - sk_wmem_alloc), which is
// the opposite of the TCP convention, so the depth is recovered by subtracting
// from SO_SNDBUF. Read via SyscallConn rather than File.Fd(): Fd() puts the
// descriptor into blocking mode and removes it from the runtime poller, which
// would change how every subsequent read and write behaves.
func (bt *bluetoothTransport) txQueueDepth() (int, error) {
	rc, err := bt.file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var depth int
	var inner error
	if err := rc.Control(func(fd uintptr) {
		free, e := unix.IoctlGetInt(int(fd), unix.TIOCOUTQ)
		if e != nil {
			inner = e
			return
		}
		sndbuf, e := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
		if e != nil {
			inner = e
			return
		}
		if d := sndbuf - free; d > 0 {
			depth = d
		}
	}); err != nil {
		return 0, err
	}
	return depth, inner
}

// checkTXDrain fails the write when our send queue has been backed up for
// txStallTimeout, i.e. the kernel has bytes for the radio that it cannot hand
// over. Returning an error routes it through the port's normal teardown, taking
// the port offline for a reconnect. See the txStallTimeout comment for why this
// covers only credit starvation and not a radio that buffers internally.
//
// Sampled *before* handing over the next frame, so it measures whether earlier
// frames drained. Sampling afterwards would always count the bytes just
// written — measured at ~960 on a healthy link for a 27-byte KISS frame, since
// the depth includes socket-buffer overhead for the frame still in flight —
// and the queue would never appear empty, tripping this on a working link.
//
// Only Write samples this, which is sufficient for the failure it targets: a
// starved link still has L2 pushing T1 polls and retransmits into it, so
// samples keep arriving for as long as anything is worth transmitting.
//
// If the query itself fails the write is left alone: no detection is better
// than refusing to transmit on a link that may be perfectly healthy.
func (bt *bluetoothTransport) checkTXDrain() error {
	depth, err := bt.txQueueDepth()
	if err != nil {
		return nil
	}
	if stalled := bt.txStall.observe(depth, time.Now()); stalled >= txStallTimeout {
		return fmt.Errorf("bluetooth: TX stalled -- %d bytes stuck in the host send queue for %s, "+
			"%s is not accepting data", depth, stalled.Round(time.Second), bt.cfg.BDAddr)
	}
	return nil
}

func (bt *bluetoothTransport) Write(b []byte) (int, error) {
	if bt.file == nil {
		return 0, fmt.Errorf("bluetooth: not open")
	}
	// Check the backlog before adding to it: a write "succeeding" here only
	// means the kernel accepted the bytes, so the question that matters is
	// whether everything written earlier actually drained to the radio. Bail
	// out rather than queueing behind them — the port is about to be torn down,
	// and piling another frame onto a queue we have just declared dead can only
	// block us in the kernel.
	if err := bt.checkTXDrain(); err != nil {
		return 0, err
	}

	if bt.ioDebug {
		// Log START before the write and DONE after, with elapsed time. If a
		// write parks (no send-buffer credit) the DONE line is delayed or never
		// appears — the definitive signal that the byte never left the host.
		// backlog is the pre-write queue depth: ~0 on a healthy link.
		backlog, qErr := bt.txQueueDepth()
		start := time.Now()
		log.Printf("bluetooth: WRITE start %d bytes backlog=%d (qerr=%v) [%s]",
			len(b), backlog, qErr, hexHead(b, 24))
		n, err := bt.file.Write(b)
		log.Printf("bluetooth: WRITE done  %d/%d bytes in %s err=%v", n, len(b), time.Since(start), err)
		return n, err
	}
	return bt.file.Write(b)
}

func (bt *bluetoothTransport) Close() error {
	if bt.file != nil {
		err := bt.file.Close()
		bt.file = nil
		return err
	}
	return nil
}

// EnterKISS is a no-op: Bluetooth SPP TNCs (Mobilinkd) are always in KISS mode.
func (bt *bluetoothTransport) EnterKISS() error { return nil }

// ExitKISS is a no-op: KISS exit bytes are serial-only (tncd.py:916).
func (bt *bluetoothTransport) ExitKISS() {}

// ---- Process-scoped profile registration ----

// sppProfile is the exported D-Bus object that BlueZ calls back on.
// Its methods are invoked by godbus in a new goroutine per incoming call.
type sppProfile struct{}

// NewConnection is called by BlueZ when a connected fd is ready for the
// registered SPP profile. godbus transfers ownership of the delivered fd to
// this method; we route it to the waiting Open() call via the pending map.
// If the connection is unexpected or the channel is full, we close the fd here.
func (p *sppProfile) NewConnection(devicePath dbus.ObjectPath, fd dbus.UnixFD, properties map[string]dbus.Variant) *dbus.Error {
	rawFD := int(fd)
	log.Printf("bluetooth: NewConnection: path=%s fd=%d", devicePath, rawFD)

	fdCh := lookupPending(string(devicePath))
	if fdCh == nil {
		log.Printf("bluetooth: unexpected NewConnection from %s, closing fd", devicePath)
		_ = closeFD(rawFD)
		return nil
	}
	// Non-blocking send: channel is buffered(1) and Open created it just for us.
	select {
	case fdCh <- rawFD:
	default:
		log.Printf("bluetooth: fd channel full for %s, closing fd", devicePath)
		_ = closeFD(rawFD)
	}
	return nil
}

// RequestDisconnection is called by BlueZ when a device disconnects.
// Log-only per tncd.py:961–963.
func (p *sppProfile) RequestDisconnection(devicePath dbus.ObjectPath) *dbus.Error {
	log.Printf("bluetooth: RequestDisconnection: %s", devicePath)
	return nil
}

// Release is called by BlueZ when the profile is unregistered.
// Log-only per tncd.py:965–969.
func (p *sppProfile) Release() *dbus.Error {
	log.Printf("bluetooth: SPP profile released by BlueZ")
	return nil
}

// profileConn is the long-lived D-Bus connection used for the profile export.
// It must stay open for the lifetime of the profile.
var profileConn *dbus.Conn

// profileMu guards profileRegistered and profileConn.
var profileMu sync.Mutex

// profileRegistered is true once a successful RegisterProfile call has been
// made. Unlike sync.Once, a failed attempt leaves it false so the next
// Open() can retry from scratch.
var profileRegistered bool

// registerProfileOnce connects to the system D-Bus, exports the Profile1
// object, and calls ProfileManager1.RegisterProfile.
//
// On success it sets profileRegistered=true and leaves profileConn open for
// the lifetime of the profile. On failure it closes any D-Bus connection
// opened during this attempt and returns the error, leaving profileRegistered
// false so the next call retries.
//
// Once registration succeeds, subsequent calls are no-ops (idempotent).
func registerProfileOnce() error {
	return ensureProfile(func() error {
		conn, err := dbus.ConnectSystemBus()
		if err != nil {
			return fmt.Errorf("bluetooth: system bus for profile: %w", err)
		}

		prof := &sppProfile{}
		// Export using ExportMethodTable so we can map D-Bus method names
		// (with dots and mixed case) to Go methods.
		err = conn.ExportMethodTable(
			map[string]interface{}{
				"NewConnection":        prof.NewConnection,
				"RequestDisconnection": prof.RequestDisconnection,
				"Release":              prof.Release,
			},
			profilePath,
			"org.bluez.Profile1",
		)
		if err != nil {
			conn.Close()
			return fmt.Errorf("bluetooth: export Profile1: %w", err)
		}

		manager := conn.Object("org.bluez", "/org/bluez")
		opts := map[string]dbus.Variant{
			"Role": dbus.MakeVariant("client"),
		}
		if err := callBlueZ(manager, btCallTimeout,
			"org.bluez.ProfileManager1.RegisterProfile",
			profilePath, sppUUID, opts,
		); err != nil {
			conn.Close()
			return fmt.Errorf("bluetooth: RegisterProfile: %w", err)
		}

		profileConn = conn
		log.Printf("bluetooth: SPP profile registered at %s", profilePath)
		return nil
	})
}

// ensureProfile is the testable seam for profile registration. It calls
// register at most once as long as registration keeps failing; once register
// succeeds the registered flag is set and subsequent calls are no-ops.
//
// Thread-safe: guarded by profileMu.
func ensureProfile(register func() error) error {
	profileMu.Lock()
	defer profileMu.Unlock()
	if profileRegistered {
		return nil
	}
	if err := register(); err != nil {
		// Leave profileRegistered false so the next Open() can retry.
		return err
	}
	profileRegistered = true
	return nil
}

// ---- Pending-connection map ----

var pendingMu sync.Mutex
var pendingMap = map[string]chan int{}

func registerPending(devicePath string, ch chan int) {
	pendingMu.Lock()
	pendingMap[devicePath] = ch
	pendingMu.Unlock()
}

func removePending(devicePath string) {
	pendingMu.Lock()
	delete(pendingMap, devicePath)
	pendingMu.Unlock()
}

func lookupPending(devicePath string) chan int {
	pendingMu.Lock()
	ch := pendingMap[devicePath]
	if ch != nil {
		delete(pendingMap, devicePath)
	}
	pendingMu.Unlock()
	return ch
}

// ---- Helpers ----

// bdaddrToPath converts a Bluetooth address (AA:BB:CC:DD:EE:FF) to the BlueZ
// D-Bus device path (/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF).
func bdaddrToPath(bdaddr string) (dbus.ObjectPath, error) {
	if bdaddr == "" {
		return "", fmt.Errorf("bluetooth: bdaddr is empty")
	}
	escaped := strings.ToUpper(strings.ReplaceAll(bdaddr, ":", "_"))
	return dbus.ObjectPath("/org/bluez/hci0/dev_" + escaped), nil
}

// isDeviceConnected reads the Connected property from a BlueZ Device1 object.
// Bounded by btCallTimeout: this runs on the connect path, where an unanswered
// call would wedge the port's reconnect loop.
func isDeviceConnected(obj dbus.BusObject) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), btCallTimeout)
	defer cancel()
	var v dbus.Variant
	err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		"org.bluez.Device1", "Connected").Store(&v)
	if err != nil {
		return false, err
	}
	connected, ok := v.Value().(bool)
	return ok && connected, nil
}

// isBenignConnectError returns true for errors that are expected when BlueZ
// delivers the fd via NewConnection rather than the ConnectProfile reply.
// Mirrors tncd.py:800–804.
func isBenignConnectError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "NoReply") ||
		strings.Contains(s, "Did not receive a reply") ||
		strings.Contains(s, "InProgress") ||
		strings.Contains(s, "br-connection-busy")
}

// closeFD closes a raw file descriptor.
func closeFD(fd int) error {
	return syscall.Close(fd)
}
