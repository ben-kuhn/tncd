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

// blockingChannel never returns from Write or Read until told to, so tests
// can verify a request's timeout bounds the write itself, not just the wait
// for a reply.
type blockingChannel struct {
	unblock chan struct{}
}

func newBlockingChannel() *blockingChannel {
	return &blockingChannel{unblock: make(chan struct{})}
}

func (b *blockingChannel) Write(p []byte) (int, error) {
	<-b.unblock
	return len(p), nil
}

func (b *blockingChannel) Read(p []byte) (int, error) {
	<-b.unblock
	return 0, io.EOF
}

func (b *blockingChannel) Close() error { return nil }

// A wedged transport that accepts a Write and never returns must not wedge
// the caller either -- CLAUDE.md documents exactly this failure mode on a
// stuck Bluetooth SPP socket. The call includes the post-timeout quiet
// period (see quietPeriodMultiplier), so the bound here is timeout +
// quietPeriod, not the bare timeout.
func TestRequestTimesOutEvenWhenWriteBlocks(t *testing.T) {
	ch := newBlockingChannel()
	defer close(ch.unblock) // release the leaked internal Write/Read goroutines
	timeout := 50 * time.Millisecond
	r := New(ch, timeout)
	defer r.Close()

	start := time.Now()
	err := r.SetFreq(145030000)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	want := timeout * (1 + quietPeriodMultiplier)
	if elapsed := time.Since(start); elapsed > want+time.Second {
		t.Errorf("took %v, want roughly %v (timeout + quiet period)", elapsed, want)
	}
}

// errCloseChannel returns a fixed error from Close, to verify that error is
// reported to every caller, not just the one that happened to run first.
type errCloseChannel struct {
	*fakeChannel
	closeErr error
}

func (e *errCloseChannel) Close() error {
	e.fakeChannel.Close()
	return e.closeErr
}

func TestCloseReturnsErrorToAllCallers(t *testing.T) {
	wantErr := errors.New("boom")
	ch := &errCloseChannel{fakeChannel: newFakeChannel(), closeErr: wantErr}
	r := New(ch, time.Second)

	err1 := r.Close()
	err2 := r.Close()
	if !errors.Is(err1, wantErr) {
		t.Errorf("first Close() = %v, want %v", err1, wantErr)
	}
	if !errors.Is(err2, wantErr) {
		t.Errorf("second Close() = %v, want %v (repeat calls must return the stored error, not nil)", err2, wantErr)
	}
}

// Close must wake a request that's blocked waiting on a reply, rather than
// making it wait out its full timeout.
func TestCloseUnblocksInFlightRequest(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, 5*time.Second) // long enough that only Close should unblock this

	done := make(chan error, 1)
	go func() {
		_, err := r.GetFreq()
		done <- err
	}()

	time.Sleep(20 * time.Millisecond) // let the request start waiting
	start := time.Now()
	r.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Close took %v to unblock the request, want near-instant", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not unblock after Close")
	}
}

// Fix round 3 replaced the reply-matching schemes from rounds 0-2 (command-
// type-only match, then two FIFO-generation designs) with a quiet-period
// barrier: after a request times out, request() itself waits out
// quietPeriodMultiplier*timeout (or an early straggler, or Close) before
// returning, still holding reqMu, so no later request can be issued while a
// straggler for the timed-out one could still arrive. See the doc comment on
// quietPeriodMultiplier in rig.go for why matching was abandoned rather than
// patched again. The four tests below are the reviewer's required scenarios.

// (1) LOST reply: request1's reply never arrives at all. request1's own call
// already absorbs its quiet period before returning, so request2 can be
// issued immediately afterward and must get its own correct reply.
func TestLostReplyRequest2GetsOwnReply(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, 20*time.Millisecond)
	defer r.Close()

	if _, err := r.GetFreq(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("request1 err = %v, want ErrTimeout", err)
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeGetStatus, []byte{0x00, 0x08, 0xA4, 0xFB, 0x70})
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("request2: %v", err)
	}
	if hz != 145030000 {
		t.Errorf("GetFreq = %d, want 145030000", hz)
	}
}

// (2) LATE reply: a straggler that arrives during request1's quiet period
// must be discarded there, never delivered to request2.
//
// Both replies are scheduled on an absolute clock from test start, NOT
// relative to when request2 happens to begin -- that's deliberate. If the
// fresh reply's delay were measured from request2's own start, a broken
// barrier (request2 starting immediately at request1's timeout, instead of
// after the quiet period) could still coincidentally receive the fresh reply
// before the independently-scheduled stale one arrives, and the test would
// pass even with no barrier at all. Scheduling both on the same absolute
// clock guarantees the stale reply always arrives first in wall-clock time,
// so whichever request is listening when it fires is the one that (correctly
// or incorrectly) receives it.
func TestLateReplyDiscardedDuringQuietPeriod(t *testing.T) {
	ch := newFakeChannel()
	timeout := 40 * time.Millisecond
	r := New(ch, timeout) // quiet period = 3*timeout = 120ms
	defer r.Close()

	staleFreq := []byte{0x00, 0x08, 0x9E, 0x86, 0x00} // != 145030000
	freshFreq := []byte{0x00, 0x08, 0xA4, 0xFB, 0x70} // 145030000 Hz

	var wg sync.WaitGroup
	wg.Add(2)
	defer wg.Wait() // let both scheduled replies finish sending before r.Close() runs

	go func() {
		defer wg.Done()
		time.Sleep(timeout + 10*time.Millisecond) // shortly after request1's own deadline
		ch.reply(benshi.CmdFreqModeGetStatus, staleFreq)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(timeout + 30*time.Millisecond)
		ch.reply(benshi.CmdFreqModeGetStatus, freshFreq)
	}()

	if _, err := r.GetFreq(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("request1 err = %v, want ErrTimeout", err)
	}

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("request2: %v", err)
	}
	if hz != 145030000 {
		t.Errorf("GetFreq = %d, want 145030000 (request2's own fresh reply, not request1's stale straggler)", hz)
	}
}

// (3) CONSECUTIVE losses: several requests in a row get no reply at all; a
// later request must still get its own correct reply, with no trailing.
func TestConsecutiveLossesDoNotTrail(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, 10*time.Millisecond)
	defer r.Close()

	for i := 0; i < 3; i++ {
		if _, err := r.GetFreq(); !errors.Is(err, ErrTimeout) {
			t.Fatalf("request %d err = %v, want ErrTimeout", i+1, err)
		}
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeGetStatus, []byte{0x00, 0x08, 0xA4, 0xFB, 0x70})
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("final request: %v, want a clean reply -- consecutive losses must not trail", err)
	}
	if hz != 145030000 {
		t.Errorf("GetFreq = %d, want 145030000", hz)
	}
}

// (4) Close during the quiet period must return promptly with ErrClosed,
// rather than waiting the period out.
func TestCloseDuringQuietPeriodReturnsPromptly(t *testing.T) {
	ch := newFakeChannel()
	timeout := 20 * time.Millisecond
	r := New(ch, timeout) // quiet period = 60ms
	defer r.Close()

	done := make(chan error, 1)
	go func() {
		_, err := r.GetFreq()
		done <- err
	}()

	// Let request1 pass its own timeout and enter the quiet period.
	time.Sleep(timeout + 10*time.Millisecond)
	start := time.Now()
	r.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("Close took %v to unblock the quiet period, want near-instant", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not unblock after Close")
	}
}
