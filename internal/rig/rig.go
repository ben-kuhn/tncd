// Package rig drives a Benshi-protocol radio over a control channel.
//
// Everything here runs OFF the engine goroutine. A Benshi command is a
// round-trip to the radio with a timeout, and the engine owns all L2 state, so
// blocking it would stall AX.25 on every port for the duration.
//
// This package does not restore the radio's original frequency when a
// session ends unless a caller explicitly asks it to (see (*Rig).Teardown).
// hamlib sets no precedent for doing so automatically either: rigctld leaves
// a radio wherever it was last tuned on disconnect, so "restore on exit" was
// never a requirement here, and this package does not fake one.
package rig

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// ErrTimeout reports a radio that did not answer within the configured timeout.
var ErrTimeout = errors.New("rig: radio did not reply in time")

// ErrClosed reports use of a rig whose channel has been closed.
var ErrClosed = errors.New("rig: control channel closed")

// quietPeriodMultiplier sets how long request() blocks -- still holding
// reqMu, so no other request can start -- after its own REPLY timeout,
// before letting the next request begin. See waitOutQuietPeriod. It does NOT
// apply to a write timeout, which poisons the rig instead -- see the
// write-timeout branch in request() for why that case is different.
//
// The Benshi protocol carries no per-message correlation id, so once a
// request's write has landed but its reply times out, a reply of that
// command type that shows up afterward cannot be attributed to the request
// it answers. Three earlier designs here tried to guess anyway -- matching
// by command type alone, then two FIFO-generation schemes -- and each guess
// was wrong in some real scenario: dropping the straggler loses a genuine
// answer when it was merely slow; delivering it to whatever is waiting next
// misattributes it when the straggler was for an earlier, abandoned request.
// The only way to make an unlabeled reply unambiguous is to guarantee at
// most one request is ever outstanding, so nothing else can possibly be
// waiting when a straggler shows up -- hence holding off the NEXT request
// instead of trying to match the stray reply.
//
// This works because a reply timeout only ever follows a COMPLETED write: at
// that point the byte stream's state is known, so a bounded wait for a
// straggler is safe. A write timeout has no such guarantee (see
// quietPeriodMultiplier's use in request()), which is why it isn't handled
// the same way.
//
// The trade-off here is added latency on the request immediately after a
// reply timeout. That's acceptable: timeouts are rare on a working link, and
// rig control (QSY) happens before connecting to a station, not mid-QSO, so
// a delayed poll costs nothing a user notices.
const quietPeriodMultiplier = 3

// Rig is a request/response session with one radio.
//
// One request is in flight at a time: the radio is a single serial endpoint
// and replies carry no correlation id, so concurrent requests could not be
// matched to their commands. After a request's reply times out (its write
// already landed), the NEXT request is held for a quiet period rather than
// started immediately -- see quietPeriodMultiplier -- so a straggler reply
// for the abandoned request can never be mistaken for the answer to a new
// one. After a request's WRITE times out (unknown whether anything reached
// the wire), the rig is poisoned instead: every later call fails with
// ErrClosed rather than risking a second write interleaving with the
// abandoned one, or a later request receiving its eventual stray reply.
type Rig struct {
	ch      io.ReadWriteCloser
	timeout time.Duration

	// reqMu serializes whole request/response exchanges, including the
	// post-timeout quiet period -- that's what makes the period actually
	// block the next request rather than merely delaying its own return.
	reqMu sync.Mutex

	mu      sync.Mutex
	waiting chan benshi.Message // non-nil while a request awaits its reply
	waitCmd benshi.Command

	// orig is the channel record the first SetFreq of this session
	// displaced, kept so Teardown can restore it verbatim.
	orig      benshi.RFCh
	origOK    bool
	closed    bool
	closeErr  error
	closeOnce sync.Once
	closedCh  chan struct{} // closed by Close to unblock in-flight requests and the quiet period
}

// New starts a rig on ch. The reader goroutine runs until Close.
func New(ch io.ReadWriteCloser, timeout time.Duration) *Rig {
	r := &Rig{ch: ch, timeout: timeout, closedCh: make(chan struct{})}
	go r.readLoop()
	return r
}

