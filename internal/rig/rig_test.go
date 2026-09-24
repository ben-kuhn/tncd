package rig

import (
	"bytes"
	"encoding/binary"
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

// Teardown must restore the frequency read from the current channel's
// stored record, NOT send the documented all-zero FREQ_MODE_SET_PAR payload.
// Live UV-PRO testing showed that payload does not exit frequency mode as
// benshi.TeardownPayload's old doc comment (and the upstream spec) claimed --
// the radio takes it literally as "tune to 0 Hz" and clamps to 136.000 MHz,
// the bottom of its VHF range, silently relocating the operator's radio to
// the band edge. A test asserting the all-zero payload would be asserting
// that broken behavior, so there is deliberately no such test here any more.
func TestTeardownRestoresChannelFrequency(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// Captured live: GET_HT_STATUS (StatusExt) -> channel 252.
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0x80, 0xC1, 0x00, 0x3C})
		time.Sleep(10 * time.Millisecond)
		// Captured live: READ_RF_CH(252) -> 145.670 MHz.
		ch.reply(benshi.CmdReadRFCh, []byte{
			0x00, 0xFC, 0x08, 0xAE, 0xBF, 0x70, 0x08, 0xAE, 0xBF, 0x70,
			0x00, 0x00, 0x00, 0x00, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		})
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeSetPar, []byte{0x00})
	}()

	if err := r.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	w := ch.writes()
	if len(w) != 3 {
		t.Fatalf("wrote %d frames, want 3 (GET_HT_STATUS, READ_RF_CH, FREQ_MODE_SET_PAR)", len(w))
	}
	m, err := benshi.DecodeMessage(w[2][4:])
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if m.Command != benshi.CmdFreqModeSetPar {
		t.Fatalf("final command = %d, want CmdFreqModeSetPar", m.Command)
	}
	if len(m.Body) != 16 {
		t.Fatalf("body length = %d, want 16", len(m.Body))
	}
	allZero := true
	for _, b := range m.Body {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("Teardown sent an all-zero payload -- on real firmware this clamps the radio to 136.000 MHz instead of restoring the channel frequency")
	}
	gotHz := binary.BigEndian.Uint32(m.Body[0:4]) & 0x3FFFFFFF
	if gotHz != 145670000 {
		t.Errorf("Teardown set frequency = %d, want 145670000 (the channel's stored frequency)", gotHz)
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

// SetPTT has no press/release parameter to assert on (see its doc comment)
// -- the only thing this package can prove is that it sends DO_PROG_FUNC
// with the MAIN_PTT effect byte, both for on and for off, since the wire
// message is identical either way.
func TestSetPTTSendsDoProgFuncMainPTT(t *testing.T) {
	for _, on := range []bool{true, false} {
		ch := newFakeChannel()
		r := New(ch, time.Second)

		go func() {
			time.Sleep(10 * time.Millisecond)
			ch.reply(benshi.CmdDoProgFunc, []byte{0x00})
		}()

		if err := r.SetPTT(on); err != nil {
			t.Fatalf("SetPTT(%v): %v", on, err)
		}
		r.Close()

		writes := ch.writes()
		if len(writes) != 1 {
			t.Fatalf("SetPTT(%v) wrote %d frames, want 1", on, len(writes))
		}
		dec := benshi.NewDecoder()
		frames, err := dec.Feed(writes[0])
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		if len(frames) != 1 {
			t.Fatalf("Feed decoded %d frames, want 1", len(frames))
		}
		msg, err := benshi.DecodeMessage(frames[0].Data)
		if err != nil {
			t.Fatalf("DecodeMessage: %v", err)
		}
		if msg.Command != benshi.CmdDoProgFunc {
			t.Errorf("SetPTT(%v) command = %v, want CmdDoProgFunc", on, msg.Command)
		}
		if len(msg.Body) != 1 || msg.Body[0] != byte(benshi.PFEffectMainPTT) {
			t.Errorf("SetPTT(%v) body = %v, want [%d] (PFEffectMainPTT)", on, msg.Body, benshi.PFEffectMainPTT)
		}
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
// stuck Bluetooth SPP socket. A write timeout poisons the rig immediately
// (fix round 4) rather than entering the quiet period, so the bound here is
// roughly the bare timeout, not timeout+quietPeriod.
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
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want roughly the %v timeout", elapsed, timeout)
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

// Fix round 4: a write timeout is handled differently from a reply timeout
// (see the write-timeout branch in request() and the quietPeriodMultiplier
// doc comment). A reply timeout means the write is known to have landed, so
// waiting out a quiet period for a straggler is safe. A write timeout means
// the byte stream's state is UNKNOWN -- a wedged-then-unwedged transport can
// complete the abandoned write at any later, unbounded time, and a second
// write while that's still possible would interleave bytes on the wire. So a
// write timeout poisons the rig (closes it) instead of entering the quiet
// period: no bound would be safe, and there is nothing to wait for since the
// problem isn't a missing reply, it's an unknown stream.

// gatedWriteChannel wraps fakeChannel with a Write that blocks until the
// test releases a gate, letting a test control exactly when an abandoned
// write -- one whose deadline already fired -- finally lands on the wire.
type gatedWriteChannel struct {
	*fakeChannel
	gate chan struct{}
}

func newGatedWriteChannel() *gatedWriteChannel {
	return &gatedWriteChannel{fakeChannel: newFakeChannel(), gate: make(chan struct{})}
}

func (g *gatedWriteChannel) Write(p []byte) (int, error) {
	<-g.gate
	return g.fakeChannel.Write(p)
}

// (5) Write-timeout-then-late-landing: the reviewer's exact reproduction.
// request1's write is gated so it can't complete in time; request1 times out
// on the write side and the rig is poisoned. The gate is then released,
// letting the abandoned write finally land -- well after request1 already
// gave up -- and request2 must NOT be able to receive whatever that
// produces: it must fail fast with ErrClosed, and it must never attempt a
// second write into the same (possibly corrupted) stream.
//
// TestRequestTimesOutEvenWhenWriteBlocks (round 1) cannot observe this: its
// blockingChannel blocks forever, so the abandoned write never lands and the
// hole this test covers never had a chance to show up there.
func TestWriteTimeoutPoisonsAgainstLateLandingWrite(t *testing.T) {
	ch := newGatedWriteChannel()
	r := New(ch, 20*time.Millisecond)
	defer r.Close()

	if err := r.SetFreq(145030000); !errors.Is(err, ErrTimeout) {
		t.Fatalf("request1 err = %v, want ErrTimeout", err)
	}

	// Release the gate: the abandoned write from request1 finally completes,
	// well after request1 itself already returned.
	close(ch.gate)
	time.Sleep(20 * time.Millisecond) // let the abandoned write actually land

	if _, err := r.GetFreq(); !errors.Is(err, ErrClosed) {
		t.Fatalf("request2 err = %v, want ErrClosed (the rig must be poisoned)", err)
	}

	w := ch.writes()
	if len(w) != 1 {
		t.Errorf("wrote %d frames, want 1 (request1's late write only -- request2 must never write into a poisoned rig)", len(w))
	}
}

// (6) After a write timeout, every subsequent call must fail fast with the
// poisoned error rather than attempting another write.
func TestWriteTimeoutPoisonsAllSubsequentCalls(t *testing.T) {
	ch := newGatedWriteChannel()
	defer close(ch.gate) // release the leaked abandoned-write goroutine
	r := New(ch, 20*time.Millisecond)
	defer r.Close()

	if err := r.SetFreq(145030000); !errors.Is(err, ErrTimeout) {
		t.Fatalf("request1 err = %v, want ErrTimeout", err)
	}

	if err := r.SetFreq(145030000); !errors.Is(err, ErrClosed) {
		t.Errorf("SetFreq after write-timeout poisoning = %v, want ErrClosed", err)
	}
	if _, err := r.GetFreq(); !errors.Is(err, ErrClosed) {
		t.Errorf("GetFreq after write-timeout poisoning = %v, want ErrClosed", err)
	}
	if err := r.Probe(); !errors.Is(err, ErrClosed) {
		t.Errorf("Probe after write-timeout poisoning = %v, want ErrClosed", err)
	}

	w := ch.writes()
	if len(w) != 0 {
		t.Errorf("wrote %d frames after poisoning, want 0 -- no call may write into a poisoned rig", len(w))
	}
}

// Three bugs found driving an actual UV-PRO. Fixture bytes below are
// captured live, not invented: the radio was parked on channel 252 at
// 145.670 MHz (145670000 Hz = 0x08AEBF70).

// Bug A: Probe() sent an empty GET_DEV_INFO body. The command requires a
// one-byte body (benlink's GetDevInfoBody, value 3) -- without it the radio
// answers nothing at all (confirmed live: 10s of silence). This test checks
// the exact wire bytes against benlink's own frame for the same command.
func TestProbeEmitsBenlinkMatchingFrame(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// Captured live GET_DEV_INFO reply body.
		ch.reply(benshi.CmdGetDevInfo, []byte{
			0x00, 0x06, 0x01, 0x04, 0x01, 0x00, 0x92, 0xD0, 0x68, 0x1E, 0x54,
		})
	}()

	if err := r.Probe(); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	w := ch.writes()
	if len(w) != 1 {
		t.Fatalf("wrote %d frames, want 1", len(w))
	}
	// benlink's exact GET_DEV_INFO frame: FF 01 <flags> <len> <group> <cmd> <body>.
	want := []byte{0xFF, 0x01, 0x00, 0x01, 0x00, 0x02, 0x00, 0x04, 0x03}
	if !bytes.Equal(w[0], want) {
		t.Errorf("Probe wrote % X, want % X (benlink's exact GET_DEV_INFO frame)", w[0], want)
	}
}

// Bug B: currChannel read only the lower nibble of the channel id, which is
// correct for the plain (3-byte) Status reply but wrong for the extended
// (5-byte) StatusExt reply, whose trailing word carries the upper nibble.
// Both shapes are tested so a future change can't silently favor one.

// Plain Status (3 bytes): channel is the lower nibble alone. This is the
// existing TestGetFreqFallsBackToChannelRead / TestGetPTTReadsTXBit shape;
// this test isolates it against currChannel directly.
func TestCurrChannelPlainStatusUsesLowerNibbleOnly(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0x80, 0x30}) // channel 3
	}()

	id, err := r.currChannel()
	if err != nil {
		t.Fatalf("currChannel: %v", err)
	}
	if id != 3 {
		t.Errorf("currChannel = %d, want 3", id)
	}
}

