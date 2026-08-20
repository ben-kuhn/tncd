//go:build linux

package kiss

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// SPP profile UUID for Serial Port Profile.
const sppUUID = "00001101-0000-1000-8000-00805f9b34fb"

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
		if callErr := deviceObj.Call("org.bluez.Device1.Disconnect", 0).Err; callErr != nil {
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
		if err := deviceObj.Call("org.bluez.Device1.DisconnectProfile", 0, uuid).Err; err == nil {
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

func (bt *bluetoothTransport) Write(b []byte) (int, error) {
	if bt.file == nil {
		return 0, fmt.Errorf("bluetooth: not open")
	}
	if bt.ioDebug {
		// Log START before the write and DONE after, with elapsed time. If a
		// write parks (no send-buffer credit) the DONE line is delayed or never
		// appears — the definitive signal that the byte never left the host.
		start := time.Now()
		log.Printf("bluetooth: WRITE start %d bytes [%s]", len(b), hexHead(b, 24))
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
		if err := manager.Call(
			"org.bluez.ProfileManager1.RegisterProfile", 0,
			profilePath, sppUUID, opts,
		).Err; err != nil {
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
func isDeviceConnected(obj dbus.BusObject) (bool, error) {
	var v dbus.Variant
	err := obj.Call("org.freedesktop.DBus.Properties.Get", 0,
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
