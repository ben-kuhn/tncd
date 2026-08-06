//go:build linux

package kiss

import (
	"fmt"
	"os"
	"testing"
	"time"

	goserial "go.bug.st/serial"
	"golang.org/x/sys/unix"
)

// fakeHandlePort mimics go.bug.st/serial's *unixPort: a modemPort with an
// unexported int "handle" field. It guards the libSerialFD reflection mechanism
// against a local regression. (It cannot catch an upstream field rename — see
// the pin comment in go.mod.)
type fakeHandlePort struct{ handle int }

func (fakeHandlePort) SetDTR(bool) error                  { return nil }
func (fakeHandlePort) SetRTS(bool) error                  { return nil }
func (fakeHandlePort) SetReadTimeout(time.Duration) error { return nil }
func (fakeHandlePort) Close() error                       { return nil }
func (fakeHandlePort) GetModemStatusBits() (*goserial.ModemStatusBits, error) {
	return &goserial.ModemStatusBits{}, nil
}

// noHandlePort is a modemPort with no "handle" field.
type noHandlePort struct{}

func (noHandlePort) SetDTR(bool) error                  { return nil }
func (noHandlePort) SetRTS(bool) error                  { return nil }
func (noHandlePort) SetReadTimeout(time.Duration) error { return nil }
func (noHandlePort) Close() error                       { return nil }
func (noHandlePort) GetModemStatusBits() (*goserial.ModemStatusBits, error) {
	return &goserial.ModemStatusBits{}, nil
}

func TestLibSerialFD(t *testing.T) {
	if fd, ok := libSerialFD(&fakeHandlePort{handle: 42}); !ok || fd != 42 {
		t.Errorf("libSerialFD(&fakeHandlePort{42}) = (%d, %v), want (42, true)", fd, ok)
	}
	if _, ok := libSerialFD(&noHandlePort{}); ok {
		t.Errorf("libSerialFD(&noHandlePort) = ok, want false (no handle field)")
	}
	if _, ok := libSerialFD(fakeHandlePort{handle: 1}); ok {
		t.Errorf("libSerialFD(non-pointer) = ok, want false")
	}
	if _, ok := libSerialFD((*fakeHandlePort)(nil)); ok {
		t.Errorf("libSerialFD(nil pointer) = ok, want false")
	}
}

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

	fd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open slave: %v", err)
	}
	defer unix.Close(fd)

	if err := applyRTSCTSFD(fd); err != nil {
		t.Fatalf("applyRTSCTSFD: %v", err)
	}

	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS: %v", err)
	}
	if tio.Cflag&unix.CRTSCTS == 0 {
		t.Fatalf("CRTSCTS not set after applyRTSCTS")
	}
}
