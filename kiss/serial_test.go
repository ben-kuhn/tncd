package kiss

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	goserial "go.bug.st/serial"
)

// fakeSerial scripts responses: each probe CR gets the next queued response.
type fakeSerial struct {
	mu        sync.Mutex
	written   bytes.Buffer
	responses [][]byte // popped on each \r written
	pending   bytes.Buffer
}

func (f *fakeSerial) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written.Write(p)
	if bytes.Equal(p, []byte("\r")) && len(f.responses) > 0 {
		f.pending.Write(f.responses[0])
		f.responses = f.responses[1:]
	}
	return len(p), nil
}
func (f *fakeSerial) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending.Read(p)
}
func (f *fakeSerial) Close() error { return nil }

func newTestSerial(fs *fakeSerial, cfg SerialConfig) *serialTransport {
	st := NewSerialTransport(cfg).(*serialTransport)
	st.rw = fs                           // bypass Open
	st.probeWait = time.Millisecond * 10 // shrink the 1s probe wait for tests
	return st
}

func TestEnterKISSSendsInitWhenCommandMode(t *testing.T) {
	fs := &fakeSerial{responses: [][]byte{[]byte("cmd:"), {}}} // cmd mode, then silent
	st := newTestSerial(fs, SerialConfig{
		InitString: `KISS ON\rRESTART\r`, InitDelay: time.Millisecond})
	if err := st.EnterKISS(); err != nil {
		t.Fatal(err)
	}
	got := fs.written.String()
	if !strings.Contains(got, "KISS ON\r") || !strings.Contains(got, "RESTART\r") {
		t.Fatalf("written = %q", got)
	}
}

func TestEnterKISSSkipsInitWhenAlreadyKISS(t *testing.T) {
	fs := &fakeSerial{responses: [][]byte{{0xC0, 0x00, 0xC0}}} // KISS echo, not text
	st := newTestSerial(fs, SerialConfig{
		InitString: `KISS ON\r`, InitDelay: time.Millisecond})
	if err := st.EnterKISS(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fs.written.String(), "KISS ON") {
		t.Fatal("init sent although TNC already in KISS mode")
	}
}

func TestEnterKISSErrorsWhenInitFails(t *testing.T) {
	fs := &fakeSerial{responses: [][]byte{[]byte("cmd:"), []byte("cmd:")}}
	st := newTestSerial(fs, SerialConfig{
		InitString: `WRONG\r`, InitDelay: time.Millisecond})
	if err := st.EnterKISS(); err == nil {
		t.Fatal("expected error when TNC stays in command mode")
	}
}

func TestExitKISSSequence(t *testing.T) {
	fs := &fakeSerial{}
	st := newTestSerial(fs, SerialConfig{
		SendKISSExit: true, HostExitString: `KISS OFF\r`,
		ExitDelay: time.Millisecond})
	st.ExitKISS()
	got := fs.written.Bytes()
	exitIdx := bytes.Index(got, []byte{0xC0, 0xFF, 0xC0})
	hostIdx := bytes.Index(got, []byte("KISS OFF\r"))
	if exitIdx == -1 || hostIdx == -1 || hostIdx < exitIdx {
		t.Fatalf("exit sequence wrong: % x", got)
	}
}

func TestExitKISSDisabled(t *testing.T) {
	fs := &fakeSerial{}
	st := newTestSerial(fs, SerialConfig{SendKISSExit: false})
	st.ExitKISS()
	if fs.written.Len() != 0 {
		t.Fatalf("wrote % x with send_kiss_exit=false", fs.written.Bytes())
	}
}

// fakeModemPort is a fake modemPort that implements io.ReadWriteCloser plus
// the modemPort interface, with configurable DTR/RTS errors.
// This simulates a PTY device that returns ENOTTY from SetDTR/SetRTS.
type fakeModemPort struct {
	fakeSerial
	dtrErr error
	rtsErr error
}

