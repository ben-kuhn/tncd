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
