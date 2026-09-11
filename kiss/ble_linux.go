//go:build linux

package kiss

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// BLE KISS transport over BlueZ's D-Bus GATT API.
//
// Connect, resolve GATT, subscribe to the RX characteristic, and write KISS
// bytes to the TX characteristic. Everything above this — the KISS decoder,
// kiss.Port, L2 — is unchanged: this is an ordinary byte-stream Transport.
//
// Reassembly needs no work here. The spec lets a KISS frame span several
// notifications and lets one notification carry several frames, which is
// exactly what Decoder.Feed already tolerates: it scans a byte stream for FEND
// and keeps partial-frame state across calls.

// bleConnectTimeout bounds Device1.Connect and the wait for GATT resolution.
// Generous because connecting a sleeping radio includes its advertising
// interval, but bounded so a port's reconnect loop cannot hang (the failure
// that wedged a port for ten minutes in bluetooth_linux.go).
const bleConnectTimeout = 30 * time.Second

// bleRXQueue is how many notification payloads may sit unread before the
// reader falls behind. Notifications are small (one MTU) and the reader loop
// drains them immediately; the queue only absorbs bursts.
const bleRXQueue = 64

type bleTransport struct {
	cfg BLEConfig

	mu       sync.Mutex
	conn     *dbus.Conn
	txChar   dbus.BusObject
	rxPath   dbus.ObjectPath
	devPath  dbus.ObjectPath
	mtu      int
	open     bool
	closedCh chan struct{}

	// rx carries notification payloads to Read; leftover holds the tail of a
	// payload that did not fit in the caller's buffer.
	rx       chan []byte
	leftover []byte
}

// NewBLETransport returns a BLE KISS transport for the radio at cfg.BDAddr.
// The device must already be paired and trusted.
func NewBLETransport(cfg BLEConfig) Transport {
	return &bleTransport{cfg: cfg, mtu: bleDefaultMTU}
}

func (bt *bleTransport) Open() error {
	devPath, err := bdaddrToPath(bt.cfg.BDAddr)
	if err != nil {
		return err
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("ble: connect to D-Bus system bus: %w", err)
	}

	dev := conn.Object("org.bluez", devPath)
	connected, err := isDeviceConnected(dev)
	if err != nil {
		conn.Close()
		return fmt.Errorf("ble: %s is not known to BlueZ (pair it first): %w", bt.cfg.BDAddr, err)
	}
	if !connected {
		if err := callBlueZ(dev, bleConnectTimeout, "org.bluez.Device1.Connect"); err != nil {
			conn.Close()
			return fmt.Errorf("ble: connect %s: %w", bt.cfg.BDAddr, err)
		}
	}

	// GATT objects only appear once BlueZ has resolved services, which lags
	// the connection.
	if err := waitServicesResolved(dev, bleConnectTimeout); err != nil {
		conn.Close()
		return fmt.Errorf("ble: %s: %w", bt.cfg.BDAddr, err)
	}

	txPath, rxPath, err := findKISSChars(conn, devPath)
	if err != nil {
		conn.Close()
		return fmt.Errorf("ble: %s: %w", bt.cfg.BDAddr, err)
	}

	bt.mu.Lock()
	bt.conn = conn
	bt.devPath = devPath
	bt.txChar = conn.Object("org.bluez", txPath)
	bt.rxPath = rxPath
	bt.mtu = readCharMTU(conn, txPath)
	bt.rx = make(chan []byte, bleRXQueue)
	bt.closedCh = make(chan struct{})
	bt.leftover = nil
	bt.open = true
	closed := bt.closedCh
	rxCh := bt.rx
	bt.mu.Unlock()

	// Watch the RX characteristic's Value property, then enable notifications.
	// Registering first means a notification arriving immediately is not lost.
	if err := watchNotifications(conn, rxPath, rxCh, closed); err != nil {
		bt.Close()
		return fmt.Errorf("ble: watch notifications: %w", err)
	}
	rxChar := conn.Object("org.bluez", rxPath)
	if err := callBlueZ(rxChar, btCallTimeout, "org.bluez.GattCharacteristic1.StartNotify"); err != nil {
		bt.Close()
		return fmt.Errorf("ble: StartNotify: %w", err)
	}

	log.Printf("ble: KISS service ready on %s (MTU %d)", bt.cfg.BDAddr, bt.mtu)
	return nil
}

// Read returns bytes delivered by RX notifications, blocking until some
// arrive. Partial payloads are preserved across calls, so a caller with a
// small buffer never loses data.
func (bt *bleTransport) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	bt.mu.Lock()
	rx, closed := bt.rx, bt.closedCh
	if len(bt.leftover) > 0 {
		n := copy(p, bt.leftover)
		bt.leftover = bt.leftover[n:]
		bt.mu.Unlock()
		return n, nil
	}
	bt.mu.Unlock()
	if rx == nil {
		return 0, fmt.Errorf("ble: not open")
	}

	select {
	case data, ok := <-rx:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, data)
		if n < len(data) {
			bt.mu.Lock()
			bt.leftover = append(bt.leftover, data[n:]...)
			bt.mu.Unlock()
		}
		return n, nil
	case <-closed:
		return 0, io.EOF
	}
}

