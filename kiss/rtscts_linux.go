//go:build linux

package kiss

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// applyRTSCTS enables hardware (RTS/CTS) flow control on the serial device.
//
// go.bug.st/serial disables CRTSCTS on open and exposes no flow-control API, so
// we set the CRTSCTS termios flag ourselves. termios is a property of the tty
// and is shared across all open file descriptors, so applying it via a
// temporary fd takes effect on the port the serial library already holds open.
// The read-modify-write preserves the VMIN/VTIME the library set via
// SetReadTimeout, so this MUST be called after that (the library's last termios
// write) — otherwise SetReadTimeout would clobber CRTSCTS.
func applyRTSCTS(device string) error {
	fd, err := unix.Open(device, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open for termios: %w", err)
	}
	defer unix.Close(fd)

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
