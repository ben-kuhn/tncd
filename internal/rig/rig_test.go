package rig

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// fakeChannel is a control channel whose replies are scripted by the test.
type fakeChannel struct {
	mu      sync.Mutex
	written [][]byte
	replies chan []byte
	closed  bool
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{replies: make(chan []byte, 8)}
}

func (f *fakeChannel) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	f.written = append(f.written, cp)
	return len(p), nil
}

func (f *fakeChannel) Read(p []byte) (int, error) {
	b, ok := <-f.replies
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.replies)
	}
	return nil
}

func (f *fakeChannel) writes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.written))
	copy(out, f.written)
	return out
}

// reply enqueues a reply message for the given command.
func (f *fakeChannel) reply(cmd benshi.Command, body []byte) {
	m := benshi.Message{Group: benshi.GroupBasic, IsReply: true, Command: cmd, Body: body}
	f.replies <- benshi.Frame{Flags: benshi.FlagNone, Data: m.Bytes()}.Bytes()
}

func TestSetFreqSendsFreqModeSetPar(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeSetPar, []byte{0x00})
	}()

	if err := r.SetFreq(145030000); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}
	w := ch.writes()
	if len(w) != 1 {
		t.Fatalf("wrote %d frames, want 1", len(w))
	}
	m, err := benshi.DecodeMessage(w[0][4:])
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if m.Command != benshi.CmdFreqModeSetPar {
		t.Errorf("command = %d, want CmdFreqModeSetPar", m.Command)
	}
	if len(m.Body) != 16 {
		t.Errorf("body length = %d, want 16", len(m.Body))
	}
}

func TestGetFreqUsesStatusReply(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeGetStatus, []byte{0x00, 0x09, 0xB0, 0x50, 0xF0})
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("GetFreq: %v", err)
	}
	if hz != 162550000 {
		t.Errorf("GetFreq = %d, want 162550000", hz)
	}
}

// A radio that never answers must not wedge the caller.
func TestRequestTimesOut(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, 50*time.Millisecond)
	defer r.Close()

	start := time.Now()
	err := r.SetFreq(145030000)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want roughly the 50ms timeout", elapsed)
	}
}

// Teardown must send the documented all-zero payload.
func TestTeardownSendsAllZeroPayload(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeSetPar, []byte{0x00})
	}()

	if err := r.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	w := ch.writes()
	m, _ := benshi.DecodeMessage(w[0][4:])
	for i, b := range m.Body {
		if b != 0 {
			t.Fatalf("teardown body byte %d = %#x, want 0", i, b)
		}
	}
}

// A pushed notification updates the cache, so GetFreq need not hit the radio.
func TestNotificationPopulatesCache(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	body := []byte{14, 0x08, 0xA4, 0xFB, 0x70, 0x08, 0xA4, 0xFB, 0x70,
		0, 0, 0, 0, 0x00, 0x40}
	ch.reply(benshi.CmdEventNotification, body)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if hz, ok := r.CachedFreq(); ok && hz == 145030000 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("notification never reached the cache")
}

func TestGetFreqFallsBackToChannelRead(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// FREQ_MODE_GET_STATUS reports failure: not in frequency mode.
		ch.reply(benshi.CmdFreqModeGetStatus, []byte{0x01})
		time.Sleep(10 * time.Millisecond)
		// GET_HT_STATUS: reply_status 0, then Status. curr_ch_id_lower is the
		// high nibble of the second status byte; channel 3 here.
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0x80, 0x30})
		time.Sleep(10 * time.Millisecond)
		// READ_RF_CH: status, channel_id, tx word, rx word (145.030 MHz).
		ch.reply(benshi.CmdReadRFCh, []byte{
			0x00, 0x03,
			0x08, 0xA4, 0xFB, 0x70,
			0x08, 0xA4, 0xFB, 0x70,
		})
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("GetFreq: %v", err)
	}
	if hz != 145030000 {
		t.Errorf("GetFreq = %d, want 145030000 via the channel-mode fallback", hz)
	}
}

func TestGetPTTReadsTXBit(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// is_in_tx is bit 1 from the MSB of the first status byte.
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0xC0, 0x00})
	}()

	on, err := r.GetPTT()
	if err != nil {
		t.Fatalf("GetPTT: %v", err)
	}
	if !on {
		t.Error("GetPTT = false, want true when is_in_tx is set")
	}
}

func TestProbeRejectsFailureStatus(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdGetDevInfo, []byte{0x01})
	}()

	if err := r.Probe(); err == nil {
		t.Error("Probe must fail when the radio reports a failure status")
	}
}
