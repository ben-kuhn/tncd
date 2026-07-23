//go:build linux

package kiss

import (
	"fmt"
	"reflect"
	"unsafe"

	"golang.org/x/sys/unix"
)

// applyRTSCTS enables hardware (RTS/CTS) flow control on the open serial port.
//
// go.bug.st/serial v1.8.0 disables CRTSCTS on open, exposes no flow-control API,
// and (with many USB-serial drivers) the device refuses a second open with
// EBUSY — so we cannot reach the tty's termios via a fresh fd. Instead we pull
// the library's own fd out of its *unixPort via reflection and set CRTSCTS on
// it. This is tightly coupled to the pinned v1.8.0 internals; if they change,
// libSerialFD returns false and the caller logs a non-fatal warning.
//
// Required for TNCs whose serial rate exceeds the on-air rate (e.g. a KPC-3+ at
// 9600 baud feeding a 1200-baud radio): without CTS pacing, the TNC's buffer
// overflows and KISS frames are corrupted.
func applyRTSCTS(port modemPort) error {
	fd, ok := libSerialFD(port)
	if !ok {
		return fmt.Errorf("could not obtain serial fd from go.bug.st/serial port")
	}
	return applyRTSCTSFD(fd)
}

// applyRTSCTSFD sets the CRTSCTS termios flag on an open serial fd, preserving
// the other settings (baud, VMIN/VTIME) via read-modify-write.
func applyRTSCTSFD(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("TCGETS: %w", err)
	}
	t.Cflag |= unix.CRTSCTS
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fmt.Errorf("TCSETS: %w", err)
	}
	return nil
}

// libSerialFD extracts the unexported int "handle" field (the fd) from
// go.bug.st/serial's *unixPort via reflection.
func libSerialFD(port modemPort) (int, bool) {
	v := reflect.ValueOf(port)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return 0, false
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName("handle")
	if !f.IsValid() || f.Kind() != reflect.Int || !f.CanAddr() {
		return 0, false
	}
	fd := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Int()
	return int(fd), true
}