// Close shuts the rig down and closes the underlying channel.
//
// closedCh is closed before the channel itself so any request blocked in its
// select -- or waiting out a post-timeout quiet period -- wakes immediately
// instead of waiting out its full duration. closeErr is stored on the struct
// rather than a local var: sync.Once blocks concurrent callers until the
// winning call to f returns, which is exactly the happens-before guarantee
// needed to let every caller -- not just the one that ran f -- read the real
// result instead of a zero-value nil.
func (r *Rig) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		close(r.closedCh)
		r.closeErr = r.ch.Close()
	})
	return r.closeErr
}

// ErrDualWatch reports a radio in dual-watch mode, where which VFO carries a
// transmission is not derivable from the settings record.
var ErrDualWatch = errors.New("rig: radio is in dual-watch mode; turn dual watch off before tuning")

// ErrChannelMode reports a radio parked on a named memory channel rather than
// on its VFO.
var ErrChannelMode = errors.New("rig: radio is on a named memory channel, not its VFO")

// ErrVFOAmbiguous reports the settings record and the live status disagreeing
// about which channel the radio is on.
var ErrVFOAmbiguous = errors.New("rig: radio's settings and status disagree about the active channel")

// activeChannel resolves which channel record the radio actually transmits on,
// and reads it.
//
// This is deliberately not just GET_HT_STATUS's curr_ch_id. That field reports
// whichever channel is live at the instant it is read, which under dual watch
// is whichever VFO last received -- not necessarily the one a transmission
// would go out on. The settings record names both VFOs explicitly, so it is
// the authority here, and curr_ch_id is used only to cross-check it: if the
// two disagree the radio is in a state this code does not model, and refusing
// is the only safe answer when the next step is a write.
func (r *Rig) activeChannel() (benshi.RFCh, error) {
	sb, err := r.request(benshi.CmdReadSettings, nil)
	if err != nil {
		return benshi.RFCh{}, err
	}
	set, err := benshi.DecodeSettings(sb)
	if err != nil {
		return benshi.RFCh{}, err
	}
	id, ok := set.ActiveChannel()
	if !ok {
		return benshi.RFCh{}, ErrDualWatch
	}
	live, err := r.currChannel()
	if err != nil {
		return benshi.RFCh{}, err
	}
	if live != id {
		return benshi.RFCh{}, fmt.Errorf("%w (settings say channel %d, status says %d)", ErrVFOAmbiguous, id, live)
	}
	body, err := r.request(benshi.CmdReadRFCh, []byte{id})
	if err != nil {
		return benshi.RFCh{}, err
	}
	if len(body) < 1 || body[0] != 0 {
		return benshi.RFCh{}, fmt.Errorf("rig: radio rejected READ_RF_CH for channel %d", id)
	}
	return benshi.ParseRFCh(body[1:])
}

// SetFreq tunes the radio's active VFO to hz simplex.
//
// It does this by rewriting the channel record the active VFO points at,
// because these radios have no separate scratch frequency register: a VFO is
// an index into the channel table, and "frequency mode" is a VFO pointed at
// an unnamed record near the top of that table (252 on a UV-PRO, with 251 as
// its partner). FREQ_MODE_SET_PAR, which reads like the command for this and
// is what this package used to send, writes a different register that the
// firmware never promotes to the operating frequency -- proven on hardware by
// tuning the radio's dial and watching channel 252 follow it while
// FREQ_MODE_GET_STATUS kept reporting a stale value this code had written.
//
// Because the target is a real channel record, SetFreq refuses rather than
// writes in three cases: dual watch on (which VFO transmits is unknowable),
// settings and live status disagreeing about the active channel, and -- the
// one that protects the operator's data -- a record carrying a NAME. A named
// record is a memory the operator programmed, which means the radio is in
// channel mode, not VFO mode; retuning it would silently destroy that memory.
// An unnamed record is a VFO scratch record and is fair game.
//
// The record is patched in place rather than rebuilt, so sub-audio,
// bandwidth, power flags, pre-emphasis bypass and any DMR fields survive the
// round trip untouched. v1 is simplex only, which is what Winlink RMS
// gateways need.
func (r *Rig) SetFreq(hz uint32) error {
	ch, err := r.activeChannel()
	if err != nil {
		return err
	}
	if name := ch.Name(); name != "" {
		return fmt.Errorf("%w: channel %d is %q -- switch the radio to VFO/frequency mode first",
			ErrChannelMode, ch.ID(), name)
	}
	r.rememberOriginal(ch)
	body, err := r.request(benshi.CmdWriteRFCh, ch.WithFreq(hz).Bytes())
	if err != nil {
		return err
	}
	if len(body) < 1 || body[0] != 0 {
		return fmt.Errorf("rig: radio rejected WRITE_RF_CH for channel %d", ch.ID())
	}
	return nil
}

