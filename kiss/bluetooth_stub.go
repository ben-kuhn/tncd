//go:build !linux

package kiss

import "fmt"

type bluetoothStub struct{}

// NewBluetoothTransport returns a Transport whose Open() always errors on
// non-Linux platforms, where BlueZ D-Bus is not available.
func NewBluetoothTransport(cfg BluetoothConfig) Transport {
	return &bluetoothStub{}
}

func (b *bluetoothStub) Open() error {
	return fmt.Errorf("bluetooth TNCs are not supported on this platform; " +
		"use the OS serial port (COM/tty) instead")
}

func (b *bluetoothStub) Read(p []byte) (int, error)  { return 0, fmt.Errorf("bluetooth: not open") }
func (b *bluetoothStub) Write(p []byte) (int, error) { return 0, fmt.Errorf("bluetooth: not open") }
func (b *bluetoothStub) Close() error                { return nil }
func (b *bluetoothStub) EnterKISS() error            { return nil }
func (b *bluetoothStub) ExitKISS()                   {}
