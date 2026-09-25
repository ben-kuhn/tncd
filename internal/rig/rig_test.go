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

// --- channel-record fixtures ------------------------------------------------
//
// Captured from a BTech UV-PRO on 2026-09-24. Channel 252 is the record VFO A
// points at -- the radio's VFO -- and is unnamed. Channel 1 is a real memory.
var (
	fakeVFORecord = []byte{
		0xfc, 0x08, 0xa4, 0xfb, 0x70, 0x08, 0xa4, 0xfb, 0x70,
		0x00, 0x00, 0x00, 0x00, 0x14, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	fakeNamedRecord = []byte{
		0x01, 0x08, 0xae, 0xbf, 0x70, 0x08, 0xae, 0xbf, 0x70,
		0x00, 0x00, 0x00, 0x00, 0x1c, 0x00,
		'M', 'N', ' ', 'P', 'a', 'c', 'k', 0, 0, 0,
	}
	// A READ_SETTINGS reply putting VFO A on channel 252, VFO B on 1, dual
	// watch off.
	fakeSettingsCh252 = []byte{
		0x00,
		0xc1, 0x04, 0xa6, 0x06, 0x18, 0x01, 0x3c, 0xe0, 0xa3, 0xf0,
		0x00, 0x20, 0x00, 0x00, 0x08, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

// statusExtFor builds a GET_HT_STATUS StatusExt reply reporting channel id.
// curr_ch_id is split: the low nibble is the high nibble of the second Status
// byte, the high nibble sits in the trailing word at bits 2..5.
func statusExtFor(id byte) []byte {
	tail := uint16(id>>4) << 2
	out := []byte{0x00, 0x80, (id & 0x0F) << 4, 0, 0}
	binary.BigEndian.PutUint16(out[3:5], tail)
	return out
}

// statusTX builds a plain GET_HT_STATUS reply with is_in_tx set or clear.
// is_in_tx is bit 6 of the first Status byte (see htStatusTXBit).
func statusTX(keyed bool) []byte {
	b := byte(0x80) // is_power_on
	if keyed {
		b |= htStatusTXBit
	}
	return []byte{0x00, b, 0x00}
}

// awaitWrite blocks until the rig has written at least n frames.
//
// Replies must be enqueued only AFTER the request they answer has gone out:
// dispatch routes a reply to whatever request is waiting for that command
// type, so a reply queued early is delivered while nothing is waiting and is
// silently dropped, wedging every later request in the sequence.
func (f *fakeChannel) awaitWrite(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.written)
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for write %d", n)
}

// scriptActiveChannel answers the three requests activeChannel makes, each
// only once its request has actually been sent. base is how many frames the
// rig had already written before this exchange began.
func scriptActiveChannel(t *testing.T, ch *fakeChannel, base int, settings []byte, id byte, record []byte) {
	t.Helper()
	ch.awaitWrite(t, base+1)
	ch.reply(benshi.CmdReadSettings, settings)
	ch.awaitWrite(t, base+2)
	ch.reply(benshi.CmdGetHTStatus, statusExtFor(id))
	ch.awaitWrite(t, base+3)
	ch.reply(benshi.CmdReadRFCh, append([]byte{0x00}, record...))
}

// TestSetFreqRewritesTheActiveVFORecord is the core of QSY on this hardware:
// the radio has no scratch frequency register, so tuning means rewriting the
// channel record the active VFO points at, preserving every other field.
func TestSetFreqRewritesTheActiveVFORecord(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		scriptActiveChannel(t, ch, 0, fakeSettingsCh252, 252, fakeVFORecord)
		ch.awaitWrite(t, 4)
		ch.reply(benshi.CmdWriteRFCh, []byte{0x00, 0xfc})
	}()

	const target = 145670000
	if err := r.SetFreq(target); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}
	w := ch.writes()
	if len(w) != 4 {
		t.Fatalf("wrote %d frames, want 4 (settings, status, read, write)", len(w))
	}
	m, err := benshi.DecodeMessage(w[3][4:])
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if m.Command != benshi.CmdWriteRFCh {
		t.Fatalf("final command = %d, want CmdWriteRFCh", m.Command)
	}
	got, err := benshi.ParseRFCh(m.Body)
	if err != nil {
		t.Fatalf("ParseRFCh(written): %v", err)
	}
	if got.ID() != 252 {
		t.Errorf("wrote channel %d, want 252", got.ID())
	}
	if got.RXFreqHz() != target || got.TXFreqHz() != target {
		t.Errorf("wrote rx/tx %d/%d, want %d both", got.RXFreqHz(), got.TXFreqHz(), target)
	}
	// Everything past the frequency words must survive verbatim, or a QSY
	// would silently reset sub-audio, bandwidth and power flags.
	if !bytes.Equal(m.Body[9:], fakeVFORecord[9:]) {
		t.Errorf("QSY changed fields past the frequencies:\n got % x\nwant % x", m.Body[9:], fakeVFORecord[9:])
	}
}