func (f *fakeModemPort) SetDTR(bool) error                  { return f.dtrErr }
func (f *fakeModemPort) SetRTS(bool) error                  { return f.rtsErr }
func (f *fakeModemPort) SetReadTimeout(time.Duration) error { return nil }

// TestOpenNonFatalDTRRTSError verifies that SetDTR/SetRTS failures (e.g. on a
// PTY device that does not support modem control signals) are non-fatal.
// Regression: the Go port previously aborted Open() on these errors, causing
// the port to remain offline and AGWPE connections to return BUSY immediately.
// The Python reference (kiss3/pyserial) never sets DTR/RTS, so they are
// always non-fatal in tncd.
func TestOpenNonFatalDTRRTSError(t *testing.T) {
	enotty := errors.New("inappropriate ioctl for device")
	fmp := &fakeModemPort{dtrErr: enotty, rtsErr: enotty}

	st := NewSerialTransport(SerialConfig{Device: "/dev/pts/fake"}).(*serialTransport)
	st.probeWait = time.Millisecond
	// Inject a fake openPort that returns our fake modem port (no real device needed).
	st.openPort = func(device string, mode *goserial.Mode) (modemPort, error) {
		return fmp, nil
	}

	// Open must succeed even though DTR and RTS return errors.
	if err := st.Open(); err != nil {
		t.Fatalf("Open() returned error: %v (expected nil — DTR/RTS errors should be non-fatal)", err)
	}

	// The rw field must be set so the transport can be used for reads/writes.
	if st.rw == nil {
		t.Fatal("st.rw is nil after Open() with non-fatal DTR/RTS error")
	}

	// Read and Write must delegate to the fake port without panicking.
	data := []byte{0xC0, 0x00, 0xC0}
	if _, err := st.rw.(io.Writer).Write(data); err != nil {
		t.Fatalf("Write after Open() failed: %v", err)
	}
}

func TestEnterKISSRetryCatchesSlowTNC(t *testing.T) {
	// Silent on the first two probes (TNC still rebooting), cmd text on the 3rd.
	fs := &fakeSerial{responses: [][]byte{{}, {}, []byte("cmd:"), {}}}
	st := newTestSerial(fs, SerialConfig{InitString: `INT KISS\rRESET\r`, InitDelay: time.Millisecond})
	if err := st.EnterKISS(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs.written.String(), "INT KISS\r") {
		t.Fatalf("init not sent after retry caught cmd mode; written=%q", fs.written.String())
	}
}

func TestEnterKISSSendsInitOnSilence(t *testing.T) {
	// Never any response: ambiguous. Init must be sent anyway; no error.
	fs := &fakeSerial{responses: [][]byte{{}, {}, {}}}
	st := newTestSerial(fs, SerialConfig{InitString: `INT KISS\rRESET\r`, InitDelay: time.Millisecond})
	if err := st.EnterKISS(); err != nil {
		t.Fatalf("silence should not error: %v", err)
	}
	if !strings.Contains(fs.written.String(), "INT KISS\r") {
		t.Fatalf("init not sent on silence; written=%q", fs.written.String())
	}
}

func TestOpenBusyPortClearError(t *testing.T) {
	st := NewSerialTransport(SerialConfig{Device: "/dev/ttyUSB9"}).(*serialTransport)
	st.openPort = func(string, *goserial.Mode) (modemPort, error) {
		return nil, syscall.EBUSY
	}
	err := st.Open()
	// Assert the FRIENDLY wording ("in use"), which is NOT in the raw errno
	// ("device or resource busy") — so this genuinely fails before the fix.
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("EBUSY open error = %v, want the friendly \"in use\" wording", err)
	}
}

func TestOpenOtherErrorUnchanged(t *testing.T) {
	st := NewSerialTransport(SerialConfig{Device: "/dev/ttyUSB9"}).(*serialTransport)
	st.openPort = func(string, *goserial.Mode) (modemPort, error) {
		return nil, errors.New("boom")
	}
	err := st.Open()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("non-EBUSY error = %v, want it wrapped through", err)
	}
}
