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

// BluetoothConfig holds the configuration for a Bluetooth SPP KISS transport.
type BluetoothConfig struct {
	BDAddr            string
	Channel           string // informational; the SPP profile UUID drives connection
	Reconnect         bool
	ReconnectDelay    time.Duration
	ReconnectMaxDelay time.Duration
}

// bluetoothTransport implements Transport for a Bluetooth SPP KISS TNC via
// BlueZ D-Bus on Linux.
type bluetoothTransport struct {
	cfg  BluetoothConfig
	file *os.File // raw OS file wrapping the socket fd
}

// NewBluetoothTransport returns a Transport that connects to a Bluetooth SPP
// KISS TNC via BlueZ D-Bus.
func NewBluetoothTransport(cfg BluetoothConfig) Transport {
	return &bluetoothTransport{cfg: cfg}
}

// Open connects to the Bluetooth SPP device.
//
// It registers the SPP Profile1 object on org.bluez (once per process), then
// calls Device1.ConnectProfile asynchronously. BlueZ delivers the connected
// file descriptor via the Profile1.NewConnection D-Bus method call; Open waits
// up to 30 seconds for that to arrive.
//
// Concurrency: ConnectProfile is dispatched via a goroutine so that the
// current goroutine can block on a channel waiting for the fd. godbus
// dispatches exported-method calls (NewConnection) on its own goroutine,
// which writes the fd into the pending map and signals the channel. The two
// goroutines communicate via a buffered channel of size 1; there is no shared
// lock required between Open's wait and NewConnection's delivery.
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
		return nil
	case err := <-errCh:
		removePending(string(devicePath))
		return err
	case <-time.After(30 * time.Second):
		removePending(string(devicePath))
		return fmt.Errorf("bluetooth: connection to %s timed out (30s)", bt.cfg.BDAddr)
	}
}

func (bt *bluetoothTransport) Read(b []byte) (int, error) {
	if bt.file == nil {
		return 0, fmt.Errorf("bluetooth: not open")
	}
	return bt.file.Read(b)
}

func (bt *bluetoothTransport) Write(b []byte) (int, error) {
	if bt.file == nil {
		return 0, fmt.Errorf("bluetooth: not open")
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
// Its methods are invoked by godbus on its own dispatch goroutine.
type sppProfile struct{}

// NewConnection is called by BlueZ when a connected fd is ready for the
// registered SPP profile. The fd is a dbus.UnixFD; we dup it immediately
// (using syscall via os) so ownership is unambiguous, then route to the
// waiting Open() call via the pending map.
func (p *sppProfile) NewConnection(devicePath dbus.ObjectPath, fd dbus.UnixFD, properties map[string]dbus.Variant) *dbus.Error {
	rawFD := int(fd)
	log.Printf("bluetooth: NewConnection: path=%s fd=%d", devicePath, rawFD)

	// Duplicate the fd so we own a copy independent of the D-Bus message.
	// The kernel will close the original after this call returns.
	dupFD, err := dupFD(rawFD)
	if err != nil {
		log.Printf("bluetooth: dup fd %d: %v", rawFD, err)
		return dbus.NewError("org.bluez.Error.Failed", []interface{}{err.Error()})
	}

	fdCh := lookupPending(string(devicePath))
	if fdCh == nil {
		log.Printf("bluetooth: unexpected NewConnection from %s, closing fd", devicePath)
		_ = closeFD(dupFD)
		return nil
	}
	// Non-blocking send: channel is buffered(1) and Open created it just for us.
	select {
	case fdCh <- dupFD:
	default:
		log.Printf("bluetooth: fd channel full for %s, closing dup fd", devicePath)
		_ = closeFD(dupFD)
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

// profileOnce ensures the SPP profile is registered exactly once per process.
var profileOnce sync.Once
var profileOnceErr error

// registerProfileOnce connects to the system D-Bus, exports the Profile1
// object, and calls ProfileManager1.RegisterProfile. It is idempotent.
func registerProfileOnce() error {
	profileOnce.Do(func() {
		conn, err := dbus.ConnectSystemBus()
		if err != nil {
			profileOnceErr = fmt.Errorf("bluetooth: system bus for profile: %w", err)
			return
		}
		profileConn = conn

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
			profileOnceErr = fmt.Errorf("bluetooth: export Profile1: %w", err)
			return
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
			profileOnceErr = fmt.Errorf("bluetooth: RegisterProfile: %w", err)
			return
		}
		log.Printf("bluetooth: SPP profile registered at %s", profilePath)
	})
	return profileOnceErr
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

// dupFD duplicates a file descriptor using the dup syscall.
func dupFD(fd int) (int, error) {
	return syscall.Dup(fd)
}

// closeFD closes a raw file descriptor.
func closeFD(fd int) error {
	return syscall.Close(fd)
}
