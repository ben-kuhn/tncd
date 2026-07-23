//go:build linux

package kiss

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestApplyRTSCTS verifies applyRTSCTS sets the CRTSCTS termios flag on a tty.
// A pty slave is a tty that stores termios flags, so we can set and read back
// CRTSCTS without real hardware.
func TestApplyRTSCTS(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx available: %v", err)
	}
	defer master.Close()

	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlockpt: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("TIOCGPTN: %v", err)
	}
	slave := fmt.Sprintf("/dev/pts/%d", n)

	if err := applyRTSCTS(slave); err != nil {
		t.Fatalf("applyRTSCTS: %v", err)
	}

	fd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open slave: %v", err)
	}
	defer unix.Close(fd)
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS: %v", err)
	}
	if tio.Cflag&unix.CRTSCTS == 0 {
		t.Fatalf("CRTSCTS not set after applyRTSCTS")
	}
}