// TestSetFreqRefusesNamedChannel is the guard that protects the operator's
// memories: a named record means the radio is on a stored channel, not its
// VFO, and rewriting it would destroy that memory.
func TestSetFreqRefusesNamedChannel(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		scriptActiveChannel(t, ch, 0, fakeSettingsCh252, 252, fakeNamedRecord)
	}()

	err := r.SetFreq(145670000)
	if !errors.Is(err, ErrChannelMode) {
		t.Fatalf("SetFreq on a named channel: err = %v, want ErrChannelMode", err)
	}
	for _, w := range ch.writes() {
		m, derr := benshi.DecodeMessage(w[4:])
		if derr == nil && m.Command == benshi.CmdWriteRFCh {
			t.Fatal("SetFreq wrote a channel record despite refusing -- a real memory would have been destroyed")
		}
	}
}

// TestSetFreqRefusesDualWatch covers the other refusal: with dual watch on,
// which VFO a transmission goes out on is not derivable, so writing either
// record could retune a band the operator is still listening to.
func TestSetFreqRefusesDualWatch(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	dual := append([]byte{}, fakeSettingsCh252...)
	dual[2] |= 0x20 // double_channel = B (bits 10..11 of the record)

	go func() {
		ch.awaitWrite(t, 1)
		ch.reply(benshi.CmdReadSettings, dual)
	}()

	if err := r.SetFreq(145670000); !errors.Is(err, ErrDualWatch) {
		t.Fatalf("SetFreq in dual watch: err = %v, want ErrDualWatch", err)
	}
}

// TestSetFreqRefusesWhenSettingsAndStatusDisagree covers a radio in a state
// this code does not model. Refusing beats guessing when the next step writes.
func TestSetFreqRefusesWhenSettingsAndStatusDisagree(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		ch.awaitWrite(t, 1)
		ch.reply(benshi.CmdReadSettings, fakeSettingsCh252) // says 252
		ch.awaitWrite(t, 2)
		ch.reply(benshi.CmdGetHTStatus, statusExtFor(1)) // says 1
	}()

	if err := r.SetFreq(145670000); !errors.Is(err, ErrVFOAmbiguous) {
		t.Fatalf("SetFreq with disagreeing state: err = %v, want ErrVFOAmbiguous", err)
	}
}

// TestGetFreqReadsTheActiveVFORecord proves GetFreq answers from the record
// the radio actually operates on rather than the frequency-mode register,
// which on real firmware can hold a stale value the radio is not tuned to.
func TestGetFreqReadsTheActiveVFORecord(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		scriptActiveChannel(t, ch, 0, fakeSettingsCh252, 252, fakeVFORecord)
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("GetFreq: %v", err)
	}
	if hz != 145030000 {
		t.Errorf("GetFreq = %d, want 145030000 (the VFO record's rx frequency)", hz)
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

// TestTeardownRestoresTheDisplacedRecord proves Teardown puts back what this
// session's first SetFreq displaced -- byte for byte, from the saved copy.
//
// It deliberately does NOT re-read the radio first. After a SetFreq the
// radio's own record holds this code's QSY, so a read-then-write would be a
// no-op dressed up as a restore; the previous implementation did exactly that
// and was silently useless.
func TestTeardownRestoresTheDisplacedRecord(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		scriptActiveChannel(t, ch, 0, fakeSettingsCh252, 252, fakeVFORecord)
		ch.awaitWrite(t, 4)
		ch.reply(benshi.CmdWriteRFCh, []byte{0x00, 0xfc})
	}()
	if err := r.SetFreq(145670000); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}

	go func() {
		ch.awaitWrite(t, 5)
		ch.reply(benshi.CmdWriteRFCh, []byte{0x00, 0xfc})
	}()
	restored, err := r.Teardown()
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !restored {
		t.Fatal("Teardown reported nothing to restore after a SetFreq")
	}

	w := ch.writes()
	m, err := benshi.DecodeMessage(w[len(w)-1][4:])
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if m.Command != benshi.CmdWriteRFCh {
		t.Fatalf("final command = %d, want CmdWriteRFCh", m.Command)
	}
	if !bytes.Equal(m.Body, fakeVFORecord) {
		t.Errorf("Teardown wrote\n got % x\nwant % x (the original record verbatim)", m.Body, fakeVFORecord)
	}
}