// rememberOriginal records the first record SetFreq displaced this session, so
// Teardown can put the radio back exactly where the operator left it. Only the
// FIRST is kept: later SetFreqs in the same session are this code's own QSYs,
// and restoring one of those would strand the radio on a frequency the
// operator never chose.
func (r *Rig) rememberOriginal(ch benshi.RFCh) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.origOK {
		r.orig = ch
		r.origOK = true
	}
}

// GetFreq reads the frequency the radio's active VFO is tuned to.
//
// This reads the channel record the VFO points at, NOT FREQ_MODE_GET_STATUS.
// That command answers from a separate frequency-mode register which on real
// firmware can hold a value the radio is not operating on -- on the bench it
// reported a stale 145.670 MHz while the radio sat on 145.030 MHz, so
// trusting it means reporting a frequency the operator is not listening to.
func (r *Rig) GetFreq() (uint32, error) {
	ch, err := r.activeChannel()
	if err != nil {
		return 0, err
	}
	return ch.RXFreqHz(), nil
}

// Teardown puts the radio back on the frequency it was on before this
// session's first SetFreq, and reports whether there was anything to undo.
//
// This restores the saved ORIGINAL record rather than re-reading the radio:
// after SetFreq the radio's own record holds this code's QSY, so a
// read-then-write would be a no-op that merely looks like a restore. The
// earlier implementation did exactly that and was silently useless.
//
// Nothing to undo is not an error -- a session that never tuned has nothing
// to restore -- so callers get (false, nil) rather than a failure.
func (r *Rig) Teardown() (bool, error) {
	r.mu.Lock()
	orig, ok := r.orig, r.origOK
	r.mu.Unlock()
	if !ok {
		return false, nil
	}
	body, err := r.request(benshi.CmdWriteRFCh, orig.Bytes())
	if err != nil {
		return false, err
	}
	if len(body) < 1 || body[0] != 0 {
		return false, fmt.Errorf("rig: radio rejected WRITE_RF_CH restoring channel %d", orig.ID())
	}
	r.mu.Lock()
	r.origOK = false
	r.mu.Unlock()
	return true, nil
}

// htStatusTXBit is is_in_tx within the first Status byte. Status packs, MSB
// first: is_power_on, is_in_tx, is_sq, is_in_rx, double_channel(2), is_scan,
// is_radio -- so is_in_tx is bit 6.
const htStatusTXBit = 0x40

// SetPTT keys or unkeys the transmitter via DO_PROG_FUNC(MAIN_PTT).
//
// DO_PROG_FUNC's body is a single effect byte -- the protocol defines no
// press/release parameter for it, so this sends the exact same wire bytes
// (PFEffectMainPTT) regardless of on. HTCommander, the reference
// implementation this protocol was reverse-engineered against, SPECULATES
// that some effects have distinct LOW_TO_HIGH/HIGH_TO_LOW edge actions that
// amount to a press and a release, but its own source does not establish
// that for MAIN_PTT, and nothing here has been bench-verified to hold a key
// open on real hardware. A nil return means only "the radio accepted the
// command" -- it is NOT proof the transmitter is now in the requested
// state. Callers must not assume a remote key can be held, and must enforce
// their own maximum key time (see internal/frontend/rigctl's PTTTimeout).
func (r *Rig) SetPTT(on bool) error {
	_, err := r.request(benshi.CmdDoProgFunc, []byte{byte(benshi.PFEffectMainPTT)})
	return err
}

// GetPTT reports whether the radio is currently transmitting.
func (r *Rig) GetPTT() (bool, error) {
	body, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		return false, err
	}
	if len(body) < 2 || body[0] != 0 {
		return false, benshi.ErrShortBody
	}
	return body[1]&htStatusTXBit != 0, nil
}

