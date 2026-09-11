//go:build !linux

package kiss

import "fmt"

// NewBLETransport reports that BLE is unavailable on this platform.
//
// Linux has a pure-Go path (BlueZ over D-Bus). Windows can gain one via the
// WinRT GATT APIs, which also need no cgo. macOS cannot: CoreBluetooth is an
// Objective-C framework requiring cgo and a macOS build host, which would
// break the CGO_ENABLED=0 cross-compile every release target depends on.
// FreeBSD's netgraph stack has no usable LE support.
//
// On those platforms use a classic SPP TNC (`type = bluetooth`, or on macOS
// `type = serial` with the paired device's /dev/cu.* port).
func NewBLETransport(cfg BLEConfig) Transport {
	return unsupportedBLE{}
}

type unsupportedBLE struct{}

func (unsupportedBLE) Open() error {
	return fmt.Errorf("ble: not supported on this platform; use type = bluetooth (classic SPP) or serial")
}
func (unsupportedBLE) Read(p []byte) (int, error)  { return 0, fmt.Errorf("ble: not open") }
func (unsupportedBLE) Write(p []byte) (int, error) { return 0, fmt.Errorf("ble: not open") }
func (unsupportedBLE) Close() error                { return nil }
func (unsupportedBLE) EnterKISS() error            { return nil }
func (unsupportedBLE) ExitKISS()                   {}
