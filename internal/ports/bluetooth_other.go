//go:build !windows

package ports

// bluetoothDevices returns no Bluetooth devices off Windows; Linux Bluetooth is
// configured directly (bdaddr) rather than discovered here.
func bluetoothDevices() []Port { return nil }