// htStatusExtLen is the reply body length that distinguishes the extended
// GET_HT_STATUS reply (StatusExt: status + Status(2) + a trailing 16-bit
// word) from the plain one (status + Status(2) only). Only StatusExt carries
// the channel id's upper nibble -- see currChannel.
const htStatusExtLen = 5

// currChannel returns the radio's currently selected channel id from
// GET_HT_STATUS.
//
// curr_ch_id_lower is the high nibble of the second Status byte, but that's
// only half the id: on a StatusExt reply the trailing 16-bit word packs
// rssi(4) region(6) curr_ch_id_upper(4) pad(2), and the true channel is
// upper<<4|lower. Reading the lower nibble alone silently wraps any channel
// >= 16 to the wrong one -- live proof: body 00 80 C1 00 3C on a radio
// parked on channel 252 gives lower=12 (an unprogrammed record) instead of
// upper<<4|lower=15<<4|12=252, confirmed by READ_RF_CH echoing
// channel_id=0xFC for the latter. The plain (non-extended) Status reply has
// no trailing word at all, so there's no upper nibble to read -- decide
// which shape a given reply is from its length rather than guessing.
func (r *Rig) currChannel() (byte, error) {
	body, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		return 0, err
	}
	if len(body) < 3 || body[0] != 0 {
		return 0, benshi.ErrShortBody
	}
	lower := body[2] >> 4
	if len(body) < htStatusExtLen {
		return lower, nil
	}
	tail := binary.BigEndian.Uint16(body[3:5])
	upper := byte((tail >> 2) & 0x0F)
	return upper<<4 | lower, nil
}

// channelFreq reads a stored channel's RX frequency. READ_RF_CH is read-only;
// its writing counterpart is deliberately absent from this package.
func (r *Rig) channelFreq(id byte) (uint32, error) {
	body, err := r.request(benshi.CmdReadRFCh, []byte{id})
	if err != nil {
		return 0, err
	}
	// body: reply status, channel_id, tx word (mod<<30|freq), rx word.
	if len(body) < 10 || body[0] != 0 {
		return 0, benshi.ErrShortBody
	}
	return binary.BigEndian.Uint32(body[6:10]) & 0x3FFFFFFF, nil
}

// getDevInfoRequestByte is GET_DEV_INFO's one-byte request body. It's a
// protocol-required literal, not a parameter we choose -- benlink's
// GetDevInfoBody hardcodes the same value 3, and it isn't documented what it
// means. Confirmed live: an empty body (what this package sent before this
// fix) gets no reply at all -- 10s of silence -- while this exact byte gets
// an immediate answer.
const getDevInfoRequestByte = 0x03

// Probe confirms the radio answers the protocol at all, so callers can fail
// with a clear message rather than a timeout on every later command.
func (r *Rig) Probe() error {
	body, err := r.request(benshi.CmdGetDevInfo, []byte{getDevInfoRequestByte})
	if err != nil {
		return err
	}
	if len(body) < 1 || body[0] != 0 {
		return fmt.Errorf("rig: radio rejected GET_DEV_INFO")
	}
	return nil
}