// Write sends KISS bytes to the TNC, split to fit the negotiated MTU.
//
// Each chunk is written *with response*, so the radio acknowledges it at the
// ATT layer and a write that did not arrive reports an error. That is the
// property classic RFCOMM cannot offer, and the reason this transport exists.
func (bt *bleTransport) Write(p []byte) (int, error) {
	bt.mu.Lock()
	tx, mtu, open := bt.txChar, bt.mtu, bt.open
	bt.mu.Unlock()
	if !open || tx == nil {
		return 0, fmt.Errorf("ble: not open")
	}

	// "request" forces write-with-response even on a characteristic that also
	// advertises write-without-response, which BlueZ would otherwise prefer.
	opts := map[string]dbus.Variant{"type": dbus.MakeVariant("request")}
	written := 0
	for _, chunk := range chunkForMTU(p, mtu) {
		if err := callBlueZ(tx, btCallTimeout,
			"org.bluez.GattCharacteristic1.WriteValue", chunk, opts); err != nil {
			return written, fmt.Errorf("ble: write to %s: %w", bt.cfg.BDAddr, err)
		}
		written += len(chunk)
	}
	return written, nil
}

func (bt *bleTransport) Close() error {
	bt.mu.Lock()
	if !bt.open {
		bt.mu.Unlock()
		return nil
	}
	bt.open = false
	conn, rxPath, closed := bt.conn, bt.rxPath, bt.closedCh
	bt.conn, bt.txChar = nil, nil
	bt.mu.Unlock()

	if closed != nil {
		close(closed) // unblocks Read
	}
	if conn != nil {
		if rxPath != "" {
			// Best effort: the link may already be gone.
			callBlueZ(conn.Object("org.bluez", rxPath), btCallTimeout,
				"org.bluez.GattCharacteristic1.StopNotify")
		}
		conn.Close()
	}
	return nil
}

// EnterKISS is a no-op: the BLE KISS service carries KISS framing by
// definition, so there is no host mode to escape from.
func (bt *bleTransport) EnterKISS() error { return nil }

// ExitKISS is a no-op for the same reason (KISS exit bytes are serial-only).
func (bt *bleTransport) ExitKISS() {}

// waitServicesResolved polls Device1.ServicesResolved until GATT is available.
func waitServicesResolved(dev dbus.BusObject, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var v dbus.Variant
		ctx, cancel := context.WithTimeout(context.Background(), btCallTimeout)
		err := dev.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
			"org.bluez.Device1", "ServicesResolved").Store(&v)
		cancel()
		if err == nil {
			if resolved, ok := v.Value().(bool); ok && resolved {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("GATT services not resolved within %s", timeout)
}

// findKISSChars locates the BLE KISS TX and RX characteristics on a device.
func findKISSChars(conn *dbus.Conn, devPath dbus.ObjectPath) (tx, rx dbus.ObjectPath, err error) {
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	obj := conn.Object("org.bluez", "/")
	ctx, cancel := context.WithTimeout(context.Background(), btCallTimeout)
	defer cancel()
	if err := obj.CallWithContext(ctx,
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects); err != nil {
		return "", "", fmt.Errorf("enumerate GATT objects: %w", err)
	}

	prefix := string(devPath) + "/"
	for path, ifaces := range objects {
		if !strings.HasPrefix(string(path), prefix) {
			continue
		}
		props, ok := ifaces["org.bluez.GattCharacteristic1"]
		if !ok {
			continue
		}
		uuid, _ := props["UUID"].Value().(string)
		switch strings.ToLower(uuid) {
		case bleKISSTXChar:
			tx = path
		case bleKISSRXChar:
			rx = path
		}
	}
	if tx == "" || rx == "" {
		return "", "", fmt.Errorf(
			"BLE KISS service %s not found (is the radio in TNC/KISS mode?)", bleKISSService)
	}
	return tx, rx, nil
}

// readCharMTU reports the characteristic's negotiated ATT MTU, falling back to
// the LE default when BlueZ does not expose it (older versions omit it).
func readCharMTU(conn *dbus.Conn, path dbus.ObjectPath) int {
	var v dbus.Variant
	ctx, cancel := context.WithTimeout(context.Background(), btCallTimeout)
	defer cancel()
	err := conn.Object("org.bluez", path).CallWithContext(ctx,
		"org.freedesktop.DBus.Properties.Get", 0,
		"org.bluez.GattCharacteristic1", "MTU").Store(&v)
	if err != nil {
		return bleDefaultMTU
	}
	if mtu, ok := v.Value().(uint16); ok && int(mtu) > bleDefaultMTU {
		return int(mtu)
	}
	return bleDefaultMTU
}

// watchNotifications forwards the RX characteristic's Value updates to rx.
// Payloads are copied: the D-Bus body is not ours to retain.
func watchNotifications(conn *dbus.Conn, rxPath dbus.ObjectPath,
	rx chan<- []byte, closed <-chan struct{}) error {
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(rxPath),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		return err
	}
	sigCh := make(chan *dbus.Signal, bleRXQueue)
	conn.Signal(sigCh)

	go func() {
		for {
			select {
			case <-closed:
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				if sig.Path != rxPath || len(sig.Body) < 2 {
					continue
				}
				changed, ok := sig.Body[1].(map[string]dbus.Variant)
				if !ok {
					continue
				}
				val, ok := changed["Value"]
				if !ok {
					continue
				}
				data, ok := val.Value().([]byte)
				if !ok || len(data) == 0 {
					continue
				}
				buf := append([]byte(nil), data...)
				select {
				case rx <- buf:
				case <-closed:
					return
				default:
					log.Printf("ble: RX queue full, dropping %d bytes", len(buf))
				}
			}
		}
	}()
	return nil
}
