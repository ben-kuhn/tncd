package kiss

import "io"

// Transport is a byte pipe to a TNC plus TNC-lifecycle hooks.
type Transport interface {
	Open() error
	io.ReadWriteCloser
	// EnterKISS is called after Open: send init_string probe/commands
	// (serial only; TCP/Bluetooth no-op).
	EnterKISS() error
	// ExitKISS is called before Close: KISS-exit + host_exit_string
	// (serial only).
	ExitKISS()
}

// RXFrame is a decoded KISS data frame received from the TNC.
type RXFrame struct {
	Port int    // tncd port number
	Data []byte // raw AX.25 bytes (KISS cmd byte stripped)
}
