// Package rig drives a Benshi-protocol radio over a control channel.
//
// Everything here runs OFF the engine goroutine. A Benshi command is a
// round-trip to the radio with a timeout, and the engine owns all L2 state, so
// blocking it would stall AX.25 on every port for the duration.
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

	cachedHz  uint32
	cachedOK  bool
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

// CachedFreq returns the last frequency pushed by the radio, if any.
func (r *Rig) CachedFreq() (uint32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cachedHz, r.cachedOK
}

// SetFreq enters frequency (VFO) mode and tunes rx = tx = hz.
//
// v1 is simplex only, which is what Winlink RMS gateways need.
func (r *Rig) SetFreq(hz uint32) error {
	p := benshi.FreqModeParams{
		RXFreqHz: hz,
		TXFreqHz: hz,
		RXMod:    benshi.ModFM,
		TXMod:    benshi.ModFM,
		Step:     benshi.DefaultStep,
	}
	_, err := r.request(benshi.CmdFreqModeSetPar, p.Payload())
	return err
}

// GetFreq reads the current frequency from the radio.
//
// A radio not in frequency (VFO) mode -- e.g. sitting on a stored channel --
// must fall back to reading the active channel directly instead of trusting
// FREQ_MODE_GET_STATUS's answer. "Not in frequency mode" shows up two ways:
// a non-zero (failure) status, which DecodeFreqModeStatus already turns into
// an error, OR a *successful* status with an all-zero frequency --
// HTCommander documents that leaving frequency mode clears the reported
// frequency to zero, so a clean 0 Hz reply is not a real answer (145.030 MHz
// on channel 0 is a valid station; a VFO genuinely tuned to 0 Hz is not) but
// the same "go read the channel instead" signal as a failure status. Treating
// only the error case as the trigger (the previous behavior) reported 0 Hz
// as if it were a real frequency whenever the radio was on a channel -- live
// UV-PRO captures show status=0 with an all-zero body in exactly that case.
func (r *Rig) GetFreq() (uint32, error) {
	body, err := r.request(benshi.CmdFreqModeGetStatus, nil)
	if err != nil {
		return 0, err
	}
	hz, ferr := benshi.DecodeFreqModeStatus(body)
	if ferr == nil && hz != 0 {
		return hz, nil
	}
	id, cerr := r.currChannel()
	if cerr != nil {
		if ferr != nil {
			return 0, ferr
		}
		return 0, cerr
	}
	return r.channelFreq(id)
}

// Teardown drops the radio out of frequency mode, restoring its channel state.
func (r *Rig) Teardown() error {
	_, err := r.request(benshi.CmdFreqModeSetPar, benshi.TeardownPayload())
	return err
}

// htStatusTXBit is is_in_tx within the first Status byte. Status packs, MSB
// first: is_power_on, is_in_tx, is_sq, is_in_rx, double_channel(2), is_scan,
// is_radio -- so is_in_tx is bit 6.
const htStatusTXBit = 0x40

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
		if st, err := benshi.DecodeFreqModeNotification(m.Body); err == nil {
			r.mu.Lock()
			if st.Active {
				r.cachedHz, r.cachedOK = st.RXFreqHz, true
			} else {
				r.cachedOK = false
			}
			r.mu.Unlock()
		}
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
