package kiss

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
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