// TestTeardownWithoutSetFreqIsANoOp: a session that never tuned has nothing
// to undo, and that is not an error -- but it must also not write anything.
func TestTeardownWithoutSetFreqIsANoOp(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	restored, err := r.Teardown()
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if restored {
		t.Error("Teardown claimed to restore something on a session that never tuned")
	}
	if n := len(ch.writes()); n != 0 {
		t.Errorf("Teardown sent %d frames, want 0", n)
	}
}

// TestTeardownRestoresTheOPERATORsFrequencyNotOurOwn: only the FIRST record
// SetFreq displaces is the operator's. Saving a later one would strand the
// radio on a frequency this code chose.
func TestTeardownRestoresTheOperatorsFrequencyNotOurOwn(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	base := 0
	qsy := func(hz uint32, record []byte) {
		b := base
		go func() {
			scriptActiveChannel(t, ch, b, fakeSettingsCh252, 252, record)
			ch.awaitWrite(t, b+4)
			ch.reply(benshi.CmdWriteRFCh, []byte{0x00, 0xfc})
		}()
		if err := r.SetFreq(hz); err != nil {
			t.Fatalf("SetFreq(%d): %v", hz, err)
		}
		base += 4
	}
	qsy(145670000, fakeVFORecord)
	// Second QSY: the radio now reports what we wrote.
	tuned, err := benshi.ParseRFCh(fakeVFORecord)
	if err != nil {
		t.Fatalf("ParseRFCh: %v", err)
	}
	qsy(145710000, tuned.WithFreq(145670000).Bytes())

	go func() {
		ch.awaitWrite(t, base+1)
		ch.reply(benshi.CmdWriteRFCh, []byte{0x00, 0xfc})
	}()
	if _, err := r.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	w := ch.writes()
	m, err := benshi.DecodeMessage(w[len(w)-1][4:])
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if !bytes.Equal(m.Body, fakeVFORecord) {
		t.Errorf("Teardown restored the wrong record:\n got % x\nwant % x (the operator's original)", m.Body, fakeVFORecord)
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
		_, err := r.GetPTT()
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

	if _, err := r.GetPTT(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("request1 err = %v, want ErrTimeout", err)
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		ch.reply(benshi.CmdGetHTStatus, statusTX(true))
	}()

	tx, err := r.GetPTT()
	if err != nil {
		t.Fatalf("request2: %v", err)
	}
	if !tx {
		t.Error("GetPTT = false, want true (request2's own reply)")
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

	staleReply := statusTX(false) // request1's straggler: TX idle
	freshReply := statusTX(true)  // request2's own answer: TX keyed

	var wg sync.WaitGroup
	wg.Add(2)
	defer wg.Wait() // let both scheduled replies finish sending before r.Close() runs

	go func() {
		defer wg.Done()
		time.Sleep(timeout + 10*time.Millisecond) // shortly after request1's own deadline
		ch.reply(benshi.CmdGetHTStatus, staleReply)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(timeout + 30*time.Millisecond)
		ch.reply(benshi.CmdGetHTStatus, freshReply)
	}()

	if _, err := r.GetPTT(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("request1 err = %v, want ErrTimeout", err)
	}

	tx, err := r.GetPTT()
	if err != nil {
		t.Fatalf("request2: %v", err)
	}
	if !tx {
		t.Error("GetPTT = false, want true (request2's own fresh reply, not request1's stale straggler)")
	}
}

// (3) CONSECUTIVE losses: several requests in a row get no reply at all; a
// later request must still get its own correct reply, with no trailing.
func TestConsecutiveLossesDoNotTrail(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, 10*time.Millisecond)
	defer r.Close()

	for i := 0; i < 3; i++ {
		if _, err := r.GetPTT(); !errors.Is(err, ErrTimeout) {
			t.Fatalf("request %d err = %v, want ErrTimeout", i+1, err)
		}
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		ch.reply(benshi.CmdGetHTStatus, statusTX(true))
	}()

	tx, err := r.GetPTT()
	if err != nil {
		t.Fatalf("final request: %v, want a clean reply -- consecutive losses must not trail", err)
	}
	if !tx {
		t.Error("GetPTT = false, want true")
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

// The CHANNEL-mode fallback this file used to test is gone along with the
// command it fell back FROM. GetFreq no longer consults FREQ_MODE_GET_STATUS
// at all: that command answers from a frequency-mode register the radio does
// not necessarily operate on, so "fall back when it looks wrong" was papering
// over reading the wrong register in the first place. GetFreq now reads the
// active VFO's channel record unconditionally -- see
// TestGetFreqReadsTheActiveVFORecord.