// request sends one command and waits for its reply.
func (r *Rig) request(cmd benshi.Command, body []byte) ([]byte, error) {
	r.reqMu.Lock()
	defer r.reqMu.Unlock()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	replyCh := make(chan benshi.Message, 1)
	r.waiting = replyCh
	r.waitCmd = cmd
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.waiting = nil
		r.mu.Unlock()
	}()

	msg := benshi.Message{Group: benshi.GroupBasic, Command: cmd, Body: body}
	frame := benshi.Frame{Flags: benshi.FlagNone, Data: msg.Bytes()}
	raw := frame.Bytes()
	if raw == nil {
		return nil, fmt.Errorf("rig: command %d produced an unencodable frame", cmd)
	}

	deadline := time.NewTimer(r.timeout)
	defer deadline.Stop()

	// io.ReadWriteCloser has no deadline method, and a wedged Bluetooth SPP
	// socket is a documented failure mode here (see CLAUDE.md) that accepts
	// writes without ever delivering them -- so Write itself must be bounded,
	// not just the wait for a reply. writeErr is buffered so this goroutine
	// can never block trying to report a result nobody is listening for any
	// more, whether the deadline fires first or the rig is closed underneath
	// it.
	writeErr := make(chan error, 1)
	go func() {
		_, err := r.ch.Write(raw)
		writeErr <- err
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			return nil, fmt.Errorf("rig: write: %w", err)
		}
	case <-deadline.C:
		// A write timeout is fatal to the rig, not just to this request --
		// see the doc comment on quietPeriodMultiplier for why the quiet
		// period does not apply here. In short: we don't know whether any
		// bytes reached the wire, and a wedged-then-unwedged transport can
		// complete the abandoned write at any later, unbounded time, so
		// there's no finite window after which a straggler stops being
		// plausible (unlike a reply timeout, where the write is known to
		// have already landed). Letting a later request write into the same
		// stream risks interleaving bytes with whatever this write
		// eventually sends; letting a later request wait for a reply risks
		// receiving THIS write's answer instead of its own. Poisoning the
		// rig closes both doors: Close() here means every later call fails
		// fast with ErrClosed instead of touching the stream again, and any
		// eventual late reply lands on a wait channel nothing is listening
		// to (this request's own, already abandoned) rather than a live one.
		r.Close()
		return nil, ErrTimeout
	case <-r.closedCh:
		return nil, ErrClosed
	}

	select {
	case m := <-replyCh:
		return m.Body, nil
	case <-deadline.C:
		if r.waitOutQuietPeriod(replyCh) {
			return nil, ErrClosed
		}
		return nil, ErrTimeout
	case <-r.closedCh:
		return nil, ErrClosed
	}
}

// waitOutQuietPeriod blocks -- still holding reqMu, so no other request can
// start -- after this request's own REPLY timeout, giving a straggler reply
// room to show up and be harmlessly discarded before the next request is
// allowed to begin. See quietPeriodMultiplier for why this exists instead of
// trying to match the straggler to whichever request happens to be waiting
// later. It reports whether the rig was closed while waiting, so the caller
// can surface ErrClosed instead of ErrTimeout.
//
// Only used for a reply-phase timeout, where the write is known to have
// already landed on the wire. A write-phase timeout is handled separately,
// by poisoning the rig instead -- see the write-timeout branch in request()
// for why the two cases aren't the same.
//
// r.waiting still points at replyCh until request()'s deferred cleanup runs
// (after this returns), so dispatch keeps routing a late reply here exactly
// as it would for any other in-flight request -- this just reads and drops
// it instead of answering a caller with it. If it arrives, the barrier lifts
// immediately: once the straggler is accounted for there is nothing left to
// be ambiguous about, so there's no reason to wait out the rest of the
// window.
func (r *Rig) waitOutQuietPeriod(replyCh chan benshi.Message) (closed bool) {
	timer := time.NewTimer(r.timeout * quietPeriodMultiplier)
	defer timer.Stop()
	select {
	case <-replyCh: // straggler arrived and is discarded; nothing left to wait for
	case <-timer.C:
	case <-r.closedCh:
		closed = true
	}
	return
}

// readLoop decodes frames from the radio, routing replies to a waiting request
// and folding notifications into the cache.
func (r *Rig) readLoop() {
	dec := benshi.NewDecoder()
	buf := make([]byte, 512)
	for {
		n, err := r.ch.Read(buf)
		if n > 0 {
			frames, derr := dec.Feed(buf[:n])
			if derr == nil {
				for _, f := range frames {
					r.dispatch(f)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *Rig) dispatch(f benshi.Frame) {
	m, err := benshi.DecodeMessage(f.Data)
	if err != nil {
		return
	}
	if m.Command == benshi.CmdEventNotification {
		// Event notifications are not replies to anything, so they must
		// never reach the reply matcher below. Nothing here consumes them:
		// the one that looked useful, the frequency-mode change (type 14),
		// reports the frequency-mode register rather than the channel record
		// the radio actually operates on, so caching it produced a
		// confidently wrong answer to "what frequency are we on".
		return
	}
	// At most one request is ever outstanding -- request() holds a quiet
	// period after its own timeout before letting the next one start (see
	// quietPeriodMultiplier) -- so matching purely by command type is safe:
	// whatever is currently waiting is the only thing this reply could
	// possibly be answering.
	r.mu.Lock()
	w, cmd := r.waiting, r.waitCmd
	r.mu.Unlock()
	if w != nil && m.Command == cmd {
		select {
		case w <- m:
		default:
		}
	}
}