// StatusExt (5 bytes), live capture: body 00 80 C1 00 3C on a radio parked
// on channel 252. Reading only the lower nibble (0xC1>>4=12) returns an
// unprogrammed channel record instead of the true upper<<4|lower=252,
// confirmed live by READ_RF_CH echoing channel_id=0xFC (252) back.
func TestCurrChannelStatusExtUsesUpperAndLowerNibble(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0x80, 0xC1, 0x00, 0x3C})
	}()

	id, err := r.currChannel()
	if err != nil {
		t.Fatalf("currChannel: %v", err)
	}
	if id != 252 {
		t.Errorf("currChannel = %d, want 252", id)
	}
}

// Bug C: GetFreq did not fall back when the radio is in CHANNEL mode. In
// that mode FREQ_MODE_GET_STATUS replies with status=0 and an all-zero body
// -- DecodeFreqModeStatus only treats a non-zero status as an error, so the
// old code returned 0 Hz as if it were a real frequency. This end-to-end
// test drives GetFreq through all three requests it needs in that case,
// using only captured live bytes, and checks the final result is the radio's
// real frequency (145.670 MHz), not 0.
func TestGetFreqFallsBackWhenFreqModeStatusIsAllZero(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// Captured live: FREQ_MODE_GET_STATUS reply while in channel mode --
		// status=0 (success) but an all-zero body, meaning "no frequency".
		ch.reply(benshi.CmdFreqModeGetStatus, []byte{
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		})
		time.Sleep(10 * time.Millisecond)
		// Captured live: GET_HT_STATUS (StatusExt) -> channel 252.
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0x80, 0xC1, 0x00, 0x3C})
		time.Sleep(10 * time.Millisecond)
		// Captured live: READ_RF_CH(252) -> 145.670 MHz.
		ch.reply(benshi.CmdReadRFCh, []byte{
			0x00, 0xFC, 0x08, 0xAE, 0xBF, 0x70, 0x08, 0xAE, 0xBF, 0x70,
			0x00, 0x00, 0x00, 0x00, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		})
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("GetFreq: %v", err)
	}
	if hz != 145670000 {
		t.Errorf("GetFreq = %d, want 145670000 (fell back to the channel read, not 0 Hz)", hz)
	}
}
