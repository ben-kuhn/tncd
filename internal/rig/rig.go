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
// reqMu, so no other request can start -- after its OWN timeout, before
// letting the next request begin. See waitOutQuietPeriod.
//
// The Benshi protocol carries no per-message correlation id, so once a
// request times out, a reply of that command type that shows up afterward
// cannot be attributed to the request it answers. Three earlier designs here
// tried to guess anyway -- matching by command type alone, then two
// FIFO-generation schemes -- and each guess was wrong in some real scenario:
// dropping the straggler loses a genuine answer when it was merely slow;
// delivering it to whatever is waiting next misattributes it when the
// straggler was for an earlier, abandoned request. The only way to make an
// unlabeled reply unambiguous is to guarantee at most one request is ever
// outstanding, so nothing else can possibly be waiting when a straggler
// shows up -- hence holding off the NEXT request instead of trying to match
// the stray reply.
//
// The trade-off is added latency on the request immediately after a
// timeout. That's acceptable here: timeouts are rare on a working link, and
// rig control (QSY) happens before connecting to a station, not mid-QSO, so
// a delayed poll costs nothing a user notices.
const quietPeriodMultiplier = 3

// Rig is a request/response session with one radio.
//
// One request is in flight at a time: the radio is a single serial endpoint
// and replies carry no correlation id, so concurrent requests could not be
// matched to their commands. After a request times out, the NEXT request is
// held for a quiet period rather than started immediately -- see
// quietPeriodMultiplier -- so a straggler reply for the abandoned request can
// never be mistaken for the answer to a new one.
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
// A radio sitting on a stored channel answers FREQ_MODE_GET_STATUS with a
// failure status rather than a frequency, since it is not in VFO mode. In that
// case fall back to reading the active channel directly instead of surfacing
// the error -- QSY only ever writes frequency mode, but readback must work
// whichever mode the radio happens to be in.
func (r *Rig) GetFreq() (uint32, error) {
	body, err := r.request(benshi.CmdFreqModeGetStatus, nil)
	if err != nil {
		return 0, err
	}
	hz, err := benshi.DecodeFreqModeStatus(body)
	if err == nil {
		return hz, nil
	}
	id, cerr := r.currChannel()
	if cerr != nil {
		return 0, err
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

// currChannel returns the radio's currently selected channel id from
// GET_HT_STATUS. curr_ch_id_lower is the high nibble of the second Status byte.
func (r *Rig) currChannel() (byte, error) {
	body, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		return 0, err
	}
	if len(body) < 3 || body[0] != 0 {
		return 0, benshi.ErrShortBody
	}
	return body[2] >> 4, nil
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

// Probe confirms the radio answers the protocol at all, so callers can fail
// with a clear message rather than a timeout on every later command.
func (r *Rig) Probe() error {
	body, err := r.request(benshi.CmdGetDevInfo, nil)
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
		// The write goroutine may still land on the wire after we give up on
		// it, so a reply is still plausible -- wait it out the same as a
		// reply-phase timeout below.
		if r.waitOutQuietPeriod(replyCh) {
			return nil, ErrClosed
		}
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
// start -- after this request's own timeout, giving a straggler reply room
// to show up and be harmlessly discarded before the next request is allowed
// to begin. See quietPeriodMultiplier for why this exists instead of trying
// to match the straggler to whichever request happens to be waiting later.
// It reports whether the rig was closed while waiting, so the caller can
// surface ErrClosed instead of ErrTimeout.
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
